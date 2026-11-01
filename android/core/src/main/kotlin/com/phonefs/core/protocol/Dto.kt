package com.phonefs.core.protocol

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/** §6.4 GET /v1/pair/info 载荷（W2：device 为附加字段，容忍未知字段前向兼容）。 */
@Serializable
data class PairInfo(
    val service: String,
    @SerialName("protocol_versions") val protocolVersions: List<Int>,
    val fingerprint: String,
    @SerialName("pairing_open") val pairingOpen: Boolean,
    @SerialName("server_time_ms") val serverTimeMs: Long,
    val device: PairDeviceInfo? = null,
)

@Serializable
data class PairDeviceInfo(
    val id: String,
    val name: String,
    val type: String,
)

/** §6.2 Attribute JSON，字段名严格对齐 SPEC（snake_case）。 */
@Serializable
data class Attr(
    val id: String,
    val parent: String? = null,
    val name: String,
    @SerialName("is_dir") val isDir: Boolean,
    val size: Long,
    @SerialName("modified_ms") val modifiedMs: Long,
    val mime: String? = null,
    @SerialName("media_type") val mediaType: String? = null,
    val writable: Boolean,
)

@Serializable
data class RootsResp(val roots: List<Attr>)

/** §6.5.2：next_cursor W2 恒为 ""（一次性返回，见报告简化项）。 */
@Serializable
data class ListResp(
    val entries: List<Attr>,
    @SerialName("next_cursor") val nextCursor: String,
    val revision: Long,
)

/** §6.5.10。 */
@Serializable
data class StatFsResp(
    @SerialName("total_bytes") val totalBytes: Long,
    @SerialName("free_bytes") val freeBytes: Long,
    @SerialName("block_size") val blockSize: Long,
)

/** §6.5 通用错误包络。 */
@Serializable
data class ErrorEnvelope(val error: ErrBody)

@Serializable
data class ErrBody(
    val code: String,
    val message: String,
    @SerialName("trace_id") val traceId: String,
    val retriable: Boolean,
    val details: Map<String, String>? = null,
)
