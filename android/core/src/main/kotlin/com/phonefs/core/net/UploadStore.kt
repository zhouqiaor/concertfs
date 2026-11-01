package com.phonefs.core.net

import com.phonefs.core.fs.FsBackend
import com.phonefs.core.protocol.ErrCode
import com.phonefs.core.protocol.PhoneFsException
import com.phonefs.core.protocol.WriteResp
import io.ktor.http.HttpStatusCode
import io.ktor.server.application.ApplicationCall
import io.ktor.server.request.receiveChannel
import io.ktor.utils.io.jvm.javaio.copyTo
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.withContext
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import java.io.OutputStream
import java.io.RandomAccessFile
import java.nio.file.AtomicMoveNotSupportedException
import java.nio.file.Files
import java.nio.file.StandardCopyOption
import java.util.UUID

/**
 * §6.5.7 写上传存储：请求体流式落同目录临时文件，完成后原子替换。
 *
 * 分块以 X-Upload-Id 建会话，任意顺序覆盖写并由区间表判全；末块先到且仍缺块时
 * 按冻结契约返回 409 UPLOAD_INCOMPLETE（会话保留可补传）。完成会话保留 24h，
 * 同 Upload-Id 重传不再改文件，直接返回 complete=true（含末块重试幂等）。
 * 会话在进程内存中，进程死亡即失效并清临时文件；M4 再评估跨重启续传。
 */
internal class UploadStore(private val backend: FsBackend) {

    private data class Session(
        val target: File,
        val total: Long,
        val temp: File,
        val ranges: MutableList<LongRange> = mutableListOf(),
        val lock: Mutex = Mutex(),
        var touchedMs: Long = System.currentTimeMillis(),
        var completed: Boolean = false,
    )

    private val sessions = mutableMapOf<String, Session>()

    suspend fun full(call: ApplicationCall, target: File, expected: Long, mtimeMs: Long?): WriteResp {
        require(expected >= 0)
        val channel = call.receiveChannel()
        return withContext(Dispatchers.IO) {
            ensureSpace(expected)
            val temp = temporary(target, "full")
            var moved = false
            try {
                FileOutputStream(temp).use { out ->
                    val copied = channel.copyTo(out, expected + 1)
                    if (copied != expected) throw incompleteBody(expected, copied)
                    out.fd.sync()
                }
                val result = commit(temp, target, mtimeMs)
                moved = true
                result
            } finally {
                if (!moved) temp.delete()
            }
        }
    }

    suspend fun chunk(
        call: ApplicationCall,
        target: File,
        uploadId: String,
        start: Long,
        end: Long,
        total: Long,
        mtimeMs: Long?,
    ): WriteResp {
        if (total <= 0 || start < 0 || end < start || end >= total) throw badRange()
        val expected = end - start + 1
        val channel = call.receiveChannel()
        return withContext(Dispatchers.IO) {
            val session = synchronized(sessions) {
                purgeExpiredLocked()
                sessions[uploadId]?.let {
                    if (it.target.absolutePath != target.absolutePath || it.total != total) {
                        throw PhoneFsException(
                            ErrCode.INVALID_ARGUMENT, HttpStatusCode.BadRequest,
                            "X-Upload-Id is already bound to another target/total",
                        )
                    }
                    // 目标被删除/重建后，旧完成态不能对空的新 inode 直接报成功。
                    if (it.completed && (!it.target.isFile || it.target.length() != it.total)) {
                        sessions.remove(uploadId)
                    }
                }
                sessions[uploadId] ?: run {
                    ensureSpace(total)
                    val temp = temporary(target, "upload")
                    try {
                        RandomAccessFile(temp, "rw").use { it.setLength(total) }
                    } catch (t: Throwable) {
                        temp.delete()
                        throw t
                    }
                    Session(target, total, temp).also { sessions[uploadId] = it }
                }
            }

            // copyTo 会挂起；协程 Mutex 不绑定线程，恢复后可安全 unlock，只串行化同一 Upload-Id。
            session.lock.lock()
            try {
                session.touchedMs = System.currentTimeMillis()
                if (session.completed) {
                    // 完成后的任意同会话重传只消费请求体，不再触碰已原子替换的目标。
                    val copied = channel.copyTo(DiscardOutput, expected + 1)
                    if (copied != expected) throw incompleteBody(expected, copied)
                    WriteResp(total, complete = true, etag = etag(target))
                } else {
                    RandomAccessFile(session.temp, "rw").use { raf ->
                        raf.seek(start)
                        val copied = channel.copyTo(RandomAccessOutput(raf), expected + 1)
                        if (copied != expected) throw incompleteBody(expected, copied)
                        raf.fd.sync()
                    }
                    markReceived(session.ranges, start, end)
                    val complete = isComplete(session.ranges, total)
                    if (complete) {
                        val result = commit(session.temp, session.target, mtimeMs)
                        session.completed = true
                        result
                    } else {
                        if (end + 1 == total) {
                            throw PhoneFsException(
                                ErrCode.UPLOAD_INCOMPLETE,
                                HttpStatusCode.Conflict,
                                "last chunk arrived but upload is incomplete",
                                true,
                                mapOf("received" to contiguousReceived(session.ranges).toString()),
                            )
                        }
                        WriteResp(received = end + 1, complete = false)
                    }
                }
            } finally {
                session.lock.unlock()
            }
        }
    }

    fun close() {
        synchronized(sessions) {
            sessions.values.forEach { if (!it.completed) it.temp.delete() }
            sessions.clear()
        }
    }

    private fun temporary(target: File, kind: String): File {
        val parent = target.parentFile
            ?: throw PhoneFsException(ErrCode.INVALID_ARGUMENT, HttpStatusCode.BadRequest, "target has no parent")
        return File(parent, ".phonefs-$kind-${UUID.randomUUID()}.tmp")
    }

    private fun commit(temp: File, target: File, mtimeMs: Long?): WriteResp {
        try {
            Files.move(temp.toPath(), target.toPath(), StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING)
        } catch (_: AtomicMoveNotSupportedException) {
            // 同目录 rename 仍是 M2 的最佳可用原子形态；ROM 不支持 ATOMIC_MOVE 时显式回退。
            Files.move(temp.toPath(), target.toPath(), StandardCopyOption.REPLACE_EXISTING)
        }
        val modified = mtimeMs ?: System.currentTimeMillis()
        if (!target.setLastModified(modified)) {
            throw IOException("setLastModified failed for completed upload")
        }
        backend.scanFile(target.absolutePath) // FR-20 MES 回映
        return WriteResp(target.length(), complete = true, etag = etag(target))
    }

    private fun etag(target: File): String = "\"${target.length()}-${target.lastModified()}\""

    private fun ensureSpace(bytes: Long) {
        if (backend.statFs().freeBytes < bytes) {
            throw PhoneFsException(
                ErrCode.INSUFFICIENT_STORAGE,
                HttpStatusCode(507, "Insufficient Storage"),
                "not enough free space for upload",
                false,
                mapOf("required" to bytes.toString()),
            )
        }
    }

    private fun purgeExpiredLocked() {
        val now = System.currentTimeMillis()
        val expired = sessions.filterValues { now - it.touchedMs > TTL_MS }.keys.toList()
        expired.forEach { id ->
            sessions.remove(id)?.let { if (!it.completed) it.temp.delete() }
        }
    }

    private fun markReceived(ranges: MutableList<LongRange>, start: Long, end: Long) {
        ranges.add(start..end)
        ranges.sortBy { it.first }
        val merged = mutableListOf<LongRange>()
        for (next in ranges) {
            val last = merged.lastOrNull()
            if (last != null && next.first <= last.last + 1) {
                merged[merged.lastIndex] = last.first..maxOf(last.last, next.last)
            } else {
                merged.add(next)
            }
        }
        ranges.clear()
        ranges.addAll(merged)
    }

    private fun isComplete(ranges: List<LongRange>, total: Long): Boolean =
        ranges.size == 1 && ranges[0].first == 0L && ranges[0].last == total - 1

    private fun contiguousReceived(ranges: List<LongRange>): Long =
        ranges.firstOrNull()?.takeIf { it.first == 0L }?.let { it.last + 1 } ?: 0L

    private fun incompleteBody(expected: Long, copied: Long) = PhoneFsException(
        ErrCode.UPLOAD_INCOMPLETE,
        HttpStatusCode.Conflict,
        "request body length mismatch",
        true,
        mapOf("expected" to expected.toString(), "received" to copied.toString()),
    )

    private fun badRange() = PhoneFsException(
        ErrCode.INVALID_ARGUMENT,
        HttpStatusCode.BadRequest,
        "invalid Content-Range",
    )

    private object DiscardOutput : OutputStream() {
        override fun write(b: Int) {}
        override fun write(b: ByteArray, off: Int, len: Int) {}
    }

    private class RandomAccessOutput(private val raf: RandomAccessFile) : OutputStream() {
        override fun write(b: Int) = raf.write(b)
        override fun write(b: ByteArray, off: Int, len: Int) = raf.write(b, off, len)
    }

    companion object {
        private const val TTL_MS = 24L * 60 * 60 * 1000
    }
}
