package com.photobeam.app

// 图库捕获：ContentObserver 推送 + 对账兜底（协议 §7.1/§7.2）
// 发现靠查询（_ID 水位）、去重靠账本
// 方案 A [B-04]：入队零 IO —— 只记元数据，不读文件内容（内容哈希在发送时计算，见 Sender.kt）
// _id 为 files 表主键（全局唯一、单调递增），单一全局水位即可覆盖全部卷 [P2-b 说明]

import android.content.ContentUris
import android.content.Context
import android.database.ContentObserver
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.provider.MediaStore

class MediaStoreSync(
    private val ctx: Context,
    private val queue: QueueStore,
    private val onChange: () -> Unit
) {
    private var observer: ContentObserver? = null

    /**
     * [Stage 6] 本轮对账是否遇到过 is_pending=1 的行（文件还没写完）。
     * 上层据此安排一次**延迟重扫**兜底——正常情况下 MediaStore 把 is_pending
     * 清零时会再次触发 ContentObserver，这条兜底是为了防止某些 ROM
     * 不通知 UPDATE 变更时该图永远补不回来。
     */
    @Volatile var sawPendingRow: Boolean = false
        private set

    fun register() {
        val o = object : ContentObserver(Handler(Looper.getMainLooper())) {
            override fun onChange(selfChange: Boolean, uri: Uri?) {
                if (uri != null && uri.toString().contains("images")) {
                    onChange()
                }
            }
        }
        ctx.contentResolver.registerContentObserver(
            MediaStore.Images.Media.EXTERNAL_CONTENT_URI, true, o)
        observer = o
    }

    fun unregister() {
        observer?.let { ctx.contentResolver.unregisterContentObserver(it) }
        observer = null
    }

    /**
     * 对账：_id 水位扫描 → 新图元数据入队（零 IO [方案 A/B-04]）。
     * file_id 留空，由发送时就绪判定补齐（Sender.readReady）。
     * 发现账本在入队时即记 _ID（发现去重）；水位照常推进（失败项走待重试路径，与水位无关 [B-11 §7.2]）。
     * 返回新增数。
     */
    fun reconcile(): Int {
        var added = 0
        sawPendingRow = false
        val uri = MediaStore.Images.Media.EXTERNAL_CONTENT_URI
        val proj = arrayOf("_id", "volume_name", "_display_name", "_size", "mime_type",
            "date_added", "relative_path", "is_pending")
        // 只取 _id > lastMaxId 的行 [P0-09]
        //
        // [Stage 6 · 缺陷修复] is_pending 的处理方式改了：
        // 原测现在 SQL 的 WHERE 里过滤 `is_pending = 0`，**但水位仍会被后面已就绪的行推过去**。
        // 实测（<PHONE-MODEL> / MIUI，2026-11-05）：截图落库瞬间 is_pending=1，
        // 若此时后续 _id 的行已就绪，水位就会越过这张 pending 图 → 它永远落在水位之下，
        // 再也扫不到 → **该照片静默丢失**（实测 10 轮里丢 2 张）。
        //
        // 修法：**水位不得越过任何 pending 行**。pending 行本身这轮不入队，
        // 等 MediaStore 把 is_pending 清零后由 ContentObserver 再次触发对账补上。
        val sel = "_id > ?"
        val args = arrayOf(queue.lastMaxId.toString())
        var upper = queue.lastMaxId
        // pendingBarrier：遇到的第一个 pending 行的 _id。水位不得越过它。
        var pendingBarrier = -1L
        try {
            ctx.contentResolver.query(uri, proj, sel, args, "_id ASC")?.use { c ->
                while (c.moveToNext()) {
                    val id = c.getLong(0)
                    if (id <= queue.lastMaxId) continue
                    // [Stage 6 · 缺陷修复] is_pending=1 = 文件还没写完。
                    // 这行这轮不入队；且**水位不能越过它**（否则该图永久丢失）。
                    val pending = if (c.isNull(7)) false else c.getInt(7) != 0
                    if (pending) {
                        if (pendingBarrier < 0) pendingBarrier = id
                        sawPendingRow = true
                        continue
                    }
                    if (queue.discoveryLedger.containsKey(id)) { // 发现账本去重 [P0-08]
                        if (id > upper && (pendingBarrier < 0 || id < pendingBarrier)) upper = id
                        continue
                    }
                    val name = c.getString(2) ?: "img_$id"
                    val size = c.getLong(3)
                    val mime = c.getString(4) ?: "image/*"
                    val capturedAt = c.getLong(5)
                    val relPath = c.getString(6) ?: ""
                    // 扩展名白名单（协议级拒绝，提前过滤省流量 [§5.2]）
                    val ext = name.substringAfterLast('.', "").lowercase()
                    if (ext !in setOf("jpg", "jpeg", "png", "webp", "gif", "bmp", "heic")) {
                        queue.discoveryLedger[id] = capturedAt
                        if (id > upper && (pendingBarrier < 0 || id < pendingBarrier)) upper = id
                        continue
                    }
                    // 单文件 200MB 上限（协议 §5.1）：超限不入队，账本记录防重扫 [R-5]
                    if (size > 200L * 1024 * 1024) {
                        android.util.Log.w("PhotoBeam", "超限跳过(${size}B > 200MB): $name")
                        queue.discoveryLedger[id] = capturedAt
                        if (id > upper && (pendingBarrier < 0 || id < pendingBarrier)) upper = id
                        continue
                    }
                    queue.enqueue(QueueItem(
                        fileId = "", // 发送时计算 [方案 A]
                        uri = ContentUris.withAppendedId(uri, id).toString(),
                        name = name, size = size, mime = mime,
                        capturedAt = capturedAt, mediaId = id,
                        relPath = relPath))
                    if (id > upper && (pendingBarrier < 0 || id < pendingBarrier)) upper = id
                    added++
                }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "reconcile 查询失败", e)
            return added
        }
        // 水位照常推进（失败项走待重试路径，与水位无关 [B-11 §7.2]）
        if (upper > queue.lastMaxId) {
            queue.lastMaxId = upper
            queue.save()
        }
        android.util.Log.d("PhotoBeam", "对账完成: 新增 $added, 水位 ${queue.lastMaxId}, 队列 ${queue.items.size}, 未就绪 ${queue.unreadyCount()}")
        queue.prune()
        return added
    }
}
