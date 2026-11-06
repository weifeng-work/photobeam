package com.photobeam.app

// 发送器：三路读取 [B-05/方案 B] + 发送时就绪判定与哈希 [方案 A/C] + 传输（协议 §5）
// 握手由调用方（flushQueue）统一完成；本对象只做就绪判定与单文件传输

import java.io.File

object Sender {

    /**
     * 三路读取 [B-05/方案 B]，取先成功且通过完整性判据者：
     * 1. ContentResolver.openFileDescriptor —— **首选**。content:// URI 由 MediaStore
     *    按 _id 解析，不依赖「主存储根路径」假设（SD 卡 / 第二用户 / 工作资料的图，
     *    用 /storage/emulated/0/... 直读必失败）。QueueItem.uri 已由 MediaStoreSync
     *    用 ContentUris.withAppendedId 生成（含 MediaStore _id），这里直接用它。
     * 2. openInputStream（同一 URI 的流式兜底）
     * 3. File 直读真实路径（保留作最后兜底；它假设主存储 = /storage/emulated/0，
     *    该假设在部分设备/多用户下不成立 —— 仅用于 expected 长度采样与旧路径兼容）
     * 就绪判据：
     *   ① 内容完整：按真实格式解析（JPEG 段解析 / PNG IEND / WebP RIFF / GIF trailer /
     *     BMP bfSize / HEIC 顶层 box 走查），见 completeImage
     *   ② 长度双采样 [R-5]：读前 length == 读后 length（两者皆 >0 时）——文件仍在增长则判未就绪
     *   ③ bytes 非空 且（expected 未知 或 bytes == expected）
     * 返回 null = 未就绪（文件仍在写入/不可读）——调用方按 blocked 处理（永不丢弃 [§7.3.1]）
     */
    fun readReady(ctx: android.content.Context, item: QueueItem): ByteArray? {
        // expected 只用于「长度相符」判据：优先 File 直读长度；主存储假设不成立时退化为 0
        // （= expected 未知）。此时完整性判据仍由 completeImage 兜住，不会放行半截图。
        val realPath = "/storage/emulated/0/" + item.relPath.trimEnd('/') + "/" + item.name
        val expected = try { File(realPath).length() } catch (e: Exception) { 0L }
        // 长度双采样 [R-5]：读后再采一次；增长中 → 本轮放弃
        fun stable(): Boolean {
            val after = try { File(realPath).length() } catch (e: Exception) { 0L }
            return !(expected > 0 && after > 0 && after != expected)
        }

        fun accept(data: ByteArray, via: String): ByteArray? {
            if (data.isNotEmpty() && completeImage(data) && stable() &&
                (expected <= 0L || data.size.toLong() == expected)) {
                android.util.Log.d("PhotoBeam", "readReady: $via 读 ${data.size}B via=$via uri=${item.uri}")
                return data
            }
            return null
        }

        // 1) ContentResolver file descriptor（首选；不依赖主存储根路径）
        try {
            val pfd = ctx.contentResolver.openFileDescriptor(android.net.Uri.parse(item.uri), "r")
            if (pfd != null) {
                val data = try {
                    java.io.FileInputStream(pfd.fileDescriptor).use { it.readBytes() }
                } finally {
                    try { pfd.close() } catch (e: Exception) { }
                }
                accept(data, "fd")?.let { return it }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "readReady fd 异常: ${e.message}")
        }

        // 2) ContentResolver stream
        try {
            val inp = ctx.contentResolver.openInputStream(android.net.Uri.parse(item.uri))
            if (inp != null) {
                val data = inp.use { it.readBytes() }
                accept(data, "stream")?.let { return it }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "readReady stream 异常: ${e.message}")
        }

        // 3) File 直读（最后兜底；/storage/emulated/0 只是主存储假设）
        try {
            val f = File(realPath)
            if (f.exists() && f.length() > 0) {
                val data = f.readBytes()
                accept(data, "File")?.let { return it }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "readReady File 直读异常: ${e.message}")
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
     * 图片内容完整性（按**真实格式**解析；覆盖入队白名单全部格式）。
     *
     * 两条硬要求（缺一条都会出事）：
     *  ① **截断文件必须拦住**：MIUI 会在截图还没写完时就插 MediaStore 行，而 MediaStore
     *     记录的 size 就是当时写到的长度 → 纯长度比对拦不住（实测截断到 130126/160387
     *     的文件长度完全一致），只有内容判据能拦，否则用户会在电脑上粘贴出半截图。
     *  ② **ROM 尾部追加必须放行**：vivo 智慧截图实测在 JPEG 的 EOI 之后追加 265 字节
     *     （b'vivo{"com.vivo.smartshot.screenshot.scen...cameraAlbum!'）→ 只认"文件
     *     末尾即 EOI"会把每一张 vivo 截图都判为不完整 → 永远读不出来 → 僵尸项占满队列
     *     → 新截图发不出去（vivo <PHONE-MODEL> 实测 355 张占队列）。
     *
     * 逐格式判据（放行条件尽量严，防半截文件）：
     *   JPEG  —— 段解析走 SOI→各 marker→SOS 熵编码→真 EOI；EOI 后允许 ≤MAX_TRAILER 追加。
     *            嵌入 APP1 的 EXIF 缩略图里那个 EOI 会被段长度跳过，不会误判（见 jpegComplete）。
     *   PNG   —— 存在 IEND 且尾部附加 ≤MAX_TRAILER。
     *   WebP  —— RIFF…WEBP 且 RIFF chunk size 与文件大小自洽（允许 ≤MAX_TRAILER 尾部追加）。
     *   GIF   —— GIF87a/GIF89a 且末字节为 trailer 0x3B。
     *   BMP   —— BM 且头内 bfSize == 文件大小。
     *   HEIC  —— ftyp + brand 白名单，且顶层 box 走查恰好走到文件尾。
     */
    private fun completeImage(data: ByteArray): Boolean {
        if (data.size < 8) return false
        return when {
            isJpeg(data) -> jpegComplete(data)
            isPng(data) -> pngComplete(data)
            isWebp(data) -> webpComplete(data)
            isGif(data) -> gifComplete(data)
            isBmp(data) -> bmpComplete(data)
            isHeic(data) -> heicComplete(data)
            else -> false
        }
    }

    /** 尾部附加数据上限（字节）—— 见 completeImage 的说明 */
    private const val MAX_TRAILER = 1024 * 1024

    private val PNG_IEND = byteArrayOf('I'.code.toByte(), 'E'.code.toByte(),
        'N'.code.toByte(), 'D'.code.toByte())

    /** HEIC/HEIF 顶层 ftyp brand 白名单（协议 §5.2 白名单内格式的常见 brand） */
    private val HEIC_BRANDS = setOf("heic", "heix", "hevc", "hevx", "mif1", "msf1")

    // ---------- 格式魔数 ----------

    private fun u8(b: Byte): Int = b.toInt() and 0xFF

    private fun tag(data: ByteArray, off: Int, s: String): Boolean {
        if (off < 0 || off + s.length > data.size) return false
        for (i in s.indices) if (data[off + i] != s[i].code.toByte()) return false
        return true
    }

    private fun isJpeg(data: ByteArray): Boolean =
        data.size >= 2 && u8(data[0]) == 0xFF && u8(data[1]) == 0xD8

    private fun isPng(data: ByteArray): Boolean =
        data.size >= 8 && u8(data[0]) == 0x89 && tag(data, 1, "PNG")

    private fun isWebp(data: ByteArray): Boolean =
        data.size >= 12 && tag(data, 0, "RIFF") && tag(data, 8, "WEBP")

    private fun isGif(data: ByteArray): Boolean =
        data.size >= 6 && (tag(data, 0, "GIF87a") || tag(data, 0, "GIF89a"))

    private fun isBmp(data: ByteArray): Boolean =
        data.size >= 6 && data[0] == 'B'.code.toByte() && data[1] == 'M'.code.toByte()

    private fun isHeic(data: ByteArray): Boolean =
        data.size >= 12 && tag(data, 4, "ftyp") &&
            String(data, 8, 4, Charsets.US_ASCII) in HEIC_BRANDS

    // ---------- 小端 / 大端读取 ----------

    private fun readLE32(data: ByteArray, off: Int): Long {
        if (off < 0 || off + 4 > data.size) return -1
        return (u8(data[off]).toLong() or (u8(data[off + 1]).toLong() shl 8) or
            (u8(data[off + 2]).toLong() shl 16) or (u8(data[off + 3]).toLong() shl 24))
    }

    private fun readBE32(data: ByteArray, off: Int): Long {
        if (off < 0 || off + 4 > data.size) return -1
        return ((u8(data[off]).toLong() shl 24) or (u8(data[off + 1]).toLong() shl 16) or
            (u8(data[off + 2]).toLong() shl 8) or u8(data[off + 3]).toLong())
    }

    private fun readBE64(data: ByteArray, off: Int): Long {
        if (off < 0 || off + 8 > data.size) return -1
        var v = 0L
        for (i in 0 until 8) v = (v shl 8) or u8(data[off + i]).toLong()
        return v
    }

    // ---------- PNG ----------

    private fun pngComplete(data: ByteArray): Boolean {
        if (!isPng(data)) return false
        // IEND(4 字节) + CRC(4 字节) = 8 字节；找**最后一个** IEND
        val idx = lastIndexOf(data, PNG_IEND)
        if (idx < 0) return false
        return data.size - (idx + 8) <= MAX_TRAILER
    }

    // ---------- JPEG（段解析，防 EXIF 缩略图 EOI 误判）----------

    /**
     * JPEG 完整性：**段解析**，不是「找最后一个 EOI」。
     *
     * 为什么改：旧判据「找最后一个 EOI 且其后 ≤1MB」会**放行被截断的半截图** ——
     * 带 EXIF 缩略图的 JPEG 在 APP1 段里就含一张完整小 JPEG（自带 EOI），截断文件的
     * 最后一个 EOI 到文件尾的距离 = 截断量（几百 KB，<1MB）→ 被判完整。
     * 注释里「截断文件根本没有 EOI」只在**没有嵌入缩略图**时成立。
     *
     * 新判据逐 marker 走：
     *   · SOI 起逐段解析：APPn/COM/DQT/DHT/SOF 等按段内长度跳过（EXIF 缩略图里的 EOI
     *     就是这样被跳过的，不会误判）；
     *   · SOS 之后进入熵编码扫描：`FF 00` stuff 跳 2、`FF D0-D7` RSTn 跳 2、
     *     `FF D9` 才是真正的 EOI（返回）、其它 `FF xx` = 扫描结束回段解析；
     *   · 真正的 EOI 之后允许 ≤MAX_TRAILER 的附加数据（vivo ROM 追加场景）。
     */
    private fun jpegComplete(data: ByteArray): Boolean {
        if (!isJpeg(data)) return false
        val n = data.size
        var pos = 2 // 跳过 SOI
        while (pos < n) {
            // 每个 marker 必须以 FF 开头（允许连续 FF 填充字节）
            if (u8(data[pos]) != 0xFF) return false
            while (pos < n && u8(data[pos]) == 0xFF) pos++
            if (pos >= n) return false
            val marker = u8(data[pos]); pos++
            when {
                marker == 0xD9 -> {
                    // 真正的 EOI；其后只允许 ROM 追加的少量元数据
                    return n - pos <= MAX_TRAILER
                }
                marker == 0xD8 -> { /* 重复 SOI：无长度，忽略 */ }
                marker == 0x01 -> { /* TEM：无长度 */ }
                marker in 0xD0..0xD7 -> { /* RSTn：无长度 */ }
                marker == 0x00 -> return false // 非法 marker（FF00 只应出现在熵编码数据里）
                else -> {
                    // 带 2 字节大端长度的段（长度含这 2 字节，不含 marker）
                    if (pos + 2 > n) return false
                    val len = (u8(data[pos]) shl 8) or u8(data[pos + 1])
                    if (len < 2) return false
                    val segEnd = pos + len
                    if (segEnd > n) return false
                    if (marker == 0xDA) {
                        // SOS：segEnd 之后是熵编码数据
                        pos = segEnd
                        while (true) {
                            if (pos >= n) return false // 没有 EOI → 截断
                            if (u8(data[pos]) != 0xFF) { pos++; continue }
                            if (pos + 1 >= n) return false
                            when (u8(data[pos + 1])) {
                                0x00 -> pos += 2            // FF00 = stuff 字节
                                in 0xD0..0xD7 -> pos += 2   // FFDn = RSTn
                                0xFF -> pos += 1            // 填充字节，继续找 marker
                                else -> break               // 真 marker：pos 停在 FF，交回外层
                            }
                        }
                    } else {
                        pos = segEnd
                    }
                }
            }
        }
        return false // 走完都没遇到 EOI
    }

    // ---------- WebP ----------

    private fun webpComplete(data: ByteArray): Boolean {
        if (!isWebp(data)) return false
        // RIFF chunk size（小端 u32）不含 "RIFF"+size 这 8 字节
        val declared = readLE32(data, 4) + 8L
        if (declared < 0) return false
        return declared == data.size.toLong() ||
            (declared < data.size.toLong() && data.size.toLong() - declared <= MAX_TRAILER)
    }

    // ---------- GIF ----------

    private fun gifComplete(data: ByteArray): Boolean {
        if (!isGif(data)) return false
        // GIF 以 trailer 0x3B 结束
        return u8(data[data.size - 1]) == 0x3B
    }

    // ---------- BMP ----------

    private fun bmpComplete(data: ByteArray): Boolean {
        if (!isBmp(data)) return false
        // 头内 bfSize（偏移 2，小端 u32）= 文件大小
        val bfSize = readLE32(data, 2)
        if (bfSize < 0) return false
        return bfSize == data.size.toLong()
    }

    // ---------- HEIC/HEIF ----------

    /**
     * HEIC/HEIF 完整性：顶层 box 走查（4 字节大端 size + 4 字节 type，size 含头）。
     * size==0 → 该 box 延伸到文件尾；size==1 → 真实 size 是后续 8 字节 largesize。
     * 走查必须**恰好**走到文件尾，否则（越界 / 少一截）即不完整。
     */
    private fun heicComplete(data: ByteArray): Boolean {
        if (!isHeic(data)) return false
        val total = data.size.toLong()
        var off = 0L
        while (off < total) {
            if (total - off < 8) return false
            val size32 = readBE32(data, off.toInt())
            if (size32 < 0) return false
            val boxSize: Long
            val header: Long
            when (size32) {
                0L -> { boxSize = total - off; header = 8L }   // 到文件尾
                1L -> {                                        // 64 位 largesize
                    if (total - off < 16) return false
                    header = 16L
                    boxSize = readBE64(data, off.toInt() + 8)
                }
                else -> { boxSize = size32; header = 8L }
            }
            if (boxSize < header) return false
            if (off + boxSize > total) return false
            off += boxSize
        }
        return off == total
    }

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

}

// [修复] 原 [Sender.sendData]（整段复制 SenderService.flushQueue 的发送逻辑）是死代码：
// grep 全仓库零调用点，留着必然与 flushQueue 漂移 → 删除。发送路径唯一实现在
// SenderService.flushQueue（协议 §4/§5）。

class SendException(val errClass: String, val reason: String) : Exception(reason)
