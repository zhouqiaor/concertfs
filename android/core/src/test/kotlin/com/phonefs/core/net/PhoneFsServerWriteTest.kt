package com.phonefs.core.net

import com.phonefs.core.crypto.Identity
import com.phonefs.core.fs.CapacityInfo
import com.phonefs.core.fs.DocIds
import com.phonefs.core.fs.FileInfo
import com.phonefs.core.fs.FsBackend
import com.phonefs.core.protocol.Attr
import com.phonefs.core.protocol.CreateReq
import com.phonefs.core.protocol.ErrCode
import com.phonefs.core.protocol.ErrorEnvelope
import com.phonefs.core.protocol.ListResp
import com.phonefs.core.protocol.MetaReq
import com.phonefs.core.protocol.StatFsResp
import com.phonefs.core.protocol.WriteResp
import io.ktor.http.HttpMethod
import io.ktor.http.encodeURLParameter
import io.ktor.server.cio.CIO
import io.ktor.server.engine.embeddedServer
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.long
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File
import java.io.FileInputStream
import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.nio.file.Files

class PhoneFsServerWriteTest {

    @Test
    fun legacyTwentyCaseProtocolRemainsCompatible() = withServer { h ->
        val isolated = h.file("PhoneFS-m2-protocol")
        h.delete(isolated, recursive = true, expected = 204)

        // 1..4：create 根/重名/非法名/缺父目录。
        h.create(DocIds.STOR_PRIMARY, isolated.name, isDir = true)
        val rootId = h.id(isolated)
        h.create(rootId, "dup", isDir = true)
        assertEquals(ErrCode.ALREADY_EXISTS, h.error(h.create(rootId, "dup", true, 409)))
        assertEquals(ErrCode.INVALID_NAME, h.error(h.create(rootId, "bad/name", false, 400)))
        assertEquals(
            ErrCode.NOT_FOUND,
            h.error(h.create(h.id(h.file("missing-parent")), "x", false, 404)),
        )

        // 5..8：整写、短文件原子替换、空写与 mtime。
        val full = h.file("PhoneFS-m2-protocol/full.bin")
        h.create(rootId, full.name, false)
        val fullData = ByteArray(1024 * 1024 + 123) { i -> (i * 37 + 11).toByte() }
        val fullMtime = 1_700_000_000_123L
        val fullResult = h.write(h.putFull(full, fullData, fullMtime))
        assertTrue(fullResult.complete)
        assertEquals(fullData.size.toLong(), fullResult.received)
        assertArrayEquals(fullData, h.get(full).bytes)
        val listing = h.json<ListResp>(h.request(HttpMethod.Get, h.endpoint("/v1/fs/list", rootId)))
        val fullAttr = listing.entries.single { it.id == h.id(full) }
        assertEquals(fullMtime / 1000, fullAttr.modifiedMs / 1000)
        val replacement = ByteArray(123457) { i -> (i * 19 + 5).toByte() }
        assertTrue(h.write(h.putFull(full, replacement, 1_700_000_000_456L)).complete)
        assertArrayEquals(replacement, h.get(full).bytes)
        val empty = h.file("PhoneFS-m2-protocol/empty.bin")
        h.create(rootId, empty.name, false)
        assertEquals(0L, h.write(h.putFull(empty, ByteArray(0))).received)
        assertEquals(0, h.get(empty).bytes.size)

        // 9：旧分块乱序、覆盖重传、区间判全与完成态幂等。
        val chunks = listOf(
            ByteArray(65536) { i -> (i * 3 + 1).toByte() },
            ByteArray(70000) { i -> (i * 5 + 2).toByte() },
            ByteArray(33333) { i -> (i * 7 + 3).toByte() },
        )
        val chunkData = chunks.reduce { acc, bytes -> acc + bytes }
        val chunked = h.file("PhoneFS-m2-protocol/chunk.bin")
        h.create(rootId, chunked.name, false)
        val uploadId = "m2-out-of-order-jvm"
        val middle = chunks[0].size.toLong()
        val last = (chunks[0].size + chunks[1].size).toLong()
        val incompleteLast = h.putChunk(chunked, uploadId, last, chunks[2], chunkData.size.toLong(), expected = 409)
        assertEquals(ErrCode.UPLOAD_INCOMPLETE, h.error(incompleteLast))
        assertEquals(0L, h.errorEnvelope(incompleteLast).error.details?.get("received")?.jsonPrimitive?.long)
        assertFalse(h.write(h.putChunk(chunked, uploadId, 0, chunks[0], chunkData.size.toLong())).complete)
        assertFalse(h.write(h.putChunk(chunked, uploadId, 0, chunks[0], chunkData.size.toLong())).complete)
        val completed = h.write(h.putChunk(chunked, uploadId, middle, chunks[1], chunkData.size.toLong()))
        assertTrue(completed.complete)
        assertEquals(chunkData.size.toLong(), completed.received)
        assertArrayEquals(chunkData, h.get(chunked).bytes)
        val retransmitted = h.write(h.putChunk(chunked, uploadId, middle, chunks[1], chunkData.size.toLong()))
        assertTrue(retransmitted.complete)
        assertArrayEquals(chunkData, h.get(chunked).bytes)

        // 10..11：末块缺前缀 409（Number details）与空间前置校验 507。
        val incomplete = h.file("PhoneFS-m2-protocol/incomplete.bin")
        h.create(rootId, incomplete.name, false)
        val incompleteReply = h.putChunk(incomplete, "m2-incomplete-jvm", 900, ByteArray(100), 1000, expected = 409)
        assertEquals(ErrCode.UPLOAD_INCOMPLETE, h.error(incompleteReply))
        val received = h.errorEnvelope(incompleteReply).error.details?.get("received")?.jsonPrimitive
        assertNotNull(received)
        assertFalse(received!!.isString)
        assertEquals(0L, received.long)
        val quota = h.file("PhoneFS-m2-protocol/quota.bin")
        h.create(rootId, quota.name, false)
        val statFs = h.json<StatFsResp>(h.request(HttpMethod.Get, "/v1/fs/statfs"))
        val quotaReply = h.putChunk(
            quota, "m2-quota-jvm", 0, ByteArray(100), statFs.freeBytes + 1024L * 1024L * 1024L,
            expected = 507,
        )
        assertEquals(ErrCode.INSUFFICIENT_STORAGE, h.error(quotaReply))

        // 12..16：rename/move/目录 501/冲突 409/mtime。
        val renamed = h.file("PhoneFS-m2-protocol/renamed.bin")
        assertEquals(renamed.name, h.json<Attr>(h.patch(full, name = renamed.name)).name)
        assertEquals(404, h.get(full, 404).status)
        assertArrayEquals(replacement, h.get(renamed).bytes)
        val movedDir = h.file("PhoneFS-m2-protocol/moved")
        h.create(rootId, movedDir.name, true)
        val moved = h.file("PhoneFS-m2-protocol/moved/renamed.bin")
        assertEquals(h.id(movedDir), h.json<Attr>(h.patch(renamed, parentId = h.id(movedDir))).parent)
        assertArrayEquals(replacement, h.get(moved).bytes)
        assertEquals(ErrCode.NOT_SUPPORTED, h.error(h.patch(movedDir, name = "moved2", expected = 501)))
        val conflictA = h.file("PhoneFS-m2-protocol/conflict-a")
        val conflictB = h.file("PhoneFS-m2-protocol/conflict-b")
        h.create(rootId, conflictA.name, false)
        h.create(rootId, conflictB.name, false)
        assertEquals(ErrCode.ALREADY_EXISTS, h.error(h.patch(conflictA, name = conflictB.name, expected = 409)))
        val metaMtime = 1_700_000_000_999L
        val metaAttr = h.json<Attr>(h.patch(moved, modifiedMs = metaMtime))
        assertEquals(metaMtime / 1000, metaAttr.modifiedMs / 1000)

        // 17..20：删除边界与清理。
        assertEquals(ErrCode.NOT_EMPTY, h.error(h.delete(isolated, recursive = false, expected = 409)))
        h.delete(h.file("PhoneFS-m2-protocol/not-there"), recursive = false, expected = 204)
        h.delete(isolated, recursive = true, expected = 204)
        h.get(isolated, expected = 404)
    }

    @Test
    fun deltaBaseMismatchAndMissingTargetReturn412WithoutTouchingBytes() = withServer { h ->
        val target = h.file("base.bin")
        val original = ByteArray(1024) { i -> (i * 29 + 7).toByte() }
        target.writeBytes(original)
        val stale = "\"${target.length() + 1}-${target.lastModified()}\""

        val mismatch = h.putChunk(target, "base-mismatch", 0, byteArrayOf(1, 2, 3), 1024, stale, expected = 412)
        assertEquals(ErrCode.PRECONDITION_FAILED, h.error(mismatch))
        assertArrayEquals(original, target.readBytes())
        val missing = h.file("missing.bin")
        val missingReply = h.putChunk(missing, "base-missing", 0, byteArrayOf(1), 1, "\"1-1\"", expected = 412)
        assertEquals(ErrCode.PRECONDITION_FAILED, h.error(missingReply))
        assertFalse(missing.exists())
        assertNull(h.root.listFiles()?.firstOrNull { it.name.startsWith(".phonefs-") })
    }

    @Test
    fun deltaConcurrentChangeBeforeFinalReturns412AndPreservesChangedTarget() = withServer { h ->
        val target = h.file("concurrent.bin")
        val original = "original-base-bytes".toByteArray()
        target.writeBytes(original)
        val base = etag(target)
        assertFalse(h.write(h.putChunk(target, "concurrent-change", 0, byteArrayOf(9, 9), original.size.toLong(), base)).complete)

        val external = "external-concurrent-value".toByteArray()
        target.writeBytes(external)
        assertTrue(target.setLastModified(target.lastModified() + 5000))
        val beforeFinal = target.readBytes()
        val reply = h.putChunk(
            target, "concurrent-change", 4, byteArrayOf(8, 8), original.size.toLong(), base,
            final = true, expected = 412,
        )

        assertEquals(ErrCode.PRECONDITION_FAILED, h.error(reply))
        assertArrayEquals(beforeFinal, target.readBytes())
        assertArrayEquals(external, target.readBytes())
    }

    @Test
    fun deltaDeletedTargetBeforeFinalReturns412() = withServer { h ->
        val target = h.file("deleted-before-final.bin")
        target.writeText("base-content")
        val base = etag(target)
        assertFalse(h.write(h.putChunk(target, "deleted-before-final", 0, byteArrayOf(1), 12, base)).complete)
        assertTrue(target.delete())

        val reply = h.putChunk(target, "deleted-before-final", 1, byteArrayOf(2), 12, base, final = true, expected = 412)

        assertEquals(ErrCode.PRECONDITION_FAILED, h.error(reply))
        assertFalse(target.exists())
    }

    @Test
    fun deltaFinalRetransmitReturnsOriginalResultAndCannotFakeSuccessAfterTargetChange() = withServer { h ->
        val target = h.file("retransmit.bin")
        target.writeText("abcdefgh")
        val base = etag(target)
        val done = h.write(h.putChunk(target, "final-retransmit", 2, "XY".toByteArray(), 8, base, final = true))
        assertTrue(done.complete)
        val committedBytes = target.readBytes()

        val replay = h.write(h.putChunk(target, "final-retransmit", 2, "XY".toByteArray(), 8, base, final = true))
        assertEquals(done, replay)
        assertArrayEquals(committedBytes, target.readBytes())

        target.writeText("external")
        assertTrue(target.setLastModified(target.lastModified() + 5000))
        val changedBytes = target.readBytes()
        val changed = h.putChunk(
            target, "final-retransmit", 2, "XY".toByteArray(), 8, base,
            final = true, expected = 412,
        )
        assertEquals(ErrCode.PRECONDITION_FAILED, h.error(changed))
        assertArrayEquals(changedBytes, target.readBytes())
    }

    @Test
    fun uploadIdCannotMixLegacyAndDeltaBaseModes() = withServer { h ->
        val target = h.file("mode.bin")
        target.writeText("12345678")
        assertFalse(h.write(h.putChunk(target, "mixed-mode", 0, byteArrayOf(1), 8)).complete)
        val base = etag(target)
        val deltaAttempt = h.putChunk(target, "mixed-mode", 1, byteArrayOf(2), 8, base, expected = 400)
        assertEquals(ErrCode.INVALID_ARGUMENT, h.error(deltaAttempt))

        assertFalse(h.write(h.putChunk(target, "mixed-mode-reverse", 0, byteArrayOf(1), 8, base)).complete)
        val legacyAttempt = h.putChunk(target, "mixed-mode-reverse", 1, byteArrayOf(2), 8, expected = 400)
        assertEquals(ErrCode.INVALID_ARGUMENT, h.error(legacyAttempt))
        assertEquals("12345678", target.readText())
    }

    @Test
    fun finalHeaderWithoutBaseAndInvalidBooleanAreRejected() = withServer { h ->
        val target = h.file("header-validation.bin")
        target.writeText("1234")
        val noBase = h.putChunk(target, "header-no-base", 0, byteArrayOf(1), 4, final = true, expected = 400)
        assertEquals(ErrCode.INVALID_ARGUMENT, h.error(noBase))
        val invalid = h.request(
            HttpMethod.Put,
            h.endpoint("/v1/fs/content", target),
            byteArrayOf(1),
            mapOf(
                "Content-Range" to "bytes 0-0/4",
                "X-Upload-Id" to "header-invalid",
                "X-Base-ETag" to etag(target),
                "X-Upload-Final" to "yes",
            ),
            expected = 400,
        )
        assertEquals(ErrCode.INVALID_ARGUMENT, h.error(invalid))
    }

    @Test
    fun patchReplaceAtomicallyReplacesTargetWithoutDeletingSourceFirst() = withServer { h ->
        val source = h.file("source.bin")
        val target = h.file("target.bin")
        source.writeText("source-content")
        target.writeText("target-content")

        assertEquals(ErrCode.ALREADY_EXISTS, h.error(h.patch(source, name = target.name, expected = 409)))
        assertEquals("source-content", source.readText())
        assertEquals("target-content", target.readText())
        val result = h.json<Attr>(h.patch(source, name = target.name, replace = true))

        assertEquals(target.name, result.name)
        assertFalse(source.exists())
        assertEquals("source-content", target.readText())
        assertEquals(1, h.root.listFiles { file -> file.name == target.name }?.size)
    }

    @Test
    fun patchReplaceRefusesDirectoryTargetAndPreservesBothEntries() = withServer { h ->
        val source = h.file("file-source.bin")
        val target = h.file("directory-target")
        source.writeText("keep-source")
        assertTrue(target.mkdir())
        val marker = File(target, "marker")
        marker.writeText("keep-marker")

        val reply = h.patch(source, name = target.name, replace = true, expected = 409)

        assertEquals(ErrCode.ALREADY_EXISTS, h.error(reply))
        assertEquals("keep-source", source.readText())
        assertEquals("keep-marker", marker.readText())
    }

    @Test
    fun patchCaseOnlyRenameIsNotAnExistingTargetConflict() = withServer { h ->
        val source = h.file("CaseName.bin")
        source.writeText("case-only-content")

        val result = h.json<Attr>(h.patch(source, name = "casename.bin"))

        assertEquals("casename.bin", result.name)
        assertEquals("case-only-content", h.file("casename.bin").readText())
        assertEquals(listOf("casename.bin"), h.root.listFiles()?.map { it.name })
    }

    private fun withServer(
        block: suspend (Harness) -> Unit,
    ) = runBlocking {
        val root = Files.createTempDirectory("phonefs-server-write-test").toFile()
        val backend = TestBackend(root)
        val server = PhoneFsServer(
            backend = backend,
            identity = Identity(certPem = "", keyPem = "", fingerprint = "test-fingerprint"),
            deviceId = "test-device",
            deviceName = "test-device",
            deviceFormFactor = "phone",
            token = TOKEN,
        )
        val engine = embeddedServer(CIO, host = "127.0.0.1", port = 0) {
            with(server) { configure() }
        }
        try {
            engine.start(wait = false)
            val port = engine.engine.resolvedConnectors().single().port
            block(Harness(HttpClient.newHttpClient(), "http://127.0.0.1:$port", backend, root))
        } finally {
            engine.stop(1_000, 1_000)
            server.stop()
            root.deleteRecursively()
        }
    }

    private data class Reply(val status: Int, val bytes: ByteArray) {
        val text: String get() = bytes.decodeToString()
    }

    private inner class Harness(
        private val client: HttpClient,
        private val baseUrl: String,
        private val backend: TestBackend,
        val root: File,
    ) {
        private val wireJson = Json { encodeDefaults = false }

        fun file(relative: String): File = File(root, relative)

        fun id(file: File): String = DocIds.encodePath(file.absolutePath)

        fun endpoint(path: String, file: File): String = endpoint(path, id(file))

        fun endpoint(path: String, id: String): String = "$path?id=${id.encodeURLParameter()}"

        suspend fun request(
            method: HttpMethod,
            path: String,
            body: ByteArray? = null,
            headers: Map<String, String> = emptyMap(),
            expected: Int = 200,
        ): Reply {
            val httpRequest = HttpRequest.newBuilder(URI.create("$baseUrl$path"))
                .header("Authorization", "Bearer $TOKEN")
                .apply { headers.forEach { (name, value) -> header(name, value) } }
                .method(
                    method.value,
                    body?.let { HttpRequest.BodyPublishers.ofByteArray(it) }
                        ?: HttpRequest.BodyPublishers.noBody(),
                )
                .build()
            val response = client.send(httpRequest, HttpResponse.BodyHandlers.ofByteArray())
            val reply = Reply(response.statusCode(), response.body())
            assertEquals("unexpected response: ${reply.text}", expected, reply.status)
            return reply
        }

        suspend fun create(parent: String, name: String, isDir: Boolean, expected: Int = 201): Reply = request(
            HttpMethod.Post,
            "/v1/fs/create",
            wireJson.encodeToString(CreateReq(parent, name, isDir)).toByteArray(),
            mapOf("Content-Type" to "application/json"),
            expected,
        )

        suspend fun putFull(file: File, data: ByteArray, mtime: Long? = null, expected: Int = 200): Reply =
            request(
                HttpMethod.Put,
                endpoint("/v1/fs/content", file),
                data,
                mtime?.let { mapOf("X-Set-Mtime" to it.toString()) } ?: emptyMap(),
                expected,
            )

        suspend fun putChunk(
            file: File,
            uploadId: String,
            start: Long,
            data: ByteArray,
            total: Long,
            base: String? = null,
            final: Boolean = false,
            expected: Int = 200,
        ): Reply {
            val headers = mutableMapOf(
                "Content-Range" to "bytes $start-${start + data.size - 1}/$total",
                "X-Upload-Id" to uploadId,
            )
            if (base != null) headers["X-Base-ETag"] = base
            if (final) headers["X-Upload-Final"] = "true"
            return request(HttpMethod.Put, endpoint("/v1/fs/content", file), data, headers, expected)
        }

        suspend fun patch(
            file: File,
            name: String? = null,
            parentId: String? = null,
            modifiedMs: Long? = null,
            replace: Boolean = false,
            expected: Int = 200,
        ): Reply = request(
            HttpMethod.Patch,
            "/v1/fs/meta",
            wireJson.encodeToString(MetaReq(id(file), name, parentId, modifiedMs, replace)).toByteArray(),
            mapOf("Content-Type" to "application/json"),
            expected,
        )

        suspend fun get(file: File, expected: Int = 200): Reply =
            request(HttpMethod.Get, endpoint("/v1/fs/content", file), expected = expected)

        suspend fun delete(file: File, recursive: Boolean, expected: Int): Reply = request(
            HttpMethod.Delete,
            endpoint("/v1/fs", file) + "&recursive=$recursive",
            expected = expected,
        )

        fun write(reply: Reply): WriteResp = json(reply)

        inline fun <reified T> json(reply: Reply): T = Json.decodeFromString(reply.text)

        fun errorEnvelope(reply: Reply): ErrorEnvelope = json(reply)

        fun error(reply: Reply): String = errorEnvelope(reply).error.code
    }

    private class TestBackend(override val storageRoot: File) : FsBackend {
        private val freeBytes = 1024L * 1024L * 1024L

        override fun ready(): Boolean = true

        override fun statFs(): CapacityInfo = CapacityInfo(freeBytes * 2, freeBytes, 4096)

        override fun list(path: String): List<FileInfo> =
            File(path).listFiles()?.sortedBy { it.name }?.mapNotNull { stat(it.absolutePath) } ?: emptyList()

        override fun stat(path: String): FileInfo? {
            val file = File(path)
            if (!file.exists()) return null
            return FileInfo(
                path = file.absolutePath,
                name = file.name,
                isDir = file.isDirectory,
                size = if (file.isFile) file.length() else 0,
                modifiedMs = file.lastModified(),
                mime = if (file.isFile) "application/octet-stream" else null,
                writable = true,
            )
        }

        override fun openInput(path: String, offset: Long): FileInputStream? {
            val file = File(path)
            if (!file.isFile) return null
            return FileInputStream(file).also { it.channel.position(offset) }
        }

        override fun isInsideStorage(path: String): Boolean {
            val rootPath = storageRoot.canonicalFile.toPath()
            return runCatching { File(path).canonicalFile.toPath().startsWith(rootPath) }.getOrDefault(false)
        }

        override fun scanFile(path: String) = Unit
    }

    companion object {
        private const val TOKEN = "phonefs-jvm-test-token"

        private fun etag(target: File): String = "\"${target.length()}-${target.lastModified()}\""
    }
}
