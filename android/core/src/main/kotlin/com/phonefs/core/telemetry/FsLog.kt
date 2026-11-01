package com.phonefs.core.telemetry

import android.util.Log
import java.io.File
import java.io.PrintWriter
import java.io.StringWriter
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * W2 证据日志：镜像 logcat + app-private 文件轮转（真机 logcat 对 App 自打日志采集不稳定，
 * 心跳/异常需要可靠落盘）。SPEC §7.7 审计文件轮转的 spike 简化版：单文件 2MB，保留 1 份 .1。
 */
object FsLog {
    @Volatile private var logFile: File? = null
    private val lock = Any()
    private const val MAX_BYTES = 2L * 1024 * 1024
    private val fmt = SimpleDateFormat("MM-dd HH:mm:ss.SSS", Locale.US)

    fun init(filesDir: File) {
        val dir = File(filesDir, "logs").apply { mkdirs() }
        logFile = File(dir, "phonefs.log")
        attachJul()
    }

    /** Netty/Ktor 内部日志走 java.util.logging（Android 默认不进 logcat），挂到同一文件抓 TLS 异常。 */
    private fun attachJul() {
        try {
            val root = java.util.logging.Logger.getLogger("")
            root.level = java.util.logging.Level.INFO
            root.addHandler(object : java.util.logging.Handler() {
                override fun publish(record: java.util.logging.LogRecord) {
                    val sb = StringBuilder("[JUL ${record.loggerName}] ${record.message}")
                    record.thrown?.let {
                        val sw = StringWriter(); it.printStackTrace(PrintWriter(sw)); sb.append('\n').append(sw)
                    }
                    write(if (record.level.intValue() >= java.util.logging.Level.WARNING.intValue()) "E" else "I", "jul", sb.toString())
                }
                override fun flush() {}
                override fun close() {}
            })
        } catch (_: Throwable) {
        }
    }

    fun i(tag: String, msg: String) {
        Log.i(tag, msg)
        write("I", tag, msg)
    }

    fun e(tag: String, msg: String, t: Throwable? = null) {
        Log.e(tag, msg, t)
        val sb = StringBuilder(msg)
        if (t != null) {
            val sw = StringWriter()
            t.printStackTrace(PrintWriter(sw))
            sb.append('\n').append(sw.toString())
        }
        write("E", tag, sb.toString())
    }

    private fun write(level: String, tag: String, msg: String) {
        val f = logFile ?: return
        synchronized(lock) {
            try {
                if (f.length() > MAX_BYTES) {
                    File(f.parentFile, "phonefs.log.1").delete()
                    f.renameTo(File(f.parentFile, "phonefs.log.1"))
                }
                f.appendText("${fmt.format(Date())} $level/$tag(${android.os.Process.myPid()}) $msg\n", Charsets.UTF_8)
            } catch (_: Throwable) {
                // 日志失败不影响业务
            }
        }
    }
}
