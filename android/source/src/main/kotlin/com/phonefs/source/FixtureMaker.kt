package com.phonefs.source

import android.os.Environment
import com.phonefs.core.telemetry.FsLog
import java.io.File

/**
 * W2 测试夹具：内部存储/PhoneFS-w2/ 下造中文/emoji/空格文件名（MES 授权后调用）。
 * 10MB 压测文件由 adb push 放入（见 test/w2/README.md）。
 */
object FixtureMaker {
    const val DIR = "PhoneFS-w2"
    private const val TAG = "phonefs.fixture"

    fun ensure(): File {
        val root = File(Environment.getExternalStorageDirectory(), DIR)
        root.mkdirs()
        val files = mapOf(
            "测试 文件.txt" to "PhoneFS W2 中文+空格文件名夹具\n".toByteArray(Charsets.UTF_8),
            "中文目录/截图 😀.bin" to byteArrayOf(0x50, 0x48, 0x4F, 0x4E, 0x45, 0x46, 0x53, 0x21),
            "中文目录/a b c.md" to "# emoji dir fixture\n".toByteArray(Charsets.UTF_8),
        )
        for ((rel, bytes) in files) {
            val f = File(root, rel)
            f.parentFile?.mkdirs()
            if (!f.exists()) f.writeBytes(bytes)
        }
        FsLog.i(TAG, "fixture ready at ${root.absolutePath}: ${root.listFiles()?.joinToString { it.name }}")
        return root
    }
}
