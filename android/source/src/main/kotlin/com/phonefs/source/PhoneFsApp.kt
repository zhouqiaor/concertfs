package com.phonefs.source

import android.app.Application
import android.util.Log
import com.phonefs.core.crypto.IdentityStore
import com.phonefs.core.telemetry.FsLog
import org.conscrypt.Conscrypt
import java.security.Security

class PhoneFsApp : Application() {

    lateinit var store: IdentityStore
        private set

    override fun onCreate() {
        super.onCreate()
        FsLog.init(filesDir)
        // Android 上 Netty direct ByteBuf 的 unsafe/Cleaner 回收不可靠（启动有 PlatformDependent
        // 警告），TLS 流式响应路径实测每 10MB 拉流泄漏约 1.1GB RSS（Unknown 段），累积触发
        // 厂商 SWAP 引擎按 PSI 杀进程。必须在任何 Netty 类加载前强制 unpooled + heap：
        // heap ByteBuffer 由 GC 回收，direct RSS 不再累积。
        System.setProperty("io.netty.allocator.type", "unpooled")
        System.setProperty("io.netty.noPreferDirect", "true")
        System.setProperty("io.netty.noUnsafe", "false")
        // F-W2-3（真机闸门实证）：Netty 的 ConscryptAlpnSslEngine 默认把 Conscrypt 的
        // BufferAllocator 桥到 Netty ByteBufAllocator（每个 TLS record 按需 alloc.directBuffer）。
        // Android 上该桥实测随密文流量线性泄漏（100 分块内 RSS 220MB→9GB 被厂商 SWAP 杀；
        // 明文口 100 分块 24MiB/s RSS 恒定，握手 100 次不泄漏 → 锁定 record 数据路径）。
        // 关闭后 Conscrypt 改为引擎内部单块 max-packet direct 缓冲（每连接一块）。
        System.setProperty("io.netty.handler.ssl.conscrypt.useBufferAllocator", "false")
        runCatching {
            // SIMPLE：抽样即可（PARANOID 每缓冲采样，会显著拖慢 10GB 压测）。
            io.netty.util.ResourceLeakDetector.setLevel(io.netty.util.ResourceLeakDetector.Level.SIMPLE)
        }.onFailure { FsLog.e(TAG, "set leak detector failed", it) }
        // §7.3：Conscrypt 置 Provider 首位，保证 API26–28 亦有 TLS1.3。
        // W2 真机 A/B 闸门：外存标记文件 PhoneFS-w2/DISABLE_BUNDLED_CONSCRYPT 存在时
        // 跳过内置 Conscrypt 2.8.0，改用 ROM 平台 JSSE（API31 自带 TLS1.3），
        // 用于定位 TLS 拉流内存暴涨归因（内置 provider vs Netty 管线）。
        val marker = java.io.File(android.os.Environment.getExternalStorageDirectory(),
            "PhoneFS-w2/DISABLE_BUNDLED_CONSCRYPT")
        val useBundled = !marker.exists()
        val inserted = if (useBundled) {
            try {
                Security.insertProviderAt(Conscrypt.newProvider(), 1) == 1
            } catch (t: Throwable) {
                Log.e(TAG, "Conscrypt init failed", t)
                false
            }
        } else false
        FsLog.i(TAG, "Conscrypt bundled=$useBundled inserted=$inserted providers=" +
            Security.getProviders().joinToString(",") { it.name })
        store = IdentityStore(this)
    }

    companion object {
        const val TAG = "phonefs.app"
    }
}
