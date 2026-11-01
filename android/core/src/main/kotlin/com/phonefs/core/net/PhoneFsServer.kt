package com.phonefs.core.net

import com.phonefs.core.crypto.Identity
import com.phonefs.core.fs.DocIds
import com.phonefs.core.fs.FsBackend
import com.phonefs.core.fs.FileInfo
import com.phonefs.core.protocol.Attr
import com.phonefs.core.protocol.CreateReq
import com.phonefs.core.protocol.ErrBody
import com.phonefs.core.protocol.ErrCode
import com.phonefs.core.protocol.ErrorEnvelope
import com.phonefs.core.protocol.ListResp
import com.phonefs.core.protocol.MetaReq
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
import io.ktor.server.request.receive
import io.ktor.server.response.header
import io.ktor.server.response.respond
import io.ktor.server.response.respondBytes
import io.ktor.server.routing.delete
import io.ktor.server.routing.get
import io.ktor.server.routing.patch
import io.ktor.server.routing.post
import io.ktor.server.routing.put
import io.ktor.server.routing.routing
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonPrimitive
import java.io.File
import java.io.FileInputStream
import java.io.IOException
import java.net.InetAddress
import java.net.ServerSocket
import java.nio.file.FileVisitResult
import java.nio.file.Files
import java.nio.file.Path
import java.nio.file.SimpleFileVisitor
import java.nio.file.StandardCopyOption
import java.nio.file.attribute.BasicFileAttributes
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
    private val uploadStore = UploadStore(backend)

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
        uploadStore.close()
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

            // §6.5.6：POST create；tmp 仅提示客户端意图，实际临时文件由 PUT 原子写管理。
            post("/v1/fs/create") {
                requireReady()
                val req = call.receive<CreateReq>()
                validateName(req.name)
                val parent = realPath(req.parent, allowStorageRoot = true)
                if (!parent.isDirectory) throw notFound()
                val child = File(parent, req.name)
                if (!backend.isInsideStorage(child.absolutePath)) throw badRequest("child escapes storage root")
                if (child.exists()) throw alreadyExists()
                val created = if (req.isDir) child.mkdir() else child.createNewFile()
                if (!created) throw alreadyExists()
                if (!req.isDir) backend.scanFile(child.absolutePath)
                val info = backend.stat(child.absolutePath)
                    ?: throw IOException("created entry vanished")
                call.respond(HttpStatusCode.Created, info.toAttr(backend.storageRoot.absolutePath))
            }

            // §6.5.7：整写/分块共用流式上传存储；目标须由 create 先行创建。
            put("/v1/fs/content") {
                requireReady()
                val id = call.request.queryParameters["id"] ?: throw badRequest("missing id")
                val target = realPath(id, allowStorageRoot = false)
                if (!target.isFile) throw notFound()
                val contentLength = call.request.header(HttpHeaders.ContentLength)?.toLongOrNull()
                    ?: throw badRequest("Content-Length required")
                if (contentLength < 0) throw badRequest("negative Content-Length")
                val mtime = parseSetMtime(call.request.header("X-Set-Mtime"))
                val rangeHeader = call.request.header(HttpHeaders.ContentRange)
                val uploadId = call.request.header("X-Upload-Id")
                val result = if (rangeHeader == null && uploadId == null) {
                    uploadStore.full(call, target, contentLength, mtime)
                } else {
                    if (rangeHeader == null || uploadId.isNullOrBlank()) {
                        throw badRequest("Content-Range and X-Upload-Id must be sent together")
                    }
                    val (start, end, total) = parseContentRange(rangeHeader)
                    if (contentLength != end - start + 1) {
                        throw badRequest("Content-Length does not match Content-Range")
                    }
                    uploadStore.chunk(call, target, uploadId, start, end, total, mtime)
                }
                call.respond(result)
            }

            // §6.5.8 + FR-19：M2 仅文件 rename/move/mtime；目录一律 501。
            patch("/v1/fs/meta") {
                requireReady()
                val req = call.receive<MetaReq>()
                if (req.name == null && req.parentId == null && req.modifiedMs == null) {
                    throw badRequest("at least one meta field is required")
                }
                if (req.modifiedMs != null && req.modifiedMs < 0) throw badRequest("negative modified_ms")
                val source = realPath(req.id, allowStorageRoot = false)
                if (source.isDirectory) {
                    throw PhoneFsException(
                        ErrCode.NOT_SUPPORTED, HttpStatusCode.NotImplemented,
                        "directory rename/move/meta is deferred to M4",
                    )
                }
                if (!source.isFile) throw notFound()
                req.name?.let(::validateName)
                val parent = req.parentId?.let { realPath(it, allowStorageRoot = true) } ?: source.parentFile
                if (parent == null || !parent.isDirectory) throw notFound()
                val target = File(parent, req.name ?: source.name)
                if (!backend.isInsideStorage(target.absolutePath)) throw badRequest("target escapes storage root")
                val moved = target.absolutePath != source.absolutePath
                if (moved) {
                    if (target.exists()) throw alreadyExists()
                    try {
                        Files.move(source.toPath(), target.toPath(), StandardCopyOption.ATOMIC_MOVE)
                    } catch (_: java.nio.file.AtomicMoveNotSupportedException) {
                        Files.move(source.toPath(), target.toPath())
                    }
                }
                req.modifiedMs?.let {
                    if (!target.setLastModified(it)) throw IOException("setLastModified failed")
                }
                if (moved) {
                    backend.scanFile(source.absolutePath)
                    backend.scanFile(target.absolutePath)
                } else if (req.modifiedMs != null) {
                    backend.scanFile(target.absolutePath)
                }
                val info = backend.stat(target.absolutePath) ?: throw IOException("patched entry vanished")
                call.respond(info.toAttr(backend.storageRoot.absolutePath))
            }

            // §6.5.9：不存在幂等 204；递归删除不跟随符号链接，部分失败显式 409 PARTIAL。
            delete("/v1/fs") {
                requireReady()
                val id = call.request.queryParameters["id"] ?: throw badRequest("missing id")
                val recursive = when (call.request.queryParameters["recursive"] ?: "false") {
                    "true" -> true
                    "false" -> false
                    else -> throw badRequest("recursive must be true/false")
                }
                val target = realPath(id, allowStorageRoot = false)
                if (!target.exists()) {
                    call.respond(HttpStatusCode.NoContent)
                    return@delete
                }
                if (target.isDirectory && !recursive && target.listFiles()?.isNotEmpty() == true) {
                    throw PhoneFsException(ErrCode.NOT_EMPTY, HttpStatusCode.Conflict, "directory is not empty")
                }
                if (target.isDirectory && recursive) {
                    val (deleted, failed) = deleteTree(target)
                    if (failed > 0) {
                        throw PhoneFsException(
                            ErrCode.PARTIAL, HttpStatusCode.Conflict, "recursive delete partially failed",
                            false, mapOf("deleted" to deleted.toString(), "failed" to failed.toString()),
                        )
                    }
                } else {
                    try {
                        Files.deleteIfExists(target.toPath())
                    } catch (t: IOException) {
                        throw PhoneFsException(
                            ErrCode.PARTIAL, HttpStatusCode.Conflict, "delete failed: ${t.javaClass.simpleName}",
                            false, mapOf("deleted" to "0", "failed" to "1"),
                        )
                    }
                    backend.scanFile(target.absolutePath)
                }
                call.respond(HttpStatusCode.NoContent)
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

    private fun requireReady() {
        if (!backend.ready()) throw notReady()
    }

    /** id 解码 + canonical 根边界；写操作禁止把 stor:primary 本身当目标。 */
    private fun realPath(id: String, allowStorageRoot: Boolean): File {
        if (id == DocIds.STOR_PRIMARY) {
            if (allowStorageRoot) return backend.storageRoot
            throw badRequest("storage root itself is not a valid write target")
        }
        if (!id.startsWith(DocIds.P_PREFIX)) throw notFound()
        val path = DocIds.decodePath(id)?.takeIf { backend.isInsideStorage(it) }
            ?: throw badRequest("malformed or escaping id")
        return File(path)
    }

    private fun validateName(name: String) {
        val invalid = name.isBlank() || name == "." || name == ".." ||
            name.length > 255 || name.any { it == '/' || it == '\\' || it.code < 0x20 }
        if (invalid) {
            throw PhoneFsException(
                ErrCode.INVALID_NAME, HttpStatusCode.BadRequest, "invalid file name",
                false, mapOf("reason" to "blank, dot, control, separator, or overlong name"),
            )
        }
    }

    private fun parseSetMtime(value: String?): Long? {
        if (value == null) return null
        return value.toLongOrNull()?.takeIf { it >= 0 }
            ?: throw badRequest("X-Set-Mtime must be unix milliseconds")
    }

    private fun parseContentRange(value: String): Triple<Long, Long, Long> {
        val match = CONTENT_RANGE_RE.matchEntire(value.trim()) ?: throw badRequest("invalid Content-Range")
        val start = match.groupValues[1].toLongOrNull() ?: throw badRequest("invalid range start")
        val end = match.groupValues[2].toLongOrNull() ?: throw badRequest("invalid range end")
        val total = match.groupValues[3].toLongOrNull() ?: throw badRequest("invalid range total")
        if (total <= 0 || start < 0 || end < start || end >= total) throw badRequest("invalid Content-Range bounds")
        return Triple(start, end, total)
    }

    private fun alreadyExists() = PhoneFsException(
        ErrCode.ALREADY_EXISTS, HttpStatusCode.Conflict, "target already exists",
    )

    private fun deleteTree(root: File): Pair<Int, Int> {
        var deleted = 0
        var failed = 0
        fun remove(path: Path, scan: Boolean) {
            try {
                Files.deleteIfExists(path)
                deleted++
                if (scan) backend.scanFile(path.toString())
            } catch (_: IOException) {
                failed++
            }
        }
        Files.walkFileTree(root.toPath(), object : SimpleFileVisitor<Path>() {
            override fun visitFile(file: Path, attrs: BasicFileAttributes): FileVisitResult {
                remove(file, scan = true)
                return FileVisitResult.CONTINUE
            }

            override fun visitFileFailed(file: Path, exc: IOException): FileVisitResult {
                failed++
                return FileVisitResult.CONTINUE
            }

            override fun postVisitDirectory(dir: Path, exc: IOException?): FileVisitResult {
                if (exc != null) failed++ else remove(dir, scan = false)
                return FileVisitResult.CONTINUE
            }
        })
        return deleted to failed
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
        details: Map<String, Any>? = null,
    ) {
        val trace = request.header("X-Trace-Id")
            ?: ByteArray(16).also { SecureRandom().nextBytes(it) }.joinToString("") { "%02x".format(it) }
        response.header("X-Trace-Id", trace)
        val jsonDetails = details?.mapValues { (_, value) ->
            when (value) {
                is Number -> JsonPrimitive(value)
                is Boolean -> JsonPrimitive(value)
                else -> JsonPrimitive(value.toString())
            }
        }
        respond(status, ErrorEnvelope(ErrBody(code, message, trace, retriable, jsonDetails)))
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
        private val CONTENT_RANGE_RE = Regex("""bytes (\d+)-(\d+)/(\d+)""")

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
