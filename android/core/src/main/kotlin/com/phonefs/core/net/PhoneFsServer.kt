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
import io.ktor.http.content.OutgoingContent
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
import io.ktor.server.routing.get
import io.ktor.server.routing.routing
import io.ktor.utils.io.ByteWriteChannel
import io.ktor.utils.io.writeFully
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import org.bouncycastle.openssl.PEMParser
import org.bouncycastle.openssl.jcajce.JcaPEMKeyConverter
import org.bouncycastle.util.io.pem.PemObject
import java.io.File
import java.io.FileInputStream
import java.io.StringReader
import java.net.InetAddress
import java.net.ServerSocket
import java.security.KeyStore
import java.security.MessageDigest
import java.security.SecureRandom
import java.security.cert.CertificateFactory
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.util.Locale

/**
 * PhoneFS 源端 HTTPS/1.1 server（Ktor 3.6.1）。
 *
 * W2 闸门发现 F-W2-1：Ktor CIO 3.6.1 的 JVM ApplicationEngine 显式拒绝 HTTPS
 * （CIOApplicationEngine.initServerJob："CIO Engine does not currently support HTTPS"），
 * 与 SPEC §2.2「Ktor CIO + TLS」钉点冲突；W2 改用同一 Ktor 版本的 Netty 引擎
 * （协程模型/content-negotiation/SSE 插件生态全部不变，仅传输引擎不同），
 * Netty JDK SSL 走 provider 顺序首位的 Conscrypt，enabledProtocols 钉死 TLSv1.3。
 * 需 M1 前补 ADR 修订 SPEC §2.2。
 */
class PhoneFsServer(
    private val backend: FsBackend,
    private val identity: Identity,
    private val deviceId: String,
    private val deviceName: String,
    private val token: String,
) {
    private var engine: io.ktor.server.engine.EmbeddedServer<*, *>? = null

    /** @return 实际监听端口；首选 18443，占用则 OS 动态端口。 */
    fun start(preferredPort: Int = 18443): Int {
        val port = choosePort(preferredPort)
        val keyStore = buildKeyStore()
        // Ktor 3.6.1：sslConnector 仅作为 ApplicationEngine.Configuration 扩展存在；
        // 显式只挂 HTTPS connector，避免 embeddedServer(host,port) 默认再开明文口。
        val https = io.ktor.server.engine.EngineSSLConnectorBuilder(
            keyStore,
            KEY_ALIAS,
            { KS_PWD },
            { KS_PWD },
        ).apply {
            host = BIND_HOST
            this.port = port
            // §7.3 MinVersion TLS1.3：拒绝任何降级（Netty 逐 engine 应用 enabledProtocols）
            enabledProtocols = listOf("TLSv1.3")
        }
        engine = embeddedServer(
            factory = Netty,
            configure = { connectors.add(https) },
            module = { configure() },
        ).start(wait = false)
        return port
    }

    fun stop() {
        engine?.stop(500, 1000)
        engine = null
    }

    private fun choosePort(preferred: Int): Int {
        return try {
            ServerSocket(preferred).use { }
            preferred
        } catch (_: Exception) {
            ServerSocket(0).use { it.localPort }
        }
    }

    private fun buildKeyStore(): KeyStore {
        val cf = CertificateFactory.getInstance("X.509")
        val cert = cf.generateCertificate(identity.certPem.byteInputStream())
        val key = PEMParser(StringReader(identity.keyPem)).use { parser ->
            val obj = parser.readObject() as PemObject
            val keyInfo = org.bouncycastle.asn1.pkcs.PrivateKeyInfo.getInstance(obj.content)
            JcaPEMKeyConverter().getPrivateKey(keyInfo)
        }
        return KeyStore.getInstance("PKCS12").apply {
            load(null, null)
            setKeyEntry(KEY_ALIAS, key, KS_PWD, arrayOf(cert))
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
                        device = PairDeviceInfo(deviceId, deviceName, "android"),
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
                        call.respond(
                            object : OutgoingContent.WriteChannelContent() {
                                override val contentType = mime
                                override val contentLength: Long? = size
                                override val status = HttpStatusCode.OK
                                override suspend fun writeTo(channel: ByteWriteChannel) =
                                    streamRange(channel, path, 0, size)
                            },
                        )
                    }
                    is RangeSpec.Partial -> {
                        val len = range.last - range.first + 1
                        call.response.header(
                            HttpHeaders.ContentRange,
                            "bytes ${range.first}-${range.last}/$size",
                        )
                        call.respond(
                            object : OutgoingContent.WriteChannelContent() {
                                override val contentType = mime
                                override val contentLength: Long? = len
                                override val status = HttpStatusCode.PartialContent
                                override suspend fun writeTo(channel: ByteWriteChannel) =
                                    streamRange(channel, path, range.first, len)
                            },
                        )
                    }
                }
            }
        }
    }

    private suspend fun streamRange(channel: ByteWriteChannel, path: String, offset: Long, len: Long) {
        withContext(Dispatchers.IO) {
            val fis = backend.openInput(path, offset)
                ?: throw PhoneFsException(ErrCode.NOT_FOUND, HttpStatusCode.NotFound, "file vanished")
            fis.use { input: FileInputStream ->
                val buf = ByteArray(128 * 1024)
                var remaining = len
                while (remaining > 0) {
                    val want = minOf(buf.size.toLong(), remaining).toInt()
                    val n = input.read(buf, 0, want)
                    if (n <= 0) break
                    channel.writeFully(buf, 0, n)
                    remaining -= n
                }
            }
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
        private const val BIND_HOST = "0.0.0.0"
        private const val KEY_ALIAS = "phonefs"
        private val KS_PWD = "phonefs-w2".toCharArray()
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
