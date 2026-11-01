package com.phonefs.core.net

import com.phonefs.core.crypto.Identity
import com.phonefs.core.fs.DocIds
import com.phonefs.core.fs.FsBackend
import com.phonefs.core.fs.FileInfo
import com.phonefs.core.protocol.Attr
import com.phonefs.core.protocol.ErrBody
import com.phonefs.core.protocol.ErrCode
import com.phonefs.core.protocol.ErrorEnvelope
import com.phonefs.core.protocol.ListResp
import com.phonefs.core.protocol.PairDeviceInfo
import com.phonefs.core.protocol.PairInfo
import com.phonefs.core.protocol.PhoneFsException
import com.phonefs.core.protocol.RootsResp
import com.phonefs.core.protocol.StatFsResp
import io.ktor.http.ContentType
import io.ktor.http.HttpHeaders
import io.ktor.http.HttpStatusCode
import io.ktor.http.RequestConnectionPoint
import io.ktor.serialization.kotlinx.json.json
import io.ktor.server.application.Application
import io.ktor.server.application.ApplicationCall
import io.ktor.server.application.ApplicationCallPipeline
import io.ktor.server.application.call
import io.ktor.server.application.install
import io.ktor.server.engine.embeddedServer
import io.ktor.server.netty.Netty
import io.ktor.server.plugins.contentnegotiation.ContentNegotiation
import io.ktor.server.plugins.statuspages.StatusPages
import io.ktor.server.request.header
import io.ktor.server.request.path
import io.ktor.server.response.header
import io.ktor.server.response.respond
import io.ktor.server.response.respondBytes
import io.ktor.server.routing.get
import io.ktor.server.routing.routing
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import java.io.File
import java.io.FileInputStream
import java.net.InetAddress
import java.net.ServerSocket
import java.security.MessageDigest
import java.security.SecureRandom
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.util.Locale

/**
 * PhoneFS 源端 HTTP/1.1 server（Ktor 3.6.1 Netty 引擎，仅环回明文）。
 *
 * W2 真机闸门两次修订（详见 docs/research/2026-10-31-phonefs-m0-w2-*.md）：
 * - F-W2-1：Ktor CIO 3.6.1 JVM ApplicationEngine 显式拒绝 HTTPS，换 Netty 引擎。
 * - F-W2-3：Netty SslHandler/JdkEngine 在 Android 上包 Conscrypt 引擎做密文 record
 *   流水时 [anon:dalvik-LinearAlloc] 随流量无界增长（每 10MB 约 +150MB，百级分块
 *   RSS→9GB 被 SWAP 杀）；同 provider 的原生 SSLServerSocket 10GB 恒定 146MB。
 *   TLS 边界移到 Netty 之外（[LoopbackTlsFront]：SSLSocket 前置 → 环回明文），
 *   Ktor 只监听 127.0.0.1。M1 前需补 ADR 修订 SPEC §2.2 传输形态。
 */
class PhoneFsServer(
    private val backend: FsBackend,
    private val identity: Identity,
    private val deviceId: String,
    private val deviceName: String,
    private val deviceFormFactor: String,
    private val token: String,
) {
    private var engine: io.ktor.server.engine.EmbeddedServer<*, *>? = null
    private var tlsFront: LoopbackTlsFront? = null

    /** @return 实际 TLS 公网口；首选 18443，占用则 OS 动态端口。 */
    fun start(preferredPort: Int = 18443): Int {
        val port = choosePort(preferredPort)
        val loopbackPort = choosePort(0)
        runCatching {
            com.phonefs.core.telemetry.FsLog.i(
                "phonefs.net",
                "allocator=" + io.netty.buffer.ByteBufAllocator.DEFAULT.javaClass.name +
                    " directNoCleaner=" + io.netty.util.internal.PlatformDependent.useDirectBufferNoCleaner() +
                    " prop.allocator.type=" + System.getProperty("io.netty.allocator.type"),
            )
        }
        engine = embeddedServer(
            factory = Netty,
            configure = {
                // 唯一 HTTP 口只绑环回；外部入口只有 LoopbackTlsFront 的 TLS 口
                connectors.add(
                    io.ktor.server.engine.EngineConnectorBuilder().apply {
                        host = LOOPBACK_HOST
                        this.port = loopbackPort
                    },
                )
            },
            module = { configure() },
        ).start(wait = false)
        val front = LoopbackTlsFront(port, loopbackPort, identity)
        front.start()
        tlsFront = front
        return port
    }

    fun stop() {
        runCatching { tlsFront?.stop() }
        tlsFront = null
        engine?.stop(500, 1000)
        engine = null
    }

    private fun choosePort(preferred: Int): Int {
        if (preferred == 0) return ServerSocket(0).use { it.localPort }
        return try {
            ServerSocket(preferred).use { }
            preferred
        } catch (_: Exception) {
            ServerSocket(0).use { it.localPort }
        }
    }

    private fun Application.configure() {
        val json = Json {
            ignoreUnknownKeys = true
            explicitNulls = false
            encodeDefaults = true
        }
        install(ContentNegotiation) { json(json) }
        install(StatusPages) {
            exception<PhoneFsException> { call, e ->
                call.respondError(e.status, e.code, e.message ?: e.code, e.retriable, e.details)
            }
            exception<Throwable> { call, t ->
                if (t is kotlinx.coroutines.CancellationException) throw t
                com.phonefs.core.telemetry.FsLog.e("phonefs.server", "unhandled", t)
                call.respondError(
                    HttpStatusCode.InternalServerError,
                    ErrCode.INTERNAL,
                    t.javaClass.simpleName + ": " + (t.message ?: ""),
                    false,
                )
            }
            status(HttpStatusCode.NotFound) { call, _ ->
                call.respondError(HttpStatusCode.NotFound, ErrCode.NOT_FOUND, "unknown endpoint", false)
            }
        }
        intercept(ApplicationCallPipeline.Plugins) {
            val conn = call.request.local
            enforceSiteLocal(conn)
            val path = call.request.path()
            if (path != PATH_PAIR_INFO) {
                val presented = call.request.header(HttpHeaders.Authorization)
                    ?.takeIf { it.lowercase(Locale.US).startsWith("bearer ") }
                    ?.substring(7)
                    ?.trim()
                if (presented == null || !constTimeEq(presented, token)) {
                    throw PhoneFsException(
                        ErrCode.TOKEN_INVALID,
                        HttpStatusCode.Unauthorized,
                        "missing or invalid bearer token",
                    )
                }
            }
        }
        routing {
            get(PATH_PAIR_INFO) {
                call.respond(
                    PairInfo(
                        service = "phonefs",
                        protocolVersions = listOf(1),
                        fingerprint = identity.fingerprint,
                        pairingOpen = false, // W2 固定演示配对，配对窗未开放
                        serverTimeMs = System.currentTimeMillis(),
                        device = PairDeviceInfo(deviceId, deviceName, "android", deviceFormFactor),
                    ),
                )
            }

            get("/v1/roots") {
                call.respond(RootsResp(FIXED_ROOTS))
            }

            get("/v1/fs/list") {
                val id = call.request.queryParameters["id"]?.takeIf { it.isNotEmpty() } ?: DocIds.ROOT
                val limit = call.request.queryParameters["limit"]?.let {
                    it.toIntOrNull()?.takeIf { n -> n in 1..5000 }
                        ?: throw badRequest("limit must be 1..5000")
                } ?: 1000
                if (!call.request.queryParameters["cursor"].isNullOrEmpty()) {
                    // W2 不支持翻页：cursor 对任意 id 均视为错配
                    throw PhoneFsException(ErrCode.BAD_CURSOR, HttpStatusCode.BadRequest, "cursor not supported in W2")
                }
                val infos: List<FileInfo> = when {
                    id == DocIds.ROOT -> {
                        call.respond(ListResp(FIXED_ROOTS.take(limit), nextCursor = "", revision = 0L))
                        return@get
                    }
                    id.startsWith(DocIds.CAT_PREFIX) ->
                        throw PhoneFsException(
                            ErrCode.NOT_SUPPORTED,
                            HttpStatusCode.NotImplemented,
                            "category index backend is not implemented in W2 (M1)",
                        )
                    else -> {
                        // 真实树需要 MES
                        if (!backend.ready()) throw notReady()
                        when {
                            id == DocIds.STOR_PRIMARY -> backend.list(backend.storageRoot.absolutePath)
                            id.startsWith(DocIds.P_PREFIX) -> {
                                val path = DocIds.decodePath(id)
                                    ?: throw badRequest("malformed p: id")
                                if (!backend.isInsideStorage(path)) throw badRequest("id escapes storage root")
                                val f = File(path)
                                if (!f.exists()) throw notFound()
                                if (!f.isDirectory) throw badRequest("not a directory")
                                backend.list(path)
                            }
                            else -> throw notFound()
                        }
                    }
                }
                val sliced = infos.take(limit)
                val attrs = sliced.map { it.toAttr(backend.storageRoot.absolutePath) }
                call.respond(
                    ListResp(
                        entries = attrs,
                        nextCursor = "",
                        revision = (attrs.maxOfOrNull { it.modifiedMs } ?: 0L),
                    ),
                )
            }

            get("/v1/fs/statfs") {
                val c = backend.statFs()
                call.respond(StatFsResp(c.totalBytes, c.freeBytes, c.blockSize))
            }

            get("/v1/fs/content") {
                if (!backend.ready()) throw notReady()
                val id = call.request.queryParameters["id"]
                    ?: throw badRequest("missing id")
                val path = when {
                    id == DocIds.STOR_PRIMARY -> backend.storageRoot.absolutePath
                    id.startsWith(DocIds.P_PREFIX) ->
                        DocIds.decodePath(id)?.takeIf { backend.isInsideStorage(it) }
                            ?: throw badRequest("malformed or escaping id")
                    else -> throw notFound()
                }
                val info = backend.stat(path) ?: throw notFound()
                if (info.isDir) throw badRequest("cannot stream a directory")

                val size = info.size
                val etag = "\"$size-${info.modifiedMs}\""
                val mime = info.mime?.let { runCatching { ContentType.parse(it) }.getOrNull() }
                    ?: ContentType.Application.OctetStream

                val ifRange = call.request.header(HttpHeaders.IfRange)?.trim()
                val rangeHeader = call.request.header(HttpHeaders.Range)
                val range = if (rangeHeader != null && (ifRange == null || ifRange == etag)) {
                    parseRange(rangeHeader, size)
                } else {
                    RangeSpec.Full
                }

                call.response.header(HttpHeaders.AcceptRanges, "bytes")
                call.response.header(HttpHeaders.ETag, etag)
                call.response.header(HttpHeaders.LastModified, httpDate(info.modifiedMs))
                call.request.queryParameters["download"]?.let {
                    val fn = encodeDisposition(info.name)
                    call.response.header(HttpHeaders.ContentDisposition, "attachment; filename*=UTF-8''$fn")
                }

                when (range) {
                    is RangeSpec.Unsatisfiable -> {
                        call.response.header(HttpHeaders.ContentRange, "bytes */$size")
                        call.respondError(
                            HttpStatusCode.RequestedRangeNotSatisfiable,
                            ErrCode.RANGE_NOT_SATISFIABLE,
                            "range outside 0..${size - 1}",
                            false,
                            mapOf("size" to size.toString()),
                        )
                    }
                    RangeSpec.Full -> {
                        // W2：一次性字节响应（单块 ≤10MB），规避 Ktor/Netty WriteChannelContent
                        // 在长连接背压下每分块写分配累积（实测 10MB 拉流泄漏 ~0.7GB RSS）。
                        val bytes = readRange(path, 0, size)
                            ?: throw PhoneFsException(ErrCode.NOT_FOUND, HttpStatusCode.NotFound, "file vanished")
                        call.respondBytes(bytes, mime, HttpStatusCode.OK)
                    }
                    is RangeSpec.Partial -> {
                        val len = range.last - range.first + 1
                        call.response.header(
                            HttpHeaders.ContentRange,
                            "bytes ${range.first}-${range.last}/$size",
                        )
                        val bytes = readRange(path, range.first, len)
                            ?: throw PhoneFsException(ErrCode.NOT_FOUND, HttpStatusCode.NotFound, "file vanished")
                        call.respondBytes(bytes, mime, HttpStatusCode.PartialContent)
                    }
                }
            }
        }
    }

    private suspend fun readRange(path: String, offset: Long, len: Long): ByteArray? =
        withContext(Dispatchers.IO) {
            val fis = backend.openInput(path, offset) ?: return@withContext null
            fis.use { input: FileInputStream ->
                val out = ByteArray(len.toInt())
                var read = 0
                while (read < out.size) {
                    val n = input.read(out, read, out.size - read)
                    if (n <= 0) break
                    read += n
                }
                if (read == out.size) out else out.copyOf(read)
            }
        }

    private fun enforceSiteLocal(conn: RequestConnectionPoint) {
        val addr = runCatching { InetAddress.getByName(conn.remoteHost) }.getOrNull()
        if (addr == null || !SiteLocal.isAllowed(addr)) {
            throw PhoneFsException(
                ErrCode.BIND_REFUSED,
                HttpStatusCode.Forbidden,
                "peer address is not site-local/loopback",
                true,
            )
        }
    }

    private fun badRequest(msg: String) =
        PhoneFsException(ErrCode.INVALID_ARGUMENT, HttpStatusCode.BadRequest, msg)

    private fun notFound() =
        PhoneFsException(ErrCode.NOT_FOUND, HttpStatusCode.NotFound, "not found")

    private fun notReady() = PhoneFsException(
        ErrCode.NOT_READY,
        HttpStatusCode.ServiceUnavailable,
        "all-files-access (MANAGE_EXTERNAL_STORAGE) not granted",
        true,
        mapOf("hint" to "grant MES in system settings"),
    )

    private fun constTimeEq(a: String, b: String): Boolean {
        val x = a.toByteArray(); val y = b.toByteArray()
        return MessageDigest.isEqual(x, y)
    }

    private suspend fun ApplicationCall.respondError(
        status: HttpStatusCode,
        code: String,
        message: String,
        retriable: Boolean,
        details: Map<String, String>? = null,
    ) {
        val trace = request.header("X-Trace-Id")
            ?: ByteArray(16).also { SecureRandom().nextBytes(it) }.joinToString("") { "%02x".format(it) }
        response.header("X-Trace-Id", trace)
        respond(status, ErrorEnvelope(ErrBody(code, message, trace, retriable, details)))
    }

    private fun FileInfo.toAttr(rootPath: String): Attr {
        val parent = File(path).parentFile
        val parentId = when {
            parent == null -> DocIds.ROOT
            parent.absolutePath == rootPath -> DocIds.STOR_PRIMARY
            else -> DocIds.encodePath(parent.absolutePath)
        }
        return Attr(
            id = DocIds.encodePath(path),
            parent = parentId,
            name = name,
            isDir = isDir,
            size = size,
            modifiedMs = modifiedMs,
            mime = mime,
            mediaType = null,
            writable = writable,
        )
    }

    private sealed interface RangeSpec {
        data object Full : RangeSpec
        data object Unsatisfiable : RangeSpec
        data class Partial(val first: Long, val last: Long) : RangeSpec
    }

    private fun parseRange(header: String, size: Long): RangeSpec {
        if (!header.lowercase(Locale.US).startsWith("bytes=")) return RangeSpec.Full
        val spec = header.substring(6).trim()
        if (spec.contains(',')) return RangeSpec.Full // W2 不支持 multipart，退回全量
        val dash = spec.indexOf('-')
        if (dash < 0) return RangeSpec.Full
        val a = spec.substring(0, dash).trim()
        val b = spec.substring(dash + 1).trim()
        return try {
            if (a.isEmpty()) {
                if (b.isEmpty()) return RangeSpec.Full
                val suffix = b.toLong()
                if (suffix <= 0) return RangeSpec.Unsatisfiable
                val first = (size - suffix).coerceAtLeast(0)
                RangeSpec.Partial(first, size - 1)
            } else {
                val first = a.toLong()
                if (first < 0) return RangeSpec.Full
                if (size == 0L || first >= size) return RangeSpec.Unsatisfiable
                val last = if (b.isEmpty()) size - 1 else b.toLong().coerceAtMost(size - 1)
                if (last < first) return RangeSpec.Unsatisfiable
                RangeSpec.Partial(first, last)
            }
        } catch (_: NumberFormatException) {
            RangeSpec.Full
        }
    }

    private fun httpDate(ms: Long): String =
        DateTimeFormatter.RFC_1123_DATE_TIME
            .withLocale(Locale.US)
            .format(java.time.Instant.ofEpochMilli(ms).atZone(ZoneOffset.UTC))

    private fun encodeDisposition(name: String): String =
        java.net.URLEncoder.encode(name, "UTF-8").replace("+", "%20")

    companion object {
        private const val LOOPBACK_HOST = "127.0.0.1"
        private const val PATH_PAIR_INFO = "/v1/pair/info"

        // §6.5.1 固定 8 行（7 cat + stor），cat:camera 不入（P2-7）
        val FIXED_ROOTS: List<Attr> = listOf(
            cat("cat:image", "图片"),
            cat("cat:video", "视频"),
            cat("cat:audio", "音频"),
            cat("cat:document", "文档"),
            cat("cat:archive", "压缩包"),
            cat("cat:downloads", "下载"),
            cat("cat:screenshots", "截图"),
            Attr(
                id = DocIds.STOR_PRIMARY,
                parent = DocIds.ROOT,
                name = "内部存储",
                isDir = true,
                size = 0,
                modifiedMs = 0,
                writable = true,
            ),
        )

        private fun cat(id: String, name: String) = Attr(
            id = id,
            parent = DocIds.ROOT,
            name = name,
            isDir = true,
            size = 0,
            modifiedMs = 0,
            writable = false,
        )
    }
}
