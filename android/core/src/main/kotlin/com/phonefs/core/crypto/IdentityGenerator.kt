package com.phonefs.core.crypto

import org.bouncycastle.asn1.x500.X500Name
import org.bouncycastle.asn1.x509.BasicConstraints
import org.bouncycastle.asn1.x509.Extension
import org.bouncycastle.cert.jcajce.JcaX509CertificateConverter
import org.bouncycastle.cert.jcajce.JcaX509v3CertificateBuilder
import org.bouncycastle.operator.jcajce.JcaContentSignerBuilder
import org.bouncycastle.util.io.pem.PemObject
import org.bouncycastle.util.io.pem.PemWriter
import java.io.StringWriter
import java.math.BigInteger
import java.security.KeyPairGenerator
import java.security.MessageDigest
import java.security.SecureRandom
import java.security.cert.X509Certificate
import java.security.spec.ECGenParameterSpec
import java.util.Date

/**
 * §7.3：ECDSA P-256 自签 X.509 v3 证书（CN=deviceId，SKI，NotAfter 10 年，SHA256withECDSA）。
 * 证书构建用 BouncyCastle bcpkix（SPEC §2.2），ECDSA/SHA 走 JCA（Conscrypt/系统 Provider）。
 */
object IdentityGenerator {

    private const val VALIDITY_YEARS = 10L

    fun generate(deviceId: String, now: Date = Date()): Identity {
        val kpg = KeyPairGenerator.getInstance("EC")
        kpg.initialize(ECGenParameterSpec("secp256r1"), SecureRandom())
        val kp = kpg.generateKeyPair()

        val notBefore = Date(now.time - 60_000L)
        val notAfter = Date(now.time + VALIDITY_YEARS * 365L * 24L * 3600_000L)
        val serial = BigInteger(63, SecureRandom()) // 63bit 正数
        val subject = X500Name("CN=$deviceId")

        val builder = JcaX509v3CertificateBuilder(
            subject,
            serial,
            notBefore,
            notAfter,
            subject,
            kp.public,
        )
        // SKI（SPEC：随机 DeviceID 语义由 deviceId 承担，SKI 用公钥派生，X.509 标准做法）
        builder.addExtension(
            Extension.subjectKeyIdentifier,
            false,
            org.bouncycastle.cert.jcajce.JcaX509ExtensionUtils().createSubjectKeyIdentifier(kp.public),
        )
        builder.addExtension(Extension.basicConstraints, true, BasicConstraints(false))

        val signer = JcaContentSignerBuilder("SHA256withECDSA").build(kp.private)
        val holder = builder.build(signer)
        val cert: X509Certificate = JcaX509CertificateConverter().getCertificate(holder)

        val fingerprint = MessageDigest.getInstance("SHA-256")
            .digest(cert.encoded)
            .joinToString("") { "%02x".format(it) }

        return Identity(
            certPem = toPem("CERTIFICATE", cert.encoded),
            keyPem = toPem("PRIVATE KEY", kp.private.encoded),
            fingerprint = fingerprint,
        )
    }

    internal fun toPem(type: String, der: ByteArray): String {
        val sw = StringWriter()
        PemWriter(sw).use { it.writeObject(PemObject(type, der)) }
        return sw.toString()
    }
}
