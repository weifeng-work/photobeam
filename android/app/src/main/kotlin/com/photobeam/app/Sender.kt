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
     * 返回 null = 未就绪（文件仍在写入/不可读）——调用方按 blocked 处理（永不丢弃 [§7.3.1]）
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

    /**
     * URI 是否**彻底失效**（MediaStore 行已被删除）[协议 §7.3]
     *
     * 与 readReady 返回 null 的另一类情况严格区分：
     *   · 行还在、文件内容没写完 → **未就绪**，永不丢弃（blocked），下轮重试；
     *   · 行没了（用户在相册里删了 / 存储被清）→ **URI 失效**，该图永远不会再就绪 → 可标 dropped。
     *
     * 判据：按 item.uri 查 MediaStore，返回 0 行即视为失效。
     * 查询抛异常一律判「未失效」——保守起见宁可继续重试，也不误丢照片。
     */
    fun uriDead(ctx: android.content.Context, item: QueueItem): Boolean {
        return try {
            ctx.contentResolver.query(
                android.net.Uri.parse(item.uri),
                arrayOf(android.provider.MediaStore.Images.Media._ID),
                null, null, null
            )?.use { c -> c.count == 0 } ?: false
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "uriDead 查询异常（按未失效处理）: ${e.message}")
            false
        }
    }

    /**
     * 图片内容完整性：JPEG SOI…EOI / PNG 头…IEND；两者都不是 → 未完成
     *
     * [Stage 15 · vivo 兼容] 判据从「末尾 8 字节内必须紧邻 EOI」改成
     * 「**文件里存在 EOI/IEND，且其后附加数据不超过限额**」。
     *
     * 为什么原来那样写：MIUI 会在截图**还没写完**时就插入 MediaStore 行，
     * 而 MediaStore 记录的 size 就是当时写到的长度 → **纯长度比对拦不住**
     * （实测截断到 130126/160387 的文件长度完全一致），只有内容判据能拦，
     * 否则用户会在电脑上粘贴出半截图。所以「内容完整」这一条必须保留。
     *
     * 为什么原来那样写会误杀：原实现假设「文件尾部就是图片结束」，
     * 只在**末尾 8 字节内**找 EOI。而部分 ROM 会在图片后面**追加私有数据**：
     *   vivo 智慧截图实测在 JPEG 的 EOI 之后追加 265 字节
     *   （b'vivo{"com.vivo.smartshot.screenshot.scen...cameraAlbum!'）
     *   → 末尾 8 字节是 "eralbum!"，没有 EOI → **每一张 vivo 截图都被判为
     *   「内容不完整」→ 永远读不出来 → 僵尸项占满队列 → 新截图发不出去**。
     *
     * 新判据同时满足两边：
     *   · 截断文件（写到一半）→ **根本没有 EOI** → 仍然被拦住 ✓
     *   · vivo 追加元数据 → EOI 存在 + 尾部附加 265B ≤ 限额 → 放行 ✓
     * 尾部附加数据上限取 1MB：正常 JPEG 的 EOI 就是最后一个标记，
     * 后面顶多跟一点元数据；超过 1MB 仍判为异常。
     */
    private fun completeImage(data: ByteArray): Boolean {
        if (data.size < 8) return false
        val jpegMagic = (data[0].toInt() and 0xFF) == 0xFF && (data[1].toInt() and 0xFF) == 0xD8
        val pngMagic = (data[0].toInt() and 0xFF) == 0x89 && data[1] == 'P'.code.toByte()
        if (pngMagic) {
            // IEND(4 字节) + CRC(4 字节) = 8 字节；找**最后一个** IEND
            val idx = lastIndexOf(data, PNG_IEND)
            if (idx < 0) return false
            return data.size - (idx + 8) <= MAX_TRAILER
        }
        if (jpegMagic) {
            // EOI = FFD9；找**最后一个**
            val idx = lastIndexOf(data, JPEG_EOI)
            if (idx < 0) return false
            return data.size - (idx + 2) <= MAX_TRAILER
        }
        return false
    }

    /** 尾部附加数据上限（字节）—— 见 completeImage 的说明 */
    private const val MAX_TRAILER = 1024 * 1024

    private val JPEG_EOI = byteArrayOf(0xFF.toByte(), 0xD9.toByte())
    private val PNG_IEND = byteArrayOf('I'.code.toByte(), 'E'.code.toByte(),
        'N'.code.toByte(), 'D'.code.toByte())

    /** 找标记的**最后一次**出现位置（-1 = 没有） */
    private fun lastIndexOf(data: ByteArray, needle: ByteArray): Int {
        val n = needle.size
        if (data.size < n) return -1
        var i = data.size - n
        while (i >= 0) {
            var hit = true
            for (j in 0 until n) {
                if (data[i + j] != needle[j]) { hit = false; break }
            }
            if (hit) return i
            i--
        }
        return -1
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
            // [Stage 7] 手机端相对目录（如 "DCIM/Screenshots/"）——PC 端按它建子目录。
            // PC 端会做完整的路径消毒（穿越/盘符/保留名），这里只做最基本的形式化。
            .put("rel_path", item.relPath.trim('/'))
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
