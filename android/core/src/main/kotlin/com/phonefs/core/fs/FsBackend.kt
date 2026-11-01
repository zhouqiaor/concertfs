package com.phonefs.core.fs

import java.io.File
import java.io.FileInputStream

/** §5.8 FsBackend 的 W2 最小子集（只读：stat/list/openRead/statFs）。 */
interface FsBackend {
    /** 内部共享存储根 File（=/storage/emulated/0）。 */
    val storageRoot: File

    /** MES「所有文件访问」是否已授权。 */
    fun ready(): Boolean

    fun statFs(): CapacityInfo

    /** 列目录（调用方保证 path 在 storageRoot 内且 isDirectory）。 */
    fun list(path: String): List<FileInfo>

    fun stat(path: String): FileInfo?

    /** 打开只读句柄；offset 由服务端 seek。文件不存在返回 null。 */
    fun openInput(path: String, offset: Long): FileInputStream?

    /** 路径越界（T10）/非存储根内 → false。 */
    fun isInsideStorage(path: String): Boolean

    /** FR-20：写完成/改名/删除后触发 MediaStore 回映；目录由实现自行忽略。 */
    fun scanFile(path: String)
}

data class FileInfo(
    val path: String,
    val name: String,
    val isDir: Boolean,
    val size: Long,
    val modifiedMs: Long,
    val mime: String?,
    val writable: Boolean,
)

data class CapacityInfo(
    val totalBytes: Long,
    val freeBytes: Long,
    val blockSize: Long,
)
