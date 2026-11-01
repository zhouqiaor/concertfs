package com.phonefs.core.net

import com.phonefs.core.fs.CapacityInfo
import com.phonefs.core.fs.FileInfo
import com.phonefs.core.fs.FsBackend
import com.phonefs.core.protocol.WriteResp
import io.ktor.client.request.header
import io.ktor.client.request.put
import io.ktor.client.request.setBody
import io.ktor.client.statement.bodyAsText
import io.ktor.http.ContentType
import io.ktor.server.application.call
import io.ktor.server.request.header
import io.ktor.server.response.respondText
import io.ktor.server.routing.put
import io.ktor.server.routing.routing
import io.ktor.server.testing.ApplicationTestBuilder
import io.ktor.server.testing.testApplication
import kotlinx.serialization.json.Json
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File
import java.io.FileInputStream
import java.nio.file.Files

class UploadStoreTest {

    @Test
    fun deltaSameSizeCopiesBaseAndOverwritesOnlyDirtyBlocks() = testApplication {
        val dir = Files.createTempDirectory("phonefs-delta-same-size")
        try {
            val target = dir.resolve("target.bin").toFile()
            val original = ByteArray(256) { i -> (i * 17 + 3).toByte() }
            target.writeBytes(original)
            val store = UploadStore(FakeBackend(dir.toFile()))
            installStore(store, target)
            val client = StoreClient(this, "delta-same-size")
            val base = etag(target)
            val first = ByteArray(16) { i -> (100 + i).toByte() }
            val second = ByteArray(16) { i -> (200 + i).toByte() }

            val pending = client.put(17, first, original.size.toLong(), base, final = false)
            assertFalse(pending.complete)
            assertArrayEquals(original, target.readBytes())
            val done = client.put(200, second, original.size.toLong(), base, final = true)

            val expected = original.copyOf()
            first.copyInto(expected, 17)
            second.copyInto(expected, 200)
            assertTrue(done.complete)
            assertEquals(original.size.toLong(), done.received)
            assertNotNull(done.etag)
            assertArrayEquals(expected, target.readBytes())
        } finally {
            dir.toFile().deleteRecursively()
        }
    }

    @Test
    fun deltaOutOfOrderFullCoverageStillWaitsForFinalBlock() = testApplication {
        val dir = Files.createTempDirectory("phonefs-delta-out-of-order")
        try {
            val target = dir.resolve("target.bin").toFile()
            val original = "old-base".toByteArray()
            target.writeBytes(original)
            val store = UploadStore(FakeBackend(dir.toFile()))
            installStore(store, target)
            val client = StoreClient(this, "delta-out-of-order")
            val base = etag(target)

            assertFalse(client.put(4, "NEW2".toByteArray(), 8, base, final = false).complete)
            assertFalse(client.put(0, "NEW1".toByteArray(), 8, base, final = false).complete)
            assertArrayEquals(original, target.readBytes())
            val done = client.put(0, "NEW1".toByteArray(), 8, base, final = true)

            assertTrue(done.complete)
            assertEquals("NEW1NEW2", target.readText())
        } finally {
            dir.toFile().deleteRecursively()
        }
    }

    @Test
    fun deltaFinalExtensionRetainsOriginalPrefix() = testApplication {
        val dir = Files.createTempDirectory("phonefs-delta-extension")
        try {
            val target = dir.resolve("target.bin").toFile()
            target.writeText("abcdef")
            val store = UploadStore(FakeBackend(dir.toFile()))
            installStore(store, target)
            val client = StoreClient(this, "delta-extension")

            val done = client.put(6, "WXYZ".toByteArray(), 10, etag(target), final = true)

            assertTrue(done.complete)
            assertEquals(10L, done.received)
            assertEquals("abcdefWXYZ", target.readText())
        } finally {
            dir.toFile().deleteRecursively()
        }
    }

    @Test
    fun commitMtimeFalseAfterMoveReturnsCompleteWithoutSparseRetryWindow() {
        verifyMtimeFailureCannotTurnCommittedContentBackIntoFailure { _, _ -> false }
    }

    @Test
    fun commitMtimeThrowAfterMoveReturnsCompleteWithoutSparseRetryWindow() {
        verifyMtimeFailureCannotTurnCommittedContentBackIntoFailure { _, _ ->
            throw SecurityException("injected setLastModified failure")
        }
    }

    private fun ApplicationTestBuilder.installStore(store: UploadStore, target: File) {
        application {
            routing {
                put("/upload") {
                    val start = call.request.header("X-Test-Start")!!.toLong()
                    val end = call.request.header("X-Test-End")!!.toLong()
                    val total = call.request.header("X-Test-Total")!!.toLong()
                    val uploadId = call.request.header("X-Test-Upload-Id")!!
                    val result = store.chunk(
                        call = call,
                        target = target,
                        uploadId = uploadId,
                        start = start,
                        end = end,
                        total = total,
                        mtimeMs = null,
                        baseEtag = call.request.header("X-Test-Base"),
                        final = call.request.header("X-Test-Final") == "true",
                    )
                    call.respondText(Json.encodeToString(result), ContentType.Application.Json)
                }
            }
        }
    }

    private class StoreClient(
        private val builder: ApplicationTestBuilder,
        private val uploadId: String,
    ) {
        suspend fun put(
            start: Long,
            data: ByteArray,
            total: Long,
            base: String,
            final: Boolean,
        ): WriteResp {
            val response = builder.client.put("/upload") {
                header("X-Test-Start", start)
                header("X-Test-End", start + data.size - 1)
                header("X-Test-Total", total)
                header("X-Test-Upload-Id", uploadId)
                header("X-Test-Base", base)
                header("X-Test-Final", final)
                setBody(data)
            }
            assertEquals(200, response.status.value)
            return Json.decodeFromString(response.bodyAsText())
        }
    }

    private fun verifyMtimeFailureCannotTurnCommittedContentBackIntoFailure(
        setter: (File, Long) -> Boolean,
    ) {
        val dir = Files.createTempDirectory("phonefs-upload-commit-test")
        try {
            val target = dir.resolve("target.bin").toFile().apply { writeBytes(byteArrayOf(1, 2, 3)) }
            val temp = dir.resolve(".phonefs-upload-test.tmp").toFile()
            val expected = ByteArray(128 * 1024 + 17) { i -> (i * 31 + 7).toByte() }
            temp.writeBytes(expected)
            val backend = FakeBackend(dir.toFile())
            val warnings = mutableListOf<String>()
            val store = UploadStore(
                backend = backend,
                mtimeSetter = setter,
                warning = { message, _ -> warnings += message },
            )

            // move 已成功但 mtime 失败时必须返回 complete=true；chunk 调用方据此置完成态，
            // 后续同 Upload-Id 只走完成态重传，不会重建稀疏 temp。
            val result = store.commit(temp, target, 1_700_000_000_123)

            assertTrue(result.complete)
            assertTrue(result.received == expected.size.toLong())
            assertArrayEquals(expected, target.readBytes())
            assertFalse(temp.exists())
            assertTrue(warnings.any { "upload remains complete" in it })
        } finally {
            dir.toFile().deleteRecursively()
        }
    }

    private fun etag(target: File): String = "\"${target.length()}-${target.lastModified()}\""

    private class FakeBackend(override val storageRoot: File) : FsBackend {
        override fun ready(): Boolean = true
        override fun statFs(): CapacityInfo = CapacityInfo(Long.MAX_VALUE, Long.MAX_VALUE, 4096)
        override fun list(path: String): List<FileInfo> = emptyList()
        override fun stat(path: String): FileInfo? = null
        override fun openInput(path: String, offset: Long): FileInputStream? = null
        override fun isInsideStorage(path: String): Boolean = true
        override fun scanFile(path: String) = Unit
    }
}
