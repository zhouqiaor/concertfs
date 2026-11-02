package com.phonefs.core.protocol

import io.ktor.http.HttpStatusCode

/** §6.6 冻结错误码（W2 仅实现子集）。 */
object ErrCode {
    const val INVALID_ARGUMENT = "INVALID_ARGUMENT"
    const val BAD_CURSOR = "BAD_CURSOR"
    const val TOKEN_INVALID = "TOKEN_INVALID"
    const val BIND_REFUSED = "BIND_REFUSED"
    const val NOT_FOUND = "NOT_FOUND"
    const val NOT_SUPPORTED = "NOT_SUPPORTED"
    const val NOT_READY = "NOT_READY"
    const val RANGE_NOT_SATISFIABLE = "RANGE_NOT_SATISFIABLE"
    const val INTERNAL = "INTERNAL"
    const val INVALID_NAME = "INVALID_NAME"
    const val ALREADY_EXISTS = "ALREADY_EXISTS"
    const val NOT_EMPTY = "NOT_EMPTY"
    const val UPLOAD_INCOMPLETE = "UPLOAD_INCOMPLETE"
    const val PRECONDITION_FAILED = "PRECONDITION_FAILED"
    const val PARTIAL = "PARTIAL"
    const val INSUFFICIENT_STORAGE = "INSUFFICIENT_STORAGE"
}

class PhoneFsException(
    val code: String,
    val status: HttpStatusCode,
    message: String,
    val retriable: Boolean = false,
    val details: Map<String, Any>? = null,
) : RuntimeException(message)
