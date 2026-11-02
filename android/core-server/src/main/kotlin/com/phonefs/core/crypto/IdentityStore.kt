package com.phonefs.core.crypto

import android.content.Context
import android.util.Base64
import java.io.File
import java.security.SecureRandom

/**
 * 身份/凭证持久化（W2 简化版，对照 SPEC §7.6 差距见 W2 报告）：
 *  - identity/cert.pem、identity/key.pem 落 app-private filesDir，目录 0700；
 *  - device.id / bearer.token 同为 0600 文本文件；
 *  - 未做 AndroidKeyStore AES/GCM 主密钥加密（SPEC §7.6 vault），M1 前补。
 */
class IdentityStore(private val context: Context) {

    private val dir = File(context.filesDir, "identity").apply {
        mkdirs()
        // 0700：先清零再只给属主 rwx
        setReadable(false, false); setWritable(false, false); setExecutable(false, false)
        setReadable(true, true); setWritable(true, true); setExecutable(true, true)
    }
    private val certFile = File(dir, "cert.pem")
    private val keyFile = File(dir, "key.pem")
    private val deviceFile = File(dir, "device.id")
    private val tokenFile = File(dir, "token")

    fun loadOrCreateIdentity(): Identity {
        if (certFile.exists() && keyFile.exists()) {
            val certPem = certFile.readText(Charsets.UTF_8)
            val keyPem = keyFile.readText(Charsets.UTF_8)
            val der = java.security.cert.CertificateFactory.getInstance("X.509")
                .generateCertificate(certPem.byteInputStream())
                .encoded
            val fp = java.security.MessageDigest.getInstance("SHA-256").digest(der)
                .joinToString("") { "%02x".format(it) }
            return Identity(certPem, keyPem, fp)
        }
        val id = IdentityGenerator.generate(loadOrCreateDeviceId())
        writePrivate(certFile, id.certPem)
        writePrivate(keyFile, id.keyPem)
        return id
    }

    fun loadOrCreateDeviceId(): String {
        if (deviceFile.exists()) return deviceFile.readText().trim()
        // §6.4：s-<随机>；W2 用 8 字节 hex
        val raw = ByteArray(8).also { SecureRandom().nextBytes(it) }
        val id = "s-" + raw.joinToString("") { "%02x".format(it) }
        writePrivate(deviceFile, id)
        return id
    }

    /** 32B 随机 bearer，base64url 无填充（§6.1）。W2 预共享演示配对：App 屏展示。 */
    fun loadOrCreateToken(): String {
        if (tokenFile.exists()) return tokenFile.readText().trim()
        val raw = ByteArray(32).also { SecureRandom().nextBytes(it) }
        val token = Base64.encodeToString(raw, Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP)
        writePrivate(tokenFile, token)
        return token
    }

    private fun writePrivate(f: File, text: String) {
        f.writeText(text, Charsets.UTF_8)
        // 0600：先清零再只给属主 rw（此前只设写导致属主不可读的 bug 已修）
        f.setReadable(false, false); f.setWritable(false, false); f.setExecutable(false, false)
        f.setReadable(true, true); f.setWritable(true, true)
    }
}
