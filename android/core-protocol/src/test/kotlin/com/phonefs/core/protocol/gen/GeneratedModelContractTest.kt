package com.phonefs.core.protocol.gen

import kotlinx.serialization.json.Json
import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue

/** 生成模型必须按线上 wire 形态解码：错误 details 为任意 JSON 对象，media_type 为 JSON 数字。 */
class GeneratedModelContractTest {
    private val json = Json { ignoreUnknownKeys = true }

    @Test
    fun decodeErrorEnvelopeWithDetails() {
        val raw = """{"error":{"code":"INVALID_ARGUMENT","message":"x","trace_id":"t","retriable":false,"details":{"received":12,"arr":[1,2]}}}"""
        val decoded = json.decodeFromString<ErrorEnvelope>(raw)
        assertEquals(12, decoded.error.details!!["received"].toString().toInt())
        assertTrue(json.encodeToString(decoded).contains("received"))
    }

    @Test
    fun decodeAttrMediaTypeAsJsonNumber() {
        val raw = """{"id":"p:eA","name":"a","is_dir":false,"size":1,"modified_ms":2,"writable":true,"media_type":1}"""
        val decoded = json.decodeFromString<Attr>(raw)
        assertEquals(1, decoded.mediaType)
    }
}
