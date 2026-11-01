package com.phonefs.core.net

import com.phonefs.core.crypto.Identity
import org.bouncycastle.openssl.PEMParser
import org.bouncycastle.openssl.jcajce.JcaPEMKeyConverter
import org.bouncycastle.util.io.pem.PemObject
import java.io.StringReader
import java.security.KeyStore
import java.security.cert.CertificateFactory

/** Identity(cert+PEM key) → JCE KeyStore（Netty TLS / SSLServerSocket 共用）。 */
internal object TlsKeyStores {
    private const val KEY_ALIAS = "phonefs"
    val PASSWORD: CharArray = "phonefs-w2".toCharArray()

    fun build(identity: Identity): KeyStore {
        val cf = CertificateFactory.getInstance("X.509")
        val cert = cf.generateCertificate(identity.certPem.byteInputStream())
        // PEMParser.readObject() 对 PKCS#8「PRIVATE KEY」直接返回 ASN.1 PrivateKeyInfo
        // （强转 PemObject 会 ClassCastException，F-W2-1 真机首启即崩的教训）。
        val key = PEMParser(StringReader(identity.keyPem)).use { parser ->
            when (val obj = parser.readObject()) {
                is org.bouncycastle.asn1.pkcs.PrivateKeyInfo ->
                    JcaPEMKeyConverter().getPrivateKey(obj)
                is PemObject ->
                    JcaPEMKeyConverter().getPrivateKey(
                        org.bouncycastle.asn1.pkcs.PrivateKeyInfo.getInstance(obj.content))
                else -> error("unexpected PEM object for private key: ${obj?.javaClass}")
            }
        }
        return KeyStore.getInstance("PKCS12").apply {
            load(null, null)
            setKeyEntry(KEY_ALIAS, key, PASSWORD, arrayOf(cert))
        }
    }
}
