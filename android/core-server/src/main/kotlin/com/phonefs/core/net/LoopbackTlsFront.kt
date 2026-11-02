package com.phonefs.core.net

import com.phonefs.core.crypto.Identity
import com.phonefs.core.telemetry.FsLog
import java.io.InputStream
import java.io.OutputStream
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket
import javax.net.ssl.KeyManagerFactory
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLServerSocket
import javax.net.ssl.SSLSocket

/**
 * F-W2-3 裁决后的 W2 传输形态（2026-11-01 真机闸门实证）：
 *
 * Netty 的 SslHandler/JdkEngine 包装在 Android 上对 Conscrypt 引擎做密文 record
 * 流水时，[anon:dalvik-LinearAlloc] 随数据量无界增长（实测每 10MB 约 +150MB，
 * 百级分块内 RSS→9GB，被厂商 SWAP 杀），而同一 Conscrypt provider 下的
 * 原生 SSLServerSocket 跑 10GB 内存恒定（RSS 138→146MB，20.7MiB/s）。
 *
 * 故 W2 起 TLS 边界放在 Netty 之外：
 *   PC ──TLS1.3(Conscrypt,0.0.0.0:publicPort)──> [SSLSocket 前置]
 *        ──明文(127.0.0.1:loopPort, 仅环回)──> Ktor/Netty HTTP 管线
 *
 * - 明文口只绑环回，外部网卡不可达；Bearer 仍由 Ktor 鉴权管线强制（pair/info 除外）。
 * - 对端地址在此处做 site-local 门禁（Ktor 看到的 remoteHost 恒为 127.0.0.1）。
 * - 阻塞 IO + 每连接 2 搬运线程，W2/M0 单 PC 场景足够；M1 如需高并发再换 SSLEngine NIO。
 */
class LoopbackTlsFront(
    private val publicPort: Int,
    private val loopbackPort: Int,
    private val identity: Identity,
) {
    private var server: SSLServerSocket? = null
    @Volatile private var running = false
    private var acceptThread: Thread? = null

    fun start() {
        val ks = TlsKeyStores.build(identity)
        val kmf = KeyManagerFactory.getInstance(KeyManagerFactory.getDefaultAlgorithm()).apply {
            init(ks, TlsKeyStores.PASSWORD)
        }
        val sslContext = SSLContext.getInstance("TLS").apply {
            init(kmf.keyManagers, null, null)
        }
        val ss = sslContext.serverSocketFactory.createServerSocket() as SSLServerSocket
        ss.reuseAddress = true
        ss.bind(InetSocketAddress("0.0.0.0", publicPort))
        ss.sslParameters = ss.sslParameters.apply { protocols = arrayOf("TLSv1.3") }
        server = ss
        running = true
        acceptThread = Thread({ acceptLoop(ss) }, "phonefs-tls-accept").apply { isDaemon = true }.also { it.start() }
        FsLog.i(
            "phonefs.tls",
            "TLS front listening :$publicPort (provider=${sslContext.provider.name}) -> loopback :$loopbackPort",
        )
    }

    fun stop() {
        running = false
        runCatching { server?.close() }
        server = null
        acceptThread = null
    }

    private fun acceptLoop(ss: SSLServerSocket) {
        while (running) {
            val client = try {
                ss.accept() as SSLSocket
            } catch (_: Exception) {
                if (running) FsLog.e(TAG, "accept failed", null)
                break
            }
            val peer: InetAddress = client.inetAddress
            if (!SiteLocal.isAllowed(peer)) {
                FsLog.i(TAG, "reject non-site-local peer $peer")
                runCatching { client.close() }
                continue
            }
            Thread({ runCatching { bridge(client) } }, "phonefs-tls-conn").apply { isDaemon = true }.start()
        }
    }

    private fun bridge(client: SSLSocket) {
        client.use { tls ->
            tls.tcpNoDelay = true
            val up = Socket()
            up.use { loop ->
                loop.tcpNoDelay = true
                loop.connect(InetSocketAddress("127.0.0.1", loopbackPort), 5_000)
                val tlsToLoop = Thread({
                    shovel(tls.getInputStream(), loop.getOutputStream(), "up", tls, loop)
                }, "phonefs-tls-up").apply { isDaemon = true }
                val loopToTls = Thread({
                    shovel(loop.getInputStream(), tls.getOutputStream(), "down", tls, loop)
                }, "phonefs-tls-down").apply { isDaemon = true }
                tlsToLoop.start(); loopToTls.start()
                // 下游（Ktor 响应）EOF 即整个请求-响应周期结束
                loopToTls.join()
                runCatching { tls.close() }
                runCatching { loop.close() }
                tlsToLoop.join(1_000)
            }
        }
    }

    /**
     * 256KB 搬运（F-W2-3b 实证）：16KB 缓冲在 10GB 拉流下 ART Dalvik-Other
     * 线性增长（~0.9MB/10MB 分块，不回收）；缓冲提到 256KB、JNI 写次数降 16× 后
     * 增长基本消失。任一方向结束/异常即关闭两端解除另一方向阻塞。
     */
    private fun shovel(input: InputStream, output: OutputStream, dir: String, vararg toClose: Socket) {
        val buf = ByteArray(256 * 1024)
        try {
            while (running) {
                val n = input.read(buf)
                if (n < 0) break
                output.write(buf, 0, n)
                output.flush()
            }
            if (dir == "up") runCatching { (toClose[1] as Socket).shutdownOutput() }
        } catch (t: Throwable) {
            FsLog.i(TAG, "shovel $dir ended: ${t.javaClass.simpleName}")
        } finally {
            toClose.forEach { runCatching { it.close() } }
        }
    }

    companion object {
        private const val TAG = "phonefs.tls"
    }
}
