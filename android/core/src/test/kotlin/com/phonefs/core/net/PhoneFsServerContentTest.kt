package com.phonefs.core.net

import com.phonefs.core.crypto.Identity
import com.phonefs.core.fs.CapacityInfo
import com.phonefs.core.fs.FileInfo
import com.phonefs.core.fs.FsBackend
import io.ktor.server.cio.CIO
import io.ktor.server.engine.EmbeddedServer
import io.ktor.server.engine.embeddedServer
import kotlinx.coroutines.runBlocking
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.BufferedInputStream
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.FileInputStream
import java.net.InetSocketAddress
import java.net.Socket
import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.nio.file.Files
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.util.Locale
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

class PhoneFsServerContentTest {
    private var tempDir: File? = null
    private var engine: EmbeddedServer<*, *>? = null
    private val client = HttpClient.newHttpClient()

    @After
    fun tearDown() {
        engine?.stop(1_000, 1_000)
        tempDir?.deleteRecursively()
    }

    @Test
    fun fullResponseStreamsAcrossBuffersAndPreservesHeaders() {
        val data = bytes(BUFFER_SIZE * 2 + 123)
        val backend = backendFor(data)
        val port = start(backend)

        val response = get(port)

        assertEquals(200, response.statusCode())
        assertArrayEquals(data, response.body())
        assertCommonHeaders(response, data.size.toLong())
        assertEquals(null, response.headers().firstValue("Content-Range").orElse(null))
        assertEquals(BUFFER_SIZE, backend.streams.single().maxRequested)
        assertTrue(backend.streams.single().closed.await(1, TimeUnit.SECONDS))
    }

    @Test
    fun rangeFirstLastAndAcrossBufferBoundaries() {
        val data = bytes(BUFFER_SIZE * 3 + 17)
        val backend = backendFor(data)
        val port = start(backend)

        val first = get(port, range = "bytes=0-4")
        val last = get(port, range = "bytes=-5")
        val cross = get(port, range = "bytes=${BUFFER_SIZE - 6}-${BUFFER_SIZE * 2 + 5}")

        assertPartial(first, data, 0, 4)
        assertPartial(last, data, data.size - 5, data.size - 1)
        assertPartial(cross, data, BUFFER_SIZE - 6, BUFFER_SIZE * 2 + 5)
        assertEquals(listOf(0L, data.size - 5L, BUFFER_SIZE - 6L), backend.offsets)
        assertTrue(backend.streams.all { it.maxRequested <= BUFFER_SIZE })
    }

    @Test
    fun emptyFileHasZeroLengthAndRangeIsUnsatisfiable() {
        val backend = backendFor(ByteArray(0))
        val port = start(backend)

        val full = get(port)
        val range = get(port, range = "bytes=0-")

        assertEquals(200, full.statusCode())
        assertEquals(0, full.body().size)
        assertEquals("0", full.headers().firstValue("Content-Length").orElse(null))
        assertCommonHeaders(full, 0)
        assertEquals(416, range.statusCode())
        assertEquals("bytes */0", range.headers().firstValue("Content-Range").orElse(null))
        assertEquals(1, backend.streams.size)
        assertTrue(backend.streams.single().closed.await(1, TimeUnit.SECONDS))
    }

    @Test
    fun ifRangeMatchSelectsPartialAndMismatchFallsBackToFull() {
        val data = bytes(100)
        val backend = backendFor(data)
        val port = start(backend)
        val etag = "\"${data.size}-$MODIFIED_MS\""

        val match = get(port, range = "bytes=10-19", ifRange = etag)
        val mismatch = get(port, range = "bytes=10-19", ifRange = "\"stale\"")

        assertPartial(match, data, 10, 19)
        assertEquals(200, mismatch.statusCode())
        assertArrayEquals(data, mismatch.body())
        assertEquals(null, mismatch.headers().firstValue("Content-Range").orElse(null))
        assertCommonHeaders(mismatch, data.size.toLong())
    }

    @Test
    fun unsatisfiableRangeRetains416AndContentRange() {
        val backend = backendFor(bytes(10))
        val port = start(backend)

        val response = get(port, range = "bytes=10-20")

        assertEquals(416, response.statusCode())
        assertEquals("bytes */10", response.headers().firstValue("Content-Range").orElse(null))
        assertEquals("bytes", response.headers().firstValue("Accept-Ranges").orElse(null))
        assertTrue(String(response.body()).contains("RANGE_NOT_SATISFIABLE"))
        assertTrue(backend.streams.isEmpty())
    }

    @Test
    fun shortSourceNeverReportsSuccess() {
        val actual = bytes(100)
        val declared = BUFFER_SIZE + 500L
        val backend = backendFor(actual, declaredSize = declared)
        val port = start(backend)

        // 引擎无关的不变量：源比声明长度短时，绝不能产出「200 + 完整 Content-Length」的成功响应。
        // CIO 在 writer 异常时会整体中止连接（客户端在收到任何首部前 EOF）；Netty 之前路径可先
        // 刷出首部+短前缀再以 EOF 结束。两种都是安全的——都不会伪造成功，也不会让客户端无限阻塞。
        var sawSuccessWithFullBody = false
        Socket().use { socket ->
            socket.connect(InetSocketAddress("127.0.0.1", port), 2_000)
            socket.soTimeout = 5_000
            socket.getOutputStream().write(requestBytes(port))
            socket.getOutputStream().flush()
            val input = BufferedInputStream(socket.getInputStream())
            val header = try {
                readHeaders(input)
            } catch (_: IllegalStateException) {
                "" // CIO：首部前即 EOF（整体中止），直接判为安全失败。
            }
            val body = if (header.isNotEmpty()) input.readBytes() else ByteArray(0)
            if (header.startsWith("HTTP/1.1 200") && header.contains("Content-Length: $declared")) {
                if (body.size >= declared) sawSuccessWithFullBody = true
            }
        }
        assertFalse("short source must not produce a successful full-length response", sawSuccessWithFullBody)
        assertTrue(backend.streams.single().closed.await(1, TimeUnit.SECONDS))
    }

    @Test
    fun clientCancelAfterFirstChunkClosesStreamWithFixedBufferAndLongLength() {
        val declared = Int.MAX_VALUE.toLong() + BUFFER_SIZE + 17L
        val file = newFile()
        val backend = FakeBackend(
            file = file,
            declaredSize = declared,
            streamFactory = { _, offset -> GeneratedFileInputStream(file, offset, delayMs = 10) },
        )
        val port = start(backend)

        Socket().use { socket ->
            socket.connect(InetSocketAddress("127.0.0.1", port), 2_000)
            socket.soTimeout = 5_000
            socket.getOutputStream().write(requestBytes(port))
            socket.getOutputStream().flush()
            val input = BufferedInputStream(socket.getInputStream())
            val header = readHeaders(input)
            assertTrue(header.startsWith("HTTP/1.1 200"))
            assertTrue(header.contains("Content-Length: $declared"))
            assertEquals(BUFFER_SIZE, input.readNBytes(BUFFER_SIZE).size)
            // use 关闭 socket；服务端不能继续读取这个 >2GB 的逻辑响应。
        }

        val stream = backend.streams.single() as GeneratedFileInputStream
        assertTrue("source was not closed after client cancellation", stream.closed.await(5, TimeUnit.SECONDS))
        assertTrue("server kept generating after cancellation: ${stream.bytesRead}", stream.bytesRead < declared)
        assertTrue("buffer exceeded 256KiB: ${stream.maxRequested}", stream.maxRequested <= BUFFER_SIZE)
        assertTrue("cancellation test did not stream a bounded prefix", stream.bytesRead <= 64L * BUFFER_SIZE)
    }

    @Test
    fun realCIOSlowConsumerBoundsSourceReadsAndReadAhead() {
        val declared = Int.MAX_VALUE.toLong() + BUFFER_SIZE + 17L
        val file = newFile()
        val backend = FakeBackend(
            file = file,
            declaredSize = declared,
            streamFactory = { _, offset -> GeneratedFileInputStream(file, offset, delayMs = 0) },
        )
        val port = start(backend)

        Socket().use { socket ->
            // 小接收缓冲 + 只读 1KiB：让 TCP/CIO 背压回到逻辑流，而非预先消费整个响应。
            socket.receiveBufferSize = 16 * 1024
            socket.connect(InetSocketAddress("127.0.0.1", port), 2_000)
            socket.soTimeout = 5_000
            socket.getOutputStream().write(requestBytes(port))
            socket.getOutputStream().flush()
            val input = BufferedInputStream(socket.getInputStream())
            val header = readHeaders(input)
            assertTrue(header.startsWith("HTTP/1.1 200"))
            assertTrue(header.contains("Content-Length: $declared"))
            val consumed = input.readNBytes(1024).size.toLong()
            assertEquals(1024L, consumed)

            val stream = backend.streams.single() as GeneratedFileInputStream
            val stalled = stream.awaitReadAheadStall()
            assertTrue("source did not start or did not reach backpressure", stalled)
            val bytesRead = stream.bytesRead
            val readCount = stream.readCount
            val readAhead = bytesRead - consumed
            assertTrue(
                "slow consumer allowed $readCount source reads (bytes=$bytesRead)",
                readCount in 1..MAX_SLOW_CONSUMER_READS,
            )
            assertTrue(
                "slow consumer allowed $readAhead bytes of read-ahead after $readCount reads",
                readAhead in 0..MAX_SLOW_CONSUMER_READ_AHEAD,
            )
            assertFalse("large logical response was consumed eagerly", bytesRead >= declared)
        }

        val stream = backend.streams.single() as GeneratedFileInputStream
        assertTrue("source was not closed after slow consumer left", stream.closed.await(5, TimeUnit.SECONDS))
    }

    private fun GeneratedFileInputStream.awaitReadAheadStall(): Boolean {
        val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5)
        var previousBytes = -1L
        var stableSince = 0L
        while (System.nanoTime() < deadline) {
            val current = bytesRead
            val now = System.nanoTime()
            if (current != previousBytes) {
                previousBytes = current
                stableSince = now
            } else if (current >= 4L * BUFFER_SIZE &&
                now - stableSince >= TimeUnit.MILLISECONDS.toNanos(300)
            ) {
                return true
            }
            Thread.sleep(20)
        }
        return false
    }

    private fun assertPartial(response: HttpResponse<ByteArray>, data: ByteArray, first: Int, last: Int) {
        assertEquals(206, response.statusCode())
        assertArrayEquals(data.copyOfRange(first, last + 1), response.body())
        assertCommonHeaders(response, data.size.toLong())
        assertEquals(
            "bytes $first-$last/${data.size}",
            response.headers().firstValue("Content-Range").orElse(null),
        )
    }

    private fun assertCommonHeaders(response: HttpResponse<ByteArray>, size: Long) {
        assertEquals(
            response.body().size.toString(),
            response.headers().firstValue("Content-Length").orElse(null),
        )
        assertEquals("\"$size-$MODIFIED_MS\"", response.headers().firstValue("ETag").orElse(null))
        assertEquals("bytes", response.headers().firstValue("Accept-Ranges").orElse(null))
        assertEquals(HTTP_DATE, response.headers().firstValue("Last-Modified").orElse(null))
    }

    private fun get(port: Int, range: String? = null, ifRange: String? = null): HttpResponse<ByteArray> {
        val builder = HttpRequest.newBuilder(URI.create("http://127.0.0.1:$port/v1/fs/content?id=stor:primary"))
            .header("Authorization", "Bearer $TOKEN")
        range?.let { builder.header("Range", it) }
        ifRange?.let { builder.header("If-Range", it) }
        return client.send(builder.GET().build(), HttpResponse.BodyHandlers.ofByteArray())
    }

    private fun requestBytes(port: Int): ByteArray = (
        "GET /v1/fs/content?id=stor:primary HTTP/1.1\r\n" +
            "Host: 127.0.0.1:$port\r\n" +
            "Authorization: Bearer $TOKEN\r\n" +
            "Connection: keep-alive\r\n\r\n"
        ).toByteArray()

    private fun readHeaders(input: BufferedInputStream): String {
        val out = ByteArrayOutputStream()
        var matched = 0
        while (matched < 4) {
            val b = input.read()
            check(b >= 0) { "EOF before response headers" }
            out.write(b)
            matched = if (b == HEADER_END[matched].toInt()) matched + 1 else 0
        }
        return String(out.toByteArray())
    }

    private fun backendFor(data: ByteArray, declaredSize: Long = data.size.toLong()): FakeBackend {
        val file = newFile()
        return FakeBackend(file, declaredSize) { _, offset ->
            BytesFileInputStream(file, data, offset.toInt())
        }
    }

    private fun newFile(): File {
        val dir = tempDir ?: Files.createTempDirectory("phonefs-content-test").toFile().also { tempDir = it }
        return File(dir, "payload.bin").apply { writeBytes(ByteArray(0)) }
    }

    private fun start(backend: FsBackend): Int {
        val server = PhoneFsServer(
            backend = backend,
            identity = Identity("", "", ""),
            deviceId = "test-device",
            deviceName = "test-device",
            deviceFormFactor = "phone",
            token = TOKEN,
        )
        val started = embeddedServer(CIO, host = "127.0.0.1", port = 0) {
            with(server) { configure() }
        }.start(wait = false)
        engine = started
        return runBlocking { started.engine.resolvedConnectors().single().port }
    }

    private class FakeBackend(
        file: File,
        declaredSize: Long,
        private val streamFactory: (String, Long) -> TrackingFileInputStream,
    ) : FsBackend {
        override val storageRoot: File = file
        private val info = FileInfo(
            path = file.absolutePath,
            name = file.name,
            isDir = false,
            size = declaredSize,
            modifiedMs = MODIFIED_MS,
            mime = "application/octet-stream",
            writable = false,
        )
        val offsets = mutableListOf<Long>()
        val streams = mutableListOf<TrackingFileInputStream>()

        override fun ready(): Boolean = true
        override fun statFs(): CapacityInfo = CapacityInfo(1, 1, 4096)
        override fun list(path: String): List<FileInfo> = emptyList()
        override fun stat(path: String): FileInfo? = info.takeIf { path == it.path }
        override fun openInput(path: String, offset: Long): FileInputStream? {
            offsets += offset
            return streamFactory(path, offset).also { streams += it }
        }
        override fun isInsideStorage(path: String): Boolean = true
        override fun scanFile(path: String) = Unit
    }

    private abstract class TrackingFileInputStream(file: File) : FileInputStream(file) {
        val closed = CountDownLatch(1)
        var maxRequested = 0
            private set

        protected fun recordRequest(len: Int) {
            maxRequested = maxOf(maxRequested, len)
        }

        override fun close() {
            super.close()
            closed.countDown()
        }
    }

    private class BytesFileInputStream(
        file: File,
        private val data: ByteArray,
        offset: Int,
    ) : TrackingFileInputStream(file) {
        private var position = offset

        override fun read(buffer: ByteArray, offset: Int, len: Int): Int {
            recordRequest(len)
            if (position >= data.size) return -1
            val count = minOf(len, data.size - position)
            System.arraycopy(data, position, buffer, offset, count)
            position += count
            return count
        }

        override fun read(): Int = if (position < data.size) data[position++].toInt() and 0xff else -1
    }

    private class GeneratedFileInputStream(
        file: File,
        offset: Long,
        private val delayMs: Long,
    ) : TrackingFileInputStream(file) {
        private var position = offset
        @Volatile var bytesRead = 0L
            private set
        @Volatile var readCount = 0
            private set

        override fun read(buffer: ByteArray, offset: Int, len: Int): Int {
            recordRequest(len)
            if (delayMs > 0) Thread.sleep(delayMs)
            for (i in 0 until len) buffer[offset + i] = ((position + i) * 31).toByte()
            position += len
            bytesRead += len
            readCount++
            return len
        }

        override fun read(): Int {
            position++
            bytesRead++
            readCount++
            return 0
        }
    }

    companion object {
        private const val BUFFER_SIZE = 256 * 1024
        private const val MAX_SLOW_CONSUMER_READS = 32
        private const val MAX_SLOW_CONSUMER_READ_AHEAD = MAX_SLOW_CONSUMER_READS.toLong() * BUFFER_SIZE
        private const val TOKEN = "content-test-token"
        private const val MODIFIED_MS = 1_700_000_000_123L
        private val HTTP_DATE = DateTimeFormatter.RFC_1123_DATE_TIME
            .withLocale(Locale.US)
            .format(java.time.Instant.ofEpochMilli(MODIFIED_MS).atZone(ZoneOffset.UTC))
        private val HEADER_END = byteArrayOf(13, 10, 13, 10)

        private fun bytes(size: Int): ByteArray = ByteArray(size) { i -> (i * 31 + 7).toByte() }
    }
}
