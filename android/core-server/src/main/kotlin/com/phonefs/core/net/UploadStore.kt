package com.phonefs.core.net

import com.phonefs.core.fs.FsBackend
import com.phonefs.core.protocol.ErrCode
import com.phonefs.core.protocol.PhoneFsException
import com.phonefs.core.protocol.WriteResp
import com.phonefs.core.telemetry.FsLog
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
 * 按冻结契约返回 409 UPLOAD_INCOMPLETE（会话保留可补传）。带 X-Base-ETag 的 delta
 * 会话先复制目标再扩展，仅 final 块提交且不要求区间覆盖全文件。完成会话保留 24h，
 * 同 Upload-Id 重传只消费请求体并返回原完成结果，目标 ETag 改变则拒绝伪成功。
 * 会话在进程内存中，进程死亡即失效；优雅停止清临时文件，硬崩溃残留与启动清扫挂账。
 */
internal class UploadStore(
    private val backend: FsBackend,
    private val mtimeSetter: (File, Long) -> Boolean = { file, modified -> file.setLastModified(modified) },
    private val warning: (String, Throwable?) -> Unit = { message, error ->
        FsLog.e("phonefs.upload", message, error)
    },
) {

    private data class Session(
        val target: File,
        val total: Long,
        val temp: File,
        val baseEtag: String?,
        val ranges: MutableList<LongRange> = mutableListOf(),
        val lock: Mutex = Mutex(),
        var touchedMs: Long = System.currentTimeMillis(),
        var completedResult: WriteResp? = null,
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
        baseEtag: String? = null,
        final: Boolean = false,
    ): WriteResp {
        if (total <= 0 || start < 0 || end < start || end >= total) throw badRange()
        if (final && baseEtag == null) {
            throw PhoneFsException(
                ErrCode.INVALID_ARGUMENT, HttpStatusCode.BadRequest,
                "X-Upload-Final requires X-Base-ETag",
            )
        }
        val expected = end - start + 1
        val channel = call.receiveChannel()
        return withContext(Dispatchers.IO) {
            val session = synchronized(sessions) {
                purgeExpiredLocked()
                sessions[uploadId]?.let {
                    if (it.target.absolutePath != target.absolutePath || it.total != total || it.baseEtag != baseEtag) {
                        throw PhoneFsException(
                            ErrCode.INVALID_ARGUMENT, HttpStatusCode.BadRequest,
                            "X-Upload-Id is already bound to another target/total/base mode",
                        )
                    }
                }
                sessions[uploadId] ?: run {
                    // base 前置条件优先于空间分配；缺失/不匹配都不得创建会话或 temp。
                    if (baseEtag != null) requireBaseTarget(target, baseEtag)
                    ensureSpace(total)
                    val temp = temporary(target, "upload")
                    try {
                        if (baseEtag == null) {
                            RandomAccessFile(temp, "rw").use { it.setLength(total) }
                        } else {
                            Files.copy(target.toPath(), temp.toPath())
                            RandomAccessFile(temp, "rw").use { it.setLength(total) }
                            // 复制期间目标若变化，本会话绝不能基于混合 base 继续。
                            requireBaseTarget(target, baseEtag)
                        }
                    } catch (t: Throwable) {
                        temp.delete()
                        throw t
                    }
                    Session(target, total, temp, baseEtag).also { sessions[uploadId] = it }
                }
            }

            // copyTo 会挂起；协程 Mutex 不绑定线程，恢复后可安全 unlock，只串行化同一 Upload-Id。
            session.lock.lock()
            try {
                session.touchedMs = System.currentTimeMillis()
                val completed = session.completedResult
                if (completed != null) {
                    // 完成后的任意同会话重传只消费请求体，不再触碰已原子替换的目标。
                    val copied = channel.copyTo(DiscardOutput, expected + 1)
                    if (copied != expected) throw incompleteBody(expected, copied)
                    if (!session.target.isFile || etag(session.target) != completed.etag) {
                        throw preconditionFailed("upload target changed after completion")
                    }
                    completed
                } else {
                    RandomAccessFile(session.temp, "rw").use { raf ->
                        raf.seek(start)
                        val copied = channel.copyTo(RandomAccessOutput(raf), expected + 1)
                        if (copied != expected) throw incompleteBody(expected, copied)
                        raf.fd.sync()
                    }
                    markReceived(session.ranges, start, end)
                    if (session.baseEtag != null) {
                        // delta 非 final 永不提交；final 只要求 base 未变，不要求 ranges 覆盖全文件。
                        if (!final) {
                            WriteResp(received = end + 1, complete = false)
                        } else {
                            requireBaseTarget(session.target, session.baseEtag)
                            val result = commit(session.temp, session.target, mtimeMs)
                            session.completedResult = result
                            result
                        }
                    } else {
                        val complete = isComplete(session.ranges, total)
                        if (complete) {
                            val result = commit(session.temp, session.target, mtimeMs)
                            session.completedResult = result
                            result
                        } else {
                            if (end + 1 == total) {
                                throw PhoneFsException(
                                    ErrCode.UPLOAD_INCOMPLETE,
                                    HttpStatusCode.Conflict,
                                    "last chunk arrived but upload is incomplete",
                                    true,
                                    mapOf("received" to contiguousReceived(session.ranges)),
                                )
                            }
                            WriteResp(received = end + 1, complete = false)
                        }
                    }
                }
            } finally {
                session.lock.unlock()
            }
        }
    }

    fun close() {
        synchronized(sessions) {
            sessions.values.forEach { if (it.completedResult == null) it.temp.delete() }
            sessions.clear()
        }
    }

    private fun temporary(target: File, kind: String): File {
        val parent = target.parentFile
            ?: throw PhoneFsException(ErrCode.INVALID_ARGUMENT, HttpStatusCode.BadRequest, "target has no parent")
        return File(parent, ".phonefs-$kind-${UUID.randomUUID()}.tmp")
    }

    internal fun commit(temp: File, target: File, mtimeMs: Long?): WriteResp {
        try {
            Files.move(temp.toPath(), target.toPath(), StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING)
        } catch (_: AtomicMoveNotSupportedException) {
            // 同目录 rename 仍是 M2 的最佳可用原子形态；ROM 不支持 ATOMIC_MOVE 时显式回退。
            Files.move(temp.toPath(), target.toPath(), StandardCopyOption.REPLACE_EXISTING)
        }
        // move 成功即内容完成。此后 mtime/媒体回映失败不得再把同 Upload-Id 打成未完成，
        // 否则重试会重建稀疏 temp 并覆盖已完整目标（Colossus P1-1）。
        val modified = mtimeMs ?: System.currentTimeMillis()
        val mtimeError = try {
            if (mtimeSetter(target, modified)) null else IOException("setLastModified returned false")
        } catch (t: Throwable) {
            t
        }
        if (mtimeError != null) {
            warning("atomic move completed; mtime failed but upload remains complete", mtimeError)
        }
        try {
            backend.scanFile(target.absolutePath) // FR-20 MES 回映
        } catch (t: Throwable) {
            warning("atomic move completed; MediaStore scan failed but upload remains complete", t)
        }
        return WriteResp(target.length(), complete = true, etag = etag(target))
    }

    private fun etag(target: File): String = "\"${target.length()}-${target.lastModified()}\""

    private fun requireBaseTarget(target: File, expected: String) {
        val actual = if (target.isFile) etag(target) else null
        if (actual != expected) {
            throw PhoneFsException(
                ErrCode.PRECONDITION_FAILED,
                HttpStatusCode.PreconditionFailed,
                "upload base ETag does not match current target",
                false,
                mapOf("expected" to expected, "actual" to (actual ?: "missing")),
            )
        }
    }

    private fun preconditionFailed(message: String) = PhoneFsException(
        ErrCode.PRECONDITION_FAILED,
        HttpStatusCode.PreconditionFailed,
        message,
    )

    private fun ensureSpace(bytes: Long) {
        if (backend.statFs().freeBytes < bytes) {
            throw PhoneFsException(
                ErrCode.INSUFFICIENT_STORAGE,
                HttpStatusCode(507, "Insufficient Storage"),
                "not enough free space for upload",
                false,
                mapOf("required" to bytes),
            )
        }
    }

    private fun purgeExpiredLocked() {
        val now = System.currentTimeMillis()
        val expired = sessions.filterValues { now - it.touchedMs > TTL_MS }.keys.toList()
        expired.forEach { id ->
            sessions.remove(id)?.let { if (it.completedResult == null) it.temp.delete() }
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
        mapOf("expected" to expected, "received" to copied),
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
