package com.phonefs.core.net

import com.phonefs.core.fs.CapacityInfo
import com.phonefs.core.fs.FileInfo
import com.phonefs.core.fs.FsBackend
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File
import java.io.FileInputStream
import java.nio.file.Files

class UploadStoreTest {

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

    private class FakeBackend(override val storageRoot: File) : FsBackend {
        override fun ready(): Boolean = true
        override fun statFs(): CapacityInfo = CapacityInfo(1, 1, 4096)
        override fun list(path: String): List<FileInfo> = emptyList()
        override fun stat(path: String): FileInfo? = null
        override fun openInput(path: String, offset: Long): FileInputStream? = null
        override fun isInsideStorage(path: String): Boolean = true
        override fun scanFile(path: String) = Unit
    }
}
