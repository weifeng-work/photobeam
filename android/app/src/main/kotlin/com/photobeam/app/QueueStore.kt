package com.photobeam.app

// 队列与发现账本（协议 §7.3 / §7.1 / §7.8）
// 持久化：写临时文件 → 原子 rename；.bak 上一版快照；启动时 sending→pending
// 方案 A [B-04]：入队零 IO（只记元数据），内容哈希在发送时计算（Sender.readReady + sendOne）
// 批次冻结 [C-12/D-08⑤]：batch_id 在入队时一次性分配，此后不重分配

import android.content.Context
import android.net.Uri
import org.json.JSONArray
import org.json.JSONObject
import java.io.File

class QueueItem(
    var fileId: String,        // 发送时计算的内容 SHA-256；"" = 尚未就绪 [B-04]
    val uri: String,
    val name: String,
    var size: Long,
    val mime: String,
    val capturedAt: Long,      // DATE_ADDED（秒）
    val mediaId: Long,         // MediaStore _ID（发现账本键）
    val relPath: String = "",  // RELATIVE_PATH（三路读取的 File 直读路径 [B-05]）
    var batchId: String = "",
    var isLatestInBatch: Boolean = false,
    var batchSize: Int = 1,    // 批内数量 [P0-04]
    var state: String = "pending", // pending | sending | sent | dropped
    var unreadyTries: Int = 0, // 读取未就绪计数（blocked 语义：永不因此丢弃 [§6.1]）
    var tries: Int = 0,        // 发送失败计数
    var dropReason: String = ""
)

class QueueStore(private val ctx: Context) {
    val items = ArrayList<QueueItem>()
    val discoveryLedger = HashMap<Long, Long>() // MediaStore _ID → DATE_ADDED [P0-08]
    var lastMaxId: Long = 0                      // 全局 _id 水位（files 表主键，全局唯一；失败项走待重试路径，水位照常推进 [B-11 §7.2]）
    var batchSeq: Int = 0                        // batch_id 单调计数 [P1-12]
    private val file = File(ctx.filesDir, "queue.json")

    @Synchronized
    fun load() {
        if (!file.exists()) return
        if (!parse(file)) {
            val bak = File(file.parentFile, "queue.json.bak")
            if (bak.exists()) parse(bak)
        }
        // 启动时 sending → pending [P0-10]
        for (it in items) if (it.state == "sending") it.state = "pending"
    }

    private fun parse(f: File): Boolean = try {
        val o = JSONObject(f.readText())
        val arr = o.optJSONArray("items") ?: return false
        items.clear()
        for (i in 0 until arr.length()) {
            val e = arr.getJSONObject(i)
            items.add(QueueItem(
                e.getString("file_id"), e.getString("uri"), e.getString("name"),
                e.getLong("size"), e.optString("mime"), e.getLong("captured_at"),
                e.getLong("media_id"), e.optString("rel_path"),
                e.optString("batch_id"), e.optBoolean("is_latest"),
                e.optInt("batch_size", 1), e.optString("state", "pending"),
                e.optInt("unready_tries"), e.optInt("tries"), e.optString("drop_reason")))
        }
        lastMaxId = o.optLong("last_max_id", 0)
        batchSeq = o.optInt("batch_seq", 0)
        val led = o.optJSONObject("ledger")
        if (led != null) for (k in led.keys()) discoveryLedger[k.toLong()] = led.getLong(k)
        true
    } catch (e: Exception) {
        false
    }

    @Synchronized
    fun save() {
        val o = JSONObject()
        val arr = JSONArray()
        for (it in items) arr.put(JSONObject()
            .put("file_id", it.fileId).put("uri", it.uri).put("name", it.name)
            .put("size", it.size).put("mime", it.mime).put("captured_at", it.capturedAt)
            .put("media_id", it.mediaId).put("rel_path", it.relPath)
            .put("batch_id", it.batchId)
            .put("is_latest", it.isLatestInBatch).put("batch_size", it.batchSize)
            .put("state", it.state)
            .put("unready_tries", it.unreadyTries)
            .put("tries", it.tries).put("drop_reason", it.dropReason))
        o.put("items", arr)
        o.put("last_max_id", lastMaxId)
        o.put("batch_seq", batchSeq)
        val led = JSONObject()
        for ((k, v) in discoveryLedger) led.put(k.toString(), v)
        o.put("ledger", led)
        val tmp = File(file.parentFile, "queue.json.tmp")
        tmp.writeText(o.toString())
        // 保留上一版快照 [P0-10]
        if (file.exists()) file.copyTo(File(file.parentFile, "queue.json.bak"), overwrite = true)
        tmp.renameTo(file) // Android File.rename = rename(2) 同目录原子
    }

    /**
     * 入队（方案 A：零 IO，只记元数据）+ 批次冻结分配 [C-12/D-08⑤]：
     * 与上一张 capturedAt 间隔 ≤3s → 同批（同批内 is_latest 让位给新张）；否则开新批。
     * batch_id 一次性分配，此后不重分配 [C-12]。发现账本在入队时即记 _ID [S-08]。
     */
    @Synchronized
    fun enqueue(it: QueueItem) {
        val prev = items.lastOrNull()
        if (prev != null && prev.batchId.isNotEmpty() &&
            it.capturedAt - prev.capturedAt in 0..3) {
            // 同批：上一张的 is_latest 让位
            it.batchId = prev.batchId
            it.isLatestInBatch = true
            prev.isLatestInBatch = false
        } else {
            batchSeq++
            it.batchId = "b$batchSeq"
            it.isLatestInBatch = true
        }
        items.add(it)
        // 发现账本在入队时即记 _ID [S-08]
        discoveryLedger[it.mediaId] = it.capturedAt
    }

    /** 全部待处理项（含未就绪；未就绪由发送时就绪判定逐项跳过 [D-01②]） */
    @Synchronized
    fun pendingItems(): List<QueueItem> = items.filter { it.state == "pending" }

    /** 可立即发送项（已就绪） */
    @Synchronized
    fun readyItems(): List<QueueItem> = pendingItems().filter { it.fileId.isNotEmpty() }

    /** 全部待处理项（含未就绪）[§6.2]：通知显示"待发送 N 张（其中 M 张暂不可读）" */
    @Synchronized
    fun pendingCount(): Int = items.count { it.state == "pending" }

    /** 未就绪计数 [§6.2]：通知与主界面展示"其中 M 张暂不可读" */
    @Synchronized
    fun unreadyCount(): Int = items.count { it.state == "pending" && it.fileId.isEmpty() }

    @Synchronized
    fun prune() {
        // 账本清理 [R-08]：仅保留最近 30 天
        val cutoff = System.currentTimeMillis() / 1000 - 30L * 24 * 3600
        discoveryLedger.entries.removeIf { it.value < cutoff && it.key <= lastMaxId }
        items.removeAll { it.state == "sent" && (it.capturedAt < cutoff) }
        // 双上限：200 张 / 500MB，丢最旧 pending [P1-05][P1-08]
        var dropped = 0
        while (items.count { it.state == "pending" } > 200) {
            val oldest = items.filter { it.state == "pending" }.minByOrNull { it.capturedAt } ?: break
            oldest.state = "dropped"; oldest.dropReason = "queue_overflow"; dropped++
        }
        var bytes = items.filter { it.state == "pending" }.sumOf { maxOf(it.size, 1L) }
        while (bytes > 500L * 1024 * 1024 && items.any { it.state == "pending" }) {
            val oldest = items.filter { it.state == "pending" }.minByOrNull { it.capturedAt } ?: break
            bytes -= maxOf(oldest.size, 1L)
            oldest.state = "dropped"; oldest.dropReason = "queue_overflow"; dropped++
        }
        if (dropped > 0) Notifier.queueOverflow(ctx, dropped)
    }
}
