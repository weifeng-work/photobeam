package com.photobeam.app

// 发送器：三路读取 [B-05/方案 B] + 发送时就绪判定与哈希 [方案 A/C] + 传输（协议 §5）
// 握手由调用方（flushQueue）统一完成；本对象只做就绪判定与单文件传输

import java.io.File
import java.net.Socket

object Sender {

    /**
     * 三路读取 [B-05/方案 B]，取先成功且通过完整性判据者：
     * 1. File 直读真实路径（绕过 MediaProvider；Android 11+ 允许，但 MIUI 上重装后实测可能
     *    EACCES——此时由 fd/stream 兜底，注释与实测一致）
     * 2. openFileDescriptor("r")
     * 3. openInputStream
     * 就绪判据：
     *   ① 内容完整：JPEG 末尾 EOI(FFD9) / PNG 末尾 IEND——MIUI 会在文件写完前就插
     *     MediaStore 行（实测截断 130126/160387 通过了纯长度比对，仅内容判据能拦住）
     *   ② 长度双采样 [R-5]：读前 length == 读后 length（两者皆 >0 时）——文件仍在增长则判未就绪
     *   ③ bytes 非空 且（expected 未知 或 bytes == expected）
     * 返回 null = 未就绪（文件仍在写入/不可读）——调用方按 blocked 处理（永不丢弃 [§6.1]）
     */
    fun readReady(ctx: android.content.Context, item: QueueItem): ByteArray? {
        val realPath = "/storage/emulated/0/" + item.relPath.trimEnd('/') + "/" + item.name
        val expected = try { File(realPath).length() } catch (e: Exception) { 0L }
        // 长度双采样 [R-5]：读后再采一次；增长中 → 本轮放弃
        fun stable(): Boolean {
            val after = try { File(realPath).length() } catch (e: Exception) { 0L }
            return !(expected > 0 && after > 0 && after != expected)
        }

        // 1) File 直读
        try {
            val f = File(realPath)
            if (f.exists() && f.length() > 0) {
                val data = f.readBytes()
                if (completeImage(data) && stable() &&
                    (expected <= 0L || data.size.toLong() == expected)) {
                    android.util.Log.d("PhotoBeam", "readReady: File 直读 ${data.size}B via=File")
                    return data
                }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "readReady File 直读异常: ${e.message}")
        }

        // 2) ContentResolver file descriptor
        try {
            val pfd = ctx.contentResolver.openFileDescriptor(android.net.Uri.parse(item.uri), "r")
            if (pfd != null) {
                val data = java.io.FileInputStream(pfd.fileDescriptor).use { it.readBytes() }
                pfd.close()
                if (completeImage(data) && stable() &&
                    (expected <= 0L || data.size.toLong() == expected)) {
                    android.util.Log.d("PhotoBeam", "readReady: fd 读 ${data.size}B via=fd")
                    return data
                }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "readReady fd 异常: ${e.message}")
        }

        // 3) ContentResolver stream
        try {
            val inp = ctx.contentResolver.openInputStream(android.net.Uri.parse(item.uri))
            if (inp != null) {
                val data = inp.use { it.readBytes() }
                if (completeImage(data) && stable() &&
                    (expected <= 0L || data.size.toLong() == expected)) {
                    android.util.Log.d("PhotoBeam", "readReady: stream 读 ${data.size}B via=stream")
                    return data
                }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "readReady stream 异常: ${e.message}")
        }
        android.util.Log.w("PhotoBeam", "readReady: 三路均未就绪 (${item.name})")
        return null
    }

    /** 图片内容完整性：JPEG SOI…EOI / PNG 头…IEND；两者都不是 → 未完成 */
    private fun completeImage(data: ByteArray): Boolean {
        if (data.size < 8) return false
        val jpegMagic = (data[0].toInt() and 0xFF) == 0xFF && (data[1].toInt() and 0xFF) == 0xD8
        val pngMagic = (data[0].toInt() and 0xFF) == 0x89 && data[1] == 'P'.code.toByte()
        if (pngMagic) {
            val n = data.size
            return n >= 8 && data[n - 8] == 'I'.code.toByte() && data[n - 7] == 'E'.code.toByte() &&
                data[n - 6] == 'N'.code.toByte() && data[n - 5] == 'D'.code.toByte()
        }
        if (jpegMagic) {
            // EOI(FF D9)，容忍末尾少量 padding（实测 MIUI 无，但扫描 8 字节更稳）
            var i = data.size - 2
            while (i >= data.size - 8 && i >= 0) {
                if ((data[i].toInt() and 0xFF) == 0xFF && (data[i + 1].toInt() and 0xFF) == 0xD9) return true
                i--
            }
            return false
        }
        return false
    }

    /** 传输单张图（连接与握手已由 flushQueue 建立独立链路）[协议 §5] */
    fun sendData(
        sock: Socket,
        ch: SecureChannel,
        item: QueueItem,
        data: ByteArray
    ) {
        val out = sock.getOutputStream()
        val inp = sock.getInputStream()

        // file_meta [协议 §5.1]
        ch.writeFrame(out, FRAME_CTRL, org.json.JSONObject()
            .put("type", "file_meta")
            .put("file_id", item.fileId)
            .put("name", item.name)
            .put("size", item.size)
            .put("mtime", item.capturedAt)
            .put("mime", item.mime)
            .put("captured_at", item.capturedAt.toString())
            .put("batch_id", item.batchId)
            .put("is_latest_in_batch", item.isLatestInBatch)
            .put("batch_size", item.batchSize)
            .toString().toByteArray())

        // chunks（256KiB，从内存 data 直发——内容与 file_id 同源 [方案 A]）
        var off = 0
        while (off < data.size) {
            val n = minOf(256 * 1024, data.size - off)
            ch.writeFrame(out, FRAME_FILE, data.copyOfRange(off, off + n))
            off += n
        }

        // file_end
        ch.writeFrame(out, FRAME_CTRL, org.json.JSONObject()
            .put("type", "file_end").put("file_id", item.fileId)
            .toString().toByteArray())

        // ack
        val (ft, pt) = ch.readFrame(inp)
        require(ft == FRAME_CTRL) { "ack 帧类型异常" }
        val ack = org.json.JSONObject(String(pt, Charsets.UTF_8))
        require(ack.optString("type") == "ack") { "预期 ack" }
        val status = ack.optString("status")
        val errClass = ack.optString("err_class", "transient")
        val reason = ack.optString("reason")
        if (status != "ok") {
            throw SendException(errClass, reason)
        }
    }
}

class SendException(val errClass: String, val reason: String) : Exception(reason)
