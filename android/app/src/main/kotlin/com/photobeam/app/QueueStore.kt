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
    var unreadyTries: Int = 0, // 读取未就绪计数（blocked 语义：永不因此丢弃 [§7.3.1]）
    var tries: Int = 0,        // 发送失败计数
    var dropReason: String = ""
)

class QueueStore(private val ctx: Context) {
    companion object {
        /** [Stage 14] 队列变化广播的节流窗口（毫秒） */
        private const val NOTIFY_DELAY_MS = 400L

        // 静态节流状态：QueueStore 会被多处 new（Activity / Service 各一份），
        // 节流标志必须跨实例共享，否则每个实例各自节流 = 不节流。
        private val notifyHandler = android.os.Handler(android.os.Looper.getMainLooper())
        @Volatile private var notifyScheduled = false
    }

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
        notifyQueueChanged()
    }

    /**
     * [Stage 14] 队列已落盘 → 通知 UI 实时刷新「待发送 N 张」。
     *
     * 放在 save() 里而不是各个调用点，是因为**队列状态变化的点太多**
     * （入队 / sending / sent / dropped / 溢出丢弃），漏一个就有一处数字不更新。
     * save() 是这些路径的**唯一汇合点** —— 落盘了就等于状态变了。
     *
     * 节流 400ms：连拍/批量补传时 save() 每张都调一次，不节流会疯狂刷 UI。
     */
    private fun notifyQueueChanged() {
        if (notifyScheduled) return
        notifyScheduled = true
        notifyHandler.postDelayed({
            notifyScheduled = false
            try {
                val app = ctx.applicationContext
                app.sendBroadcast(
                    android.content.Intent(SenderService.ACTION_QUEUE_CHANGED)
                        .setPackage(app.packageName))
            } catch (e: Exception) {
                android.util.Log.w("PhotoBeam", "发队列变化广播失败: ${e.message}")
            }
        }, NOTIFY_DELAY_MS)
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

    /** 全部待处理项（含未就绪）[§7.3.1]：通知显示"待发送 N 张（其中 M 张暂不可读）" */
    @Synchronized
    fun pendingCount(): Int = items.count { it.state == "pending" }

    /** 未就绪计数 [§7.3.1]：通知与主界面展示"其中 M 张暂不可读" */
    @Synchronized
    fun unreadyCount(): Int = items.count { it.state == "pending" && it.fileId.isEmpty() }

    /**
     * [Stage 15] 清理「反复尝试仍读不出来」的僵尸项。
     *
     * 为什么要单独做一次（而不是等 flushQueue 的重试上限）：
     * v1.2.2 及更早版本里，vivo 等 ROM 追加尾部元数据的截图**永远读不出来**，
     * 于是几百个历史截图长期卡在队列里。本版修好读取判据后，它们会**立刻变成可读**
     * → 如果不清掉，一次 flush 就会把它们**一次性全推给电脑** ——
     * 正是用户在 v1.2.0 时代抱怨过的「意外全量」。
     *
     * 判据用 unreadyTries：这批僵尸项在旧版本下已经被反复尝试过很多轮
     * （unreadyTries 很大），而正常的新项恒为 0 → 不会误伤。
     *
     * 一次性：靠 prefs 标记（PREFS_QUEUE_PURGED），只在升级后首次运行执行。
     * 被清掉的图用户仍可随时用「同时补传已有照片」主动传。
     */
    @Synchronized
    fun purgeStuckUnreadable(threshold: Int): Int {
        var n = 0
        for (it in items) {
            if (it.state == "pending" && it.unreadyTries >= threshold) {
                it.state = "dropped"
                it.dropReason = "stuck_unreadable"
                n++
            }
        }
        if (n > 0) save()
        return n
    }

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
