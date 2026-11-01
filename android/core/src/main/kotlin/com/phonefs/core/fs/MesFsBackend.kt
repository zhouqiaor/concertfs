package com.phonefs.core.fs

import android.annotation.SuppressLint
import android.os.Environment
import android.os.StatFs
import android.webkit.MimeTypeMap
import java.io.File
import java.io.FileInputStream
import java.util.Locale

/**
 * §5.8 backend-fs：MES 授权后 java.io.File 真实树（ADR-07）。
 * W2：只读；MediaStore 写回映/Shizuku/分类索引均未做。
 */
class MesFsBackend : FsBackend {

    override val storageRoot: File = Environment.getExternalStorageDirectory()

    @SuppressLint("UnsafeOptInUsageError")
    override fun ready(): Boolean = Environment.isExternalStorageManager()

    override fun statFs(): CapacityInfo {
        val s = StatFs(storageRoot.absolutePath)
        return CapacityInfo(
            totalBytes = s.totalBytes,
            freeBytes = s.availableBytes,
            blockSize = s.blockSizeLong,
        )
    }

    override fun isInsideStorage(path: String): Boolean {
        val rootPath = storageRoot.absolutePath
        val canonical = try {
            File(path).canonicalPath
        } catch (_: Exception) {
            return false
        }
        return canonical == rootPath || canonical.startsWith(rootPath + File.separator)
    }

    override fun stat(path: String): FileInfo? {
        if (!isInsideStorage(path)) return null
        val f = File(path)
        if (!f.exists()) return null
        return f.toInfo()
    }

    override fun list(path: String): List<FileInfo> {
        val dir = File(path)
        val kids = dir.listFiles() ?: return emptyList()
        // §6.5.2：真实树 name ASC（大小写不敏感的代码点序；目录不分组）
        return kids
            .sortedBy { it.name.lowercase(Locale.ROOT) }
            .map { it.toInfo() }
    }

    override fun openInput(path: String, offset: Long): FileInputStream? {
        if (!isInsideStorage(path)) return null
        val f = File(path)
        if (!f.isFile) return null
        val fis = FileInputStream(f)
        return try {
            fis.channel.position(offset)
            fis
        } catch (t: Throwable) {
            fis.close()
            throw t
        }
    }

    private fun File.toInfo(): FileInfo = FileInfo(
        path = absolutePath,
        name = name,
        isDir = isDirectory,
        size = if (isDirectory) 0L else length(),
        modifiedMs = lastModified(),
        mime = if (isFile) guessMime(name) else null,
        writable = canWrite(),
    )

    private fun guessMime(name: String): String? {
        val dot = name.lastIndexOf('.')
        if (dot < 0 || dot == name.length - 1) return null
        val ext = name.substring(dot + 1).lowercase(Locale.US)
        return MimeTypeMap.getSingleton().getMimeTypeFromExtension(ext)
    }
}
