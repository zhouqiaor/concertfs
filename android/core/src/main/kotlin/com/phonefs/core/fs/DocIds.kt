package com.phonefs.core.fs

import android.util.Base64

/**
 * §6.2 docId 规则（W2 实现子集）：
 *  - root           虚拟根（8 个固定条目）
 *  - stor:primary   内部共享存储根（= Environment.getExternalStorageDirectory）
 *  - p:<base64url>  真实文件/目录，base64url 无填充，载荷为 UTF-8 绝对路径
 *  - cat:*          分类条目（W2 list 返回 501 NOT_SUPPORTED，index 后端 M1）
 */
object DocIds {
    const val ROOT = "root"
    const val STOR_PRIMARY = "stor:primary"
    const val P_PREFIX = "p:"
    const val CAT_PREFIX = "cat:"

    /** 路径 → p: id（UTF-8 → base64url 无填充）。 */
    fun encodePath(absPath: String): String =
        P_PREFIX + Base64.encodeToString(
            absPath.toByteArray(Charsets.UTF_8),
            Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP,
        )

    /** 解码 p: id 为绝对路径；非 p: 或非法 base64 返回 null。 */
    fun decodePath(id: String): String? {
        if (!id.startsWith(P_PREFIX)) return null
        return try {
            val bytes = Base64.decode(
                id.substring(P_PREFIX.length),
                Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP,
            )
            String(bytes, Charsets.UTF_8)
        } catch (_: IllegalArgumentException) {
            null
        }
    }
}
