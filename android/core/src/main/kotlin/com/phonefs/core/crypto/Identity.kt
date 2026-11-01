package com.phonefs.core.crypto

/** §7.3 身份：ECDSA P-256 自签证书 + PKCS#8 私钥（PEM），fingerprint=SHA-256(DER) hex。 */
data class Identity(
    val certPem: String,
    val keyPem: String,
    val fingerprint: String,
)
