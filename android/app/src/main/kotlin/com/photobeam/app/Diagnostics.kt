package com.photobeam.app

// 手机端诊断信息导出 [A4]
// 语义与 PC 端「导出诊断信息」一致：**写文件**，不碰剪贴板。
// 落盘位置：getExternalFilesDir(null) 优先（用户能在文件管理器里找到），失败退回 filesDir。

import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import android.os.PowerManager
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

object Diagnostics {
    private fun fmt(pattern: String, t: Long = System.currentTimeMillis()): String =
        SimpleDateFormat(pattern, Locale.getDefault()).format(Date(t))

    /** 生成诊断文本 */
    fun build(ctx: Context): String {
        val app = ctx.applicationContext
        val prefs = app.getSharedPreferences("photobeam", Context.MODE_PRIVATE)
        val sb = StringBuilder()

        sb.append("===== PhotoBeam 诊断信息 =====\n")
        sb.append("生成时间：${fmt("yyyy-MM-dd HH:mm:ss")}\n\n")

        // ---- 应用 ----
        sb.append("--- 应用 ---\n")
        sb.append("包名：${app.packageName}\n")
        val ver = try {
            val pi = app.packageManager.getPackageInfo(app.packageName, 0)
            "${pi.versionName ?: "未设置"} (${pi.longVersionCode})"
        } catch (e: Exception) { "未知" }
        sb.append("版本：$ver\n")
        sb.append("协议版本：$PROTO_VER（最低支持 $MIN_PROTO_VER）\n")
        sb.append("设备型号：${Build.MODEL}\n")
        sb.append("Android 版本：${Build.VERSION.RELEASE}（SDK ${Build.VERSION.SDK_INT}）\n")
        sb.append("ABI：${Build.SUPPORTED_ABIS.joinToString(", ")}\n\n")

        // ---- 设备身份 ----
        sb.append("--- 设备身份 ---\n")
        val keys = try { IdentityKeys.loadOrCreate(prefs) } catch (e: Exception) { null }
        sb.append("设备指纹(fp)：${keys?.fingerprint() ?: "读取失败"}\n")
        sb.append("设备名称：${Build.MODEL}\n\n")

        // ---- 配对电脑 ----
        sb.append("--- 配对电脑 ---\n")
        val pc = try {
            PairingEngine(prefs, keys ?: IdentityKeys.loadOrCreate(prefs), Build.MODEL).loadPairedPC()
        } catch (e: Exception) { null }
        if (pc == null) {
            sb.append("未配对\n\n")
        } else {
            sb.append("名字：${pc.name}\n")
            sb.append("指纹(fp)：${pc.fp}\n")
            sb.append("最近 IP：${pc.lastIP}\n")
            sb.append("最近端口：${pc.lastPort}\n")
            val pairedAt = try { prefs.getLong("pc_paired_at", 0L) } catch (e: Exception) { 0L }
            sb.append("配对时间：${if (pairedAt > 0L) fmt("yyyy-MM-dd HH:mm:ss", pairedAt) else "未记录"}\n")
            val netFp = try { prefs.getString("net_fp", null) } catch (e: Exception) { null }
            sb.append("配对时网络指纹：${netFp ?: "未记录"}\n\n")
        }

        // ---- 队列 ----
        sb.append("--- 队列统计 ---\n")
        try {
            val q = QueueStore(app).also { it.load() }
            val pending = q.items.count { it.state == "pending" }
            val sending = q.items.count { it.state == "sending" }
            val sent = q.items.count { it.state == "sent" }
            val dropped = q.items.count { it.state == "dropped" }
            sb.append("待发送(pending)：$pending\n")
            sb.append("发送中(sending)：$sending\n")
            sb.append("已发送(sent)：$sent\n")
            sb.append("已丢弃(dropped)：$dropped\n")
            sb.append("合计：${q.items.size}\n")
            sb.append("其中暂不可读：${q.unreadyCount()}\n")
            sb.append("发现账本水位(last_max_id)：${q.lastMaxId}\n")
            sb.append("批次序号(batch_seq)：${q.batchSeq}\n")
        } catch (e: Exception) {
            sb.append("读取队列失败：${e.message}\n")
        }
        sb.append("\n")

        // ---- 服务 ----
        sb.append("--- 服务状态 ---\n")
        sb.append("服务运行：${SenderService.running}\n")
        sb.append("图标模式(iconMode)：${SenderService.iconMode}\n")
        sb.append("持久化图标模式：${try { prefs.getString("icon_mode", null) ?: "无" } catch (e: Exception) { "读取失败" }}\n")
        sb.append("仅闹钟模式：${SenderService.alarmOnlyMode || prefs.getBoolean(SenderService.PREFS_ALARM_ONLY, false)}\n")
        sb.append("前台服务超时连续次数：${try { prefs.getInt(SenderService.PREFS_TIMEOUT_STREAK, 0) } catch (e: Exception) { -1 }}\n")
        sb.append("startForeground 曾被拒(fgs_denied)：${try { prefs.getBoolean("fgs_denied", false) } catch (e: Exception) { false }}\n")
        val exempt = try {
            (app.getSystemService(Context.POWER_SERVICE) as PowerManager)
                .isIgnoringBatteryOptimizations(app.packageName)
        } catch (e: Exception) { false }
        sb.append("电池优化白名单：${if (exempt) "已放行" else "未放行（后台自启会被拦）"}\n")
        val nextAlarm = try { prefs.getLong("next_alarm_at", 0L) } catch (e: Exception) { 0L }
        sb.append("下次心跳闹钟：${if (nextAlarm > 0L) fmt("yyyy-MM-dd HH:mm:ss", nextAlarm) else "未排程"}\n\n")

        // ---- 网络 ----
        sb.append("--- 网络 ---\n")
        sb.append("网络指纹：${try { Discovery.currentFingerprint(app) } catch (e: Exception) { "读取失败" }}\n")
        sb.append("本机 IP：${try { Discovery.ownIP() ?: "未知" } catch (e: Exception) { "读取失败" }}\n")
        sb.append("网关 IP：${try { Discovery.gatewayIP(app) ?: "未知" } catch (e: Exception) { "读取失败" }}\n\n")

        // ---- 权限 ----
        sb.append("--- 权限 ---\n")
        for (p in listOf(
            android.Manifest.permission.READ_MEDIA_IMAGES,
            android.Manifest.permission.READ_MEDIA_VISUAL_USER_SELECTED,
            android.Manifest.permission.POST_NOTIFICATIONS)) {
            val granted = app.checkSelfPermission(p) == PackageManager.PERMISSION_GRANTED
            sb.append("$p：${if (granted) "已授予" else "未授予"}\n")
        }
        sb.append("\n")

        // ---- 最近同步 ----
        sb.append("--- 最近同步 ---\n")
        val at = try { prefs.getLong("last_sync_at", 0L) } catch (e: Exception) { 0L }
        if (at <= 0L) {
            sb.append("尚未同步\n\n")
        } else {
            sb.append("时间：${fmt("yyyy-MM-dd HH:mm:ss", at)}\n")
            sb.append("结果：${try { prefs.getString("last_sync_result", "") ?: "" } catch (e: Exception) { "" }}\n")
            sb.append("成功：${try { prefs.getBoolean("last_sync_ok", false) } catch (e: Exception) { false }}\n")
            sb.append("触发方式：${if (try { prefs.getBoolean("last_sync_manual", false) } catch (e: Exception) { false }) "手动（③ 立即同步）" else "周期心跳"}\n\n")
        }

        // ---- logcat ----
        sb.append("--- logcat -s PhotoBeam（最后 30 行）---\n")
        sb.append(logcatTail(30))
        sb.append("\n===== 诊断信息结束 =====\n")
        return sb.toString()
    }

    /** 取 logcat 最后 N 行（应用自己的日志；Android 10+ 系统只放行本应用 UID 的日志） */
    private fun logcatTail(lines: Int): String = try {
        val p = ProcessBuilder("logcat", "-d", "-s", "PhotoBeam", "-t", lines.toString())
            .redirectErrorStream(true)
            .start()
        val out = p.inputStream.bufferedReader().use { it.readText() }
        try { p.waitFor(3, java.util.concurrent.TimeUnit.SECONDS) } catch (e: Exception) { }
        p.destroy()
        if (out.isBlank()) "(无 PhotoBeam 日志)" else out.trimEnd()
    } catch (e: Exception) {
        "(读取 logcat 失败：${e.message})"
    }

    /**
     * 导出诊断信息到文件。
     * @return 成功返回文件；失败返回 null
     */
    fun export(ctx: Context): File? {
        val text = build(ctx)
        val name = "diagnostics-${fmt("yyyyMMdd-HHmmss")}.txt"
        // 优先外部私有目录（文件管理器可见），失败退回内部私有目录
        val dirs = listOfNotNull(ctx.getExternalFilesDir(null), ctx.filesDir)
        for (d in dirs) {
            try {
                val f = File(d, name)
                f.writeText(text, Charsets.UTF_8)
                try {
                    ctx.getSharedPreferences("photobeam", Context.MODE_PRIVATE)
                        .edit().putString("last_diag_path", f.absolutePath).apply()
                } catch (e: Exception) { }
                return f
            } catch (e: Exception) {
                android.util.Log.w("PhotoBeam", "写诊断文件失败(${d.absolutePath}): ${e.message}")
            }
        }
        return null
    }
}
