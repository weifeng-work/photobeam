package com.photobeam.app

// 主界面：权限引导、配对、状态、启动服务

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.widget.AdapterView
import android.widget.ArrayAdapter
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ListView
import android.widget.ScrollView
import android.widget.TextView
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

object Notifier {
    const val CH_EVENTS = "photobeam_events"
    // [E-a] 「图片已到电脑」单独一个**低优先级**通道：连着拍十几张时不该一直响。
    const val CH_ARRIVED = "photobeam_arrived"
    fun channel(ctx: Context): NotificationManager {
        val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.createNotificationChannel(
            NotificationChannel(CH_EVENTS, "事件通知", NotificationManager.IMPORTANCE_DEFAULT))
        nm.createNotificationChannel(
            NotificationChannel(CH_ARRIVED, "图片已到电脑", NotificationManager.IMPORTANCE_LOW))
        return nm
    }

    /**
     * [E-a] 图片已到电脑 —— **合并式**通知：
     *  始终复用同一个通知 id（104），连续到达只更新那一条 → 通知栏里只有一条，
     *  标题带累计张数、内容带最近一张文件名。
     *  通道是 IMPORTANCE_LOW → 连续截图**不会**连环响铃/震动（只安静地更新）。
     */
    fun arrived(ctx: Context, total: Int, lastName: String) {
        channel(ctx).notify(104, android.app.Notification.Builder(ctx, CH_ARRIVED)
            .setContentTitle("已到电脑：$total 张")
            .setContentText(if (lastName.isEmpty()) "图片已同步到电脑" else "最近一张：$lastName")
            .setSmallIcon(android.R.drawable.stat_notify_sync).build())
    }
    fun queueOverflow(ctx: Context, dropped: Int) {
        channel(ctx).notify(100, android.app.Notification.Builder(ctx, CH_EVENTS)
            .setContentTitle("队列已满")
            .setContentText("为防止占满空间，已丢弃 $dropped 张最旧的待发送图片")
            .setSmallIcon(android.R.drawable.stat_notify_error).build())
    }
    fun dropped(ctx: Context, name: String, reason: String) {
        channel(ctx).notify(101, android.app.Notification.Builder(ctx, CH_EVENTS)
            .setContentTitle("一张图片被电脑拒绝")
            .setContentText("$name：$reason（不再重试）")
            .setSmallIcon(android.R.drawable.stat_notify_error).build())
    }

    /**
     * 本端丢弃：MediaStore 行已被删除，该图永远无法再发送 [协议 §7.3]。
     * 与「被电脑拒绝」区分开——这一张不是电脑不要，是它自己已经不存在了。
     */
    fun localDropped(ctx: Context, name: String) {
        channel(ctx).notify(103, android.app.Notification.Builder(ctx, CH_EVENTS)
            .setContentTitle("一张图片已失效")
            .setContentText("$name：相册里已不存在该图片，已跳过（不影响其他图片）")
            .setSmallIcon(android.R.drawable.stat_notify_error).build())
    }

    /**
     * [A6] 前台服务被系统连续超时停止 → 已降级为「仅闹钟模式」。
     * 提示用户去授予省电白名单（事件通道，与队列通知同一通道）。
     */
    fun batteryAdvice(ctx: Context) {
        channel(ctx).notify(102, android.app.Notification.Builder(ctx, CH_EVENTS)
            .setContentTitle("PhotoBeam 已降级为仅闹钟模式")
            .setContentText("前台服务被系统反复超时停止，请点 ⑤ 允许后台常驻（关闭电池优化）后重新打开应用")
            .setSmallIcon(android.R.drawable.stat_notify_error).build())
    }
}

class MainActivity : Activity() {
    private lateinit var prefs: android.content.SharedPreferences
    private lateinit var queue: QueueStore
    private lateinit var statusView: TextView

    /** UI 线程 Handler：只用于「③ 立即同步」的轮询回显 [A3] */
    private val uiHandler = Handler(Looper.getMainLooper())
    private var syncPolling = false

    /**
     * 需要申请的运行时权限 [协议 §7.7]
     *
     * Android 14+ 是**三态**授权（全量 / 部分 / 无）。
     * 申请时必须同时带上 READ_MEDIA_VISUAL_USER_SELECTED：
     * 否则用户选「部分照片」时应用拿不到部分授权，只会被系统判成"未授予"。
     */
    private val needed: MutableList<String> = mutableListOf(
        Manifest.permission.READ_MEDIA_IMAGES,
        Manifest.permission.POST_NOTIFICATIONS
    ).also {
        if (Build.VERSION.SDK_INT >= 34) it.add(Manifest.permission.READ_MEDIA_VISUAL_USER_SELECTED)
    }

    /** 照片授权三态 [协议 §7.7] */
    private enum class PhotoPerm { FULL, PARTIAL, NONE }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = getSharedPreferences("photobeam", Context.MODE_PRIVATE)
        queue = QueueStore(this).also { it.load() }

        val pad = (16 * resources.displayMetrics.density).toInt()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, pad)
        }
        val title = TextView(this).apply {
            text = "PhotoBeam"
            textSize = 24f
            gravity = Gravity.CENTER
        }
        statusView = TextView(this).apply {
            textSize = 14f
            setPadding(0, pad, 0, pad)
        }
        fun btn(text: String, action: (View.OnClickListener.() -> Unit)? = null): Button =
            Button(this).apply {
                this.text = text
                layoutParams = ViewGroup.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT)
            }

        // [Stage 5] 手工输入电脑 IP 的兜底（协议 §2 [R-19]）：
        // 某些网络（AP 隔离 / 不同 VLAN / 广播被挡）三通道都找不到电脑，
        // 但只要用户知道 PC 的 IP，直连通道① 就能连上 —— 前提是有一个地方能填这个 IP。
        // 此前手机上没有任何输入口，却在失败提示里让用户「手动输入电脑 IP」→ 改为真的提供输入框。
        val ipInput = EditText(this).apply {
            // 示例地址必须是**中性的**，不能写本机真实网段（仓库会进 GitHub）
            hint = "电脑 IP（可选：连不上时手动填，如 192.168.1.10）"
            textSize = 14f
            inputType = android.text.InputType.TYPE_CLASS_TEXT or
                android.text.InputType.TYPE_TEXT_VARIATION_URI
            setSingleLine(true)
            layoutParams = ViewGroup.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT)
        }

        // [A5] 按钮编号连续、文案统一
        // [A4] ① 不是「首次授权」而是「再要一次」——应用启动时已经自动申请过，
        //      用户被拒过之后系统不会再弹窗，所以文案要说清它到底能干什么。
        val bPerm = btn("① 重新申请照片与通知权限（被拒过请用下一项）")
        val bPermSys = btn("打开系统权限页（改为「允许所有照片」）")
        val bPair = btn("② 配对电脑（核对 6 位数字）")
        val bSync = btn("③ 立即同步")
        val bService = btn("④ 启动 / 刷新后台同步服务")
        val bBattery = btn("⑤ 允许后台常驻（关闭电池优化）")
        val bStop = btn("⑥ 停止同步服务")
        val bDiag = btn("⑦ 导出诊断信息")
        // [Stage 8] 同步目录筛选：用户选择哪些手机相册目录参与同步
        val bDirs = btn("⑧ 选择同步文件夹（默认不同步任何目录）")

        root.addView(title)
        root.addView(statusView)
        root.addView(bPerm)
        root.addView(bPermSys)
        root.addView(bPair)
        root.addView(ipInput)
        root.addView(bSync)
        root.addView(bService)
        root.addView(bBattery)
        root.addView(bStop)
        root.addView(bDiag)
        root.addView(bDirs)

        // [Stage 9 · 修复] 内容超出屏幕时无法滚动 —— ⑦「导出诊断信息」被裁到只剩 21px，
        // ⑧「选择同步文件夹」完全在屏幕外，用户根本点不到。
        // 根因：root 是裸 LinearLayout，直接 setContentView(root) → 超出部分被裁掉且不可滚。
        // 修法：把 root 包进 ScrollView（可滚动），并给底部留一段空白，
        //       让最后一个按钮滚到顶时也能完整显示（不被手势条/导航栏压住）。
        val scroll = ScrollView(this).apply {
            isFillViewport = true
            addView(root, ViewGroup.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        }
        root.setPadding(pad, pad, pad, pad * 2)   // 底部双倍留白：最后一个按钮滚上来也不贴边
        setContentView(scroll)

        bPerm.setOnClickListener {
            requestPermissions(needed.toTypedArray(), 100)
        }
        bPermSys.setOnClickListener {
            // [A2] 部分照片授权时，① 已经问过系统了、改不了 →
            // 只能跳到系统应用详情页让用户自己改成「允许所有照片」。
            try {
                startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                    .setData(Uri.parse("package:$packageName")))
            } catch (e: Exception) {
                statusView.text = "无法打开系统权限页：${e.message}"
            }
        }
        // [Stage 5] 填了 IP 就优先用它（直连通道①），没填就走完整三通道发现
        bPair.setOnClickListener { pairFlow(ipInput.text.toString().trim()) }
        bSync.setOnClickListener {
            // [A3] 立即同步：服务跑完一轮会把结果写进 prefs，这里启动轮询等回执
            SenderService.start(this, SenderService.ACTION_SYNC_NOW)
            statusView.text = "已触发同步，请稍候…"
            startSyncPolling()
        }
        bService.setOnClickListener { SenderService.start(this, SenderService.ACTION_ENSURE_FGS) }
        bBattery.setOnClickListener {
            // [修复 2026-11-02] Android 12+ 后台启动 FGS 受限 + MIUI 更严：
            // 让用户把应用加进"电池优化白名单"（= deviceidle whitelist），
            // 这是"图标亮=已连接"能长期成立的前提之一。
            // 实测（MIUI 13）：加入白名单后 →
            //   ① "Pending user blocked background alarms" 立即清空（精确闹钟不再被挂起）
            //   ② 后台启动 FGS 变为 SYSTEM_ALLOW_LISTED（永久放行）
            // MIUI 上标准 intent 常被忽略 → 按优先级依次尝试多个入口。
            val candidates = listOf(
                Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
                    .setData(Uri.parse("package:$packageName")),
                // MIUI 自启动/省电策略管理页
                Intent("miui.intent.action.OP_AUTO_START"),
                Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS),
                Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                    .setData(Uri.parse("package:$packageName"))
            )
            for (c in candidates) {
                try {
                    startActivity(c)
                    return@setOnClickListener
                } catch (e: Exception) {
                    // 试下一个
                }
            }
            statusView.text = "无法打开系统设置，请手动到：设置 → 应用管理 → PhotoBeam → 省电策略 → 无限制"
        }
        bStop.setOnClickListener {
            // [A1] 停止同步服务：走服务自己的 ACTION_STOP 通道（服务会撤心跳闹钟 + 撤通知）
            SenderService.start(this, SenderService.ACTION_STOP)
            cancelHeartbeat(this)
            statusView.text = "已请求停止同步服务…"
            // 服务销毁是异步的 → 稍后刷新状态区（如实显示"服务：未运行 / 图标：无"）
            uiHandler.postDelayed({ refreshStatus() }, 600)
            uiHandler.postDelayed({ refreshStatus() }, 1500)
        }
        bDiag.setOnClickListener { exportDiagnostics() }
        bDirs.setOnClickListener { showDirPicker(firstRun = false) }

        requestPermissions(needed.toTypedArray(), 100)
        refreshStatus()

        // [Stage 8] 首次启动引导：必须先选同步目录，否则**一张图都不会同步**
        //（隐私默认拒绝）。已选过就不再弹（用户随时可用 ⑧ 改）。
        if (!SyncDirs.hasPicked(this)) {
            root.post { showDirPicker(firstRun = true) }
        }
    }

    // ---------- 同步目录选择 [Stage 8] ----------

    /**
     * 目录选择对话框：列出手机上有图片的目录，多选。
     *
     * 不用 SAF 目录授权：MediaStore 的 RELATIVE_PATH 已经给出全部图片目录，
     * 现有「允许访问所有照片」权限就够读目录名 —— 不给用户额外的授权弹窗。
     *
     * @param firstRun 首次引导（说明"不选就不同步"）；平时从 ⑧ 进入
     */
    private fun showDirPicker(firstRun: Boolean) {
        val dirs = SyncDirs.listDirs(this)
        if (dirs.isEmpty()) {
            // 手机里还没有任何图片 → 没有可选目录，提示去拍照/截图
            AlertDialog.Builder(this)
                .setTitle(if (firstRun) "选择要同步的文件夹" else "选择要同步的文件夹")
                .setMessage("手机相册里还没有图片。\n\n先截几张图或拍几张照片，再回到这里选择要同步的文件夹。\n\n" +
                    "（未选择任何文件夹时，PhotoBeam 不会同步任何图片——这是默认的隐私保护。）")
                .setPositiveButton("好", null).show()
            return
        }
        val labels = dirs.map { "${it.first}（${it.second} 张）" }.toTypedArray()
        val checked = BooleanArray(dirs.size) { i ->
            SyncDirs.selected(this).contains(dirs[i].first)
        }
        // [Stage 8 · UI 修正] 不用 setMessage + setMultiChoiceItems ——
        // AlertDialog 里 setMessage 会**覆盖掉** setMultiChoiceItems 的列表（只显示说明文字，
        // 目录一项都不出来）。改用 setView 自定义布局：说明 TextView + ListView。
        val pad = (12 * resources.displayMetrics.density).toInt()
        val container = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, 0)
        }
        container.addView(TextView(this).apply {
            text = "勾选需要同步到电脑的相册文件夹（可多选）。\n" +
                "· 勾选某个文件夹会连同它的子文件夹一起同步。\n" +
                "· 未勾选任何文件夹 = 不同步任何图片。\n" +
                "· 只有之后新增的图片会被同步，历史图片不会补传。"
            setPadding(0, 0, 0, pad)
        })
        // 显式起一个 ListView 变量：apply 块内的 this 与 lambda 内的 this 不是同一个，
        // 直接写 adapter/setItemChecked 会解析不到（编译器报 unresolved reference）。
        val lv = ListView(this)
        lv.adapter = ArrayAdapter(this, android.R.layout.simple_list_item_multiple_choice, labels)
        lv.setOnItemClickListener(object : AdapterView.OnItemClickListener {
            override fun onItemClick(parent: AdapterView<*>?, view: View?, position: Int, id: Long) {
                val which = position
                checked[which] = !checked[which]
                for (i in labels.indices) lv.setItemChecked(i, checked[i])
            }
        })
        for (i in labels.indices) lv.setItemChecked(i, checked[i])
        container.addView(lv)

        val dlg = AlertDialog.Builder(this)
            .setTitle("选择要同步的文件夹")
            .setView(container)
            .setPositiveButton("保存") { _, _ ->
                val chosen = HashSet<String>()
                for (i in dirs.indices) if (checked[i]) chosen.add(dirs[i].first)
                SyncDirs.save(this, chosen)
                android.util.Log.d("PhotoBeam", "同步目录已保存：${chosen.size} 个（$chosen）")
                if (chosen.isEmpty()) {
                    statusView.text = "已保存：未选择任何文件夹 → 当前不会同步任何图片"
                } else {
                    statusView.text = "已保存：${chosen.size} 个文件夹参与同步 → ${chosen.joinToString(", ")}"
                }
                // 立刻触发一次对账（让新选的目录里的新图马上能发出去）
                SenderService.start(this, SenderService.ACTION_SYNC_NOW)
                startSyncPolling()
            }
            .setNegativeButton(if (firstRun) "暂不选择（不同步任何图片）" else "取消") { _, _ ->
                if (firstRun) {
                    // 首次引导取消 = 明确保持"不同步任何东西"，并标记已做过选择
                    // （避免每次启动都弹窗骚扰；用户随时可从 ⑧ 改）。
                    SyncDirs.save(this, emptySet())
                    statusView.text = "当前不会同步任何图片。可点「⑧ 选择同步文件夹」随时开启。"
                }
            }
            .create()
        dlg.show()
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        // [A2] 授权结果变了 → 状态区必须刷新（部分授权 ≠ 未授权）
        refreshStatus()
    }

    override fun onResume() {
        super.onResume()
        // [修复 2026-11-02] 应用回到前台 = 允许后台启动 FGS 的时机：
        // 顺便把"降级态"（startForeground 被拒）自动恢复成正规前台服务通知。
        SenderService.start(this, SenderService.ACTION_ENSURE_FGS)
        refreshStatus()
    }

    override fun onPause() {
        super.onPause()
        // [A3] 离开前台就停轮询（别在后台空转）
        stopSyncPolling()
    }

    override fun onDestroy() {
        super.onDestroy()
        stopSyncPolling()
    }

    // ---------- 状态区 ----------

    private fun refreshStatus() {
        val paired = prefs.getString("pc_name", null)
        val pending = queue.pendingCount()
        val mode = prefs.getString("icon_mode", null) ?: SenderService.iconMode
        // [修复 2026-11-02 v2] 电池优化白名单状态：
        // 实测（MIUI 13）加入 deviceidle 白名单后，
        //   ① "Pending user blocked background alarms" 立即清空（精确闹钟不再被挂起）
        //   ② 后台启动 FGS 变为 "SYSTEM_ALLOW_LISTED"（永久放行，不再是 20s 临时许可）
        // 这是 MIUI 上"后台自起 + 状态栏图标可见"能否成立的决定性开关，必须如实显示。
        val exempt = isIgnoringBatteryOptimizations()
        // [A6] 仅闹钟降级态：前台服务被系统连续超时停止后，不再申请前台服务身份
        val alarmOnly = SenderService.alarmOnlyMode || prefs.getBoolean(SenderService.PREFS_ALARM_ONLY, false)
        // [Stage 8] 同步目录状态：让用户一眼看出"现在到底在同步什么"
        // 注意：必须在 buildString 之外取 —— buildString 里 this 是 StringBuilder，不是 Context
        val selDirs = SyncDirs.selected(this)
        statusView.text = buildString {
            appendLine(if (selDirs.isEmpty()) "同步目录：未选择（不同步任何图片）"
                       else "同步目录：${selDirs.size} 个（${selDirs.joinToString(", ")}）")
            append("电脑：${paired ?: "未配对"}\n")
            append("待发送：$pending 张\n")
            append("服务：${serviceStateText(alarmOnly)}\n")
            // 状态栏图标状态如实显示：图标亮 = 已连接，所以图标本身必须一直在
            append("状态栏图标：${when (mode) {
                "fgs" -> "正常（前台服务）"
                "fallback" -> "降级（普通通知）"
                "alarm" -> "无（仅闹钟模式）"
                else -> "无（服务未运行）"
            }}\n")
            append("后台常驻：${if (exempt) "已放行（可后台自启）" else "未放行（后台自启会被拦）"}\n")
            if (!exempt) {
                // [A5] 文案修正：实测未放行白名单时的真实表现是"开机自启被拦、状态栏图标可能长时间不亮"
                append("⚠ 未加入电池优化白名单 → 点 ⑤ 允许后台常驻，否则开机自启会被拦、状态栏图标可能长时间不亮\n")
            }
            if (alarmOnly) {
                append("⚠ 前台服务被系统超时停止（连续 3 次）→ 已降级为仅闹钟模式，点 ⑤ 允许后台常驻后重新打开应用可恢复\n")
            } else if (SenderService.running && mode != "fgs" && mode != "alarm") {
                // 只有"服务在跑但拿不到前台服务通知"才算降级态；
                // 服务根本没起来时（刚点过 ⑥）提示"点 ④ 启动"更准确。
                append("⚠ 状态栏处于降级态：点 ④ 恢复正规前台服务\n")
            } else if (!SenderService.running) {
                append("⚠ 服务未运行：点 ④ 启动后台同步服务\n")
            }
            // [A2] 照片授权三态：部分授权时点 ① 也改不回来（系统已问过），要提示去系统设置改全量
            when (photoPerm()) {
                PhotoPerm.PARTIAL ->
                    append("⚠ 仅授权了部分照片 → 点 ① 改为「允许访问所有照片」（或用上面的系统权限页）\n")
                PhotoPerm.NONE ->
                    append("⚠ 尚有权限未授予（点 ①）\n")
                PhotoPerm.FULL -> { }
            }
            if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
                append("⚠ 通知权限未授予（点 ①）\n")
            }
            append(lastSyncLine())
            append(arrivalLine())   // [B3] 逐张「已到电脑」回执
            append(failedLine())    // [B3] 最近一次失败
            val diag = prefs.getString("last_diag_path", null)
            if (diag != null) append("最近诊断：$diag\n")
        }
    }

    /** 服务状态文案 [A6：降级态必须如实显示] */
    private fun serviceStateText(alarmOnly: Boolean): String = when {
        alarmOnly -> "降级（仅闹钟，靠心跳闹钟拉起）"
        SenderService.running -> "运行中"
        else -> "未运行"
    }

    /** [A3] 最近同步：HH:mm:ss + 结果；没有记录时显示"尚未同步" */
    private fun lastSyncLine(): String {
        val at = try { prefs.getLong("last_sync_at", 0L) } catch (e: Exception) { 0L }
        if (at <= 0L) return "最近同步：尚未同步\n"
        val hh = SimpleDateFormat("HH:mm:ss", Locale.getDefault()).format(Date(at))
        val res = try { prefs.getString("last_sync_result", "") ?: "" } catch (e: Exception) { "" }
        val ok = try { prefs.getBoolean("last_sync_ok", false) } catch (e: Exception) { false }
        return "最近同步：$hh $res${if (ok) "（成功）" else "（未成功）"}\n"
    }

    /** [B3] 逐张到达回执：已到电脑 N 张（最近 <文件名> <时间>）。
     *  之前只有「最近同步：…（成功）」这种汇总，用户看不出「这一张到底到没到电脑」。 */
    private fun arrivalLine(): String {
        val n = try { prefs.getInt("arrived_count", 0) } catch (e: Exception) { 0 }
        if (n <= 0) return ""
        val name = try { prefs.getString("arrived_last_name", "") ?: "" } catch (e: Exception) { "" }
        val at = try { prefs.getLong("arrived_last_at", 0L) } catch (e: Exception) { 0L }
        val hh = if (at > 0L) SimpleDateFormat("HH:mm:ss", Locale.getDefault()).format(Date(at)) else ""
        val tail = if (name.isEmpty()) "" else "（最近 $name${if (hh.isNotEmpty()) " $hh" else ""}）"
        return "已到电脑：$n 张$tail\n"
    }

    /** [B3] 最近一次失败的图片与原因（成功一张后自动清除） */
    private fun failedLine(): String {
        val name = try { prefs.getString("failed_last_name", null) } catch (e: Exception) { null }
            ?: return ""
        val reason = try { prefs.getString("failed_last_reason", "") ?: "" } catch (e: Exception) { "" }
        val at = try { prefs.getLong("failed_last_at", 0L) } catch (e: Exception) { 0L }
        val hh = if (at > 0L) SimpleDateFormat("HH:mm:ss", Locale.getDefault()).format(Date(at)) else ""
        val r = if (reason.isEmpty()) "" else "（$reason）"
        return "⚠ 最近失败：$name$r${if (hh.isNotEmpty()) " $hh" else ""}\n"
    }

    /** [A10] 配对失败文案：原始异常可能是英文/技术串 → 映射成用户能看懂的中文。
     *  映射不到时保留原始信息（放中文标题下面），但不把英文技术串当标题甩给用户。 */
    private fun friendlyPairError(e: Exception): String {
        val raw = (e.message ?: e.javaClass.simpleName)
        return when {
            e is java.net.ConnectException ->
                "连不上电脑。\n请确认电脑上的 PhotoBeam 正在运行，且手机和电脑连的是同一个 WiFi。"
            e is java.net.SocketTimeoutException ->
                "连接电脑超时（等不到回应）。\n请确认电脑上的 PhotoBeam 正在运行，且手机和电脑连的是同一个 WiFi。"
            // PC 端发了 pair_reject（PC 上点了「不一致」）→ 手机端读到的是 pair_reject 而非 pair_ok
            raw.contains("配对流程异常") ->
                "电脑端拒绝了本次配对。\n多半是两端显示的 6 位数字不一致（或电脑上点过「重新配对」）。\n请确认两边数字一样后，再点一次 ②。"
            raw.contains("用户拒绝配对") ->
                "配对被拒绝：两端显示的 6 位数字不一致。\n请确认两边数字一样后重新配对。"
            raw.contains("配对核对超时") ->
                "配对核对超时（60 秒内没有点确认）。\n请重新点一次 ② 配对电脑。"
            raw.contains("电脑签名校验失败") || raw.contains("认证流程异常") ->
                "电脑的身份密钥变了（电脑重装过、或换过机器）。\n请在电脑上点「重新配对」，再点一次 ②。"
            raw.contains("对端要求重新配对") || raw.contains("身份已变化") ->
                "电脑端不认识这台手机了（电脑可能重装过，或在电脑上点过「重新配对」）。\n请在电脑上重新配对后，再点一次 ②。"
            raw.contains("后台发送路径") ->
                "这条连接是后台自动同步发起的，不能用来配对。\n请在手机上点 ② 配对电脑。"
            raw.contains("Connection reset") || raw.contains("Connection aborted") ||
                raw.contains("Software caused connection") ->
                "连接被电脑端断开了。\n请重试（②）。"
            raw.contains("timeout") || raw.contains("timed out") ->
                "连接电脑超时。\n请确认电脑上的 PhotoBeam 正在运行。"
            else ->
                "配对失败。\n原因：$raw\n请重试（②）。"
        }
    }

    /** [A2] 照片授权三态：全量 / 部分 / 无 */
    private fun photoPerm(): PhotoPerm {
        if (checkSelfPermission(Manifest.permission.READ_MEDIA_IMAGES)
            == PackageManager.PERMISSION_GRANTED) return PhotoPerm.FULL
        if (Build.VERSION.SDK_INT >= 34 &&
            checkSelfPermission(Manifest.permission.READ_MEDIA_VISUAL_USER_SELECTED)
            == PackageManager.PERMISSION_GRANTED) return PhotoPerm.PARTIAL
        return PhotoPerm.NONE
    }

    /** 是否已加入电池优化白名单（= deviceidle whitelist） */
    private fun isIgnoringBatteryOptimizations(): Boolean = try {
        val pm = getSystemService(Context.POWER_SERVICE) as android.os.PowerManager
        pm.isIgnoringBatteryOptimizations(packageName)
    } catch (e: Exception) {
        false
    }

    // ---------- ③ 立即同步：轮询等回执 [A3] ----------

    /** 每 500ms 刷新状态区，直到检测到 last_sync_at 变化（服务写回了本轮结果） */
    private fun startSyncPolling() {
        stopSyncPolling()
        syncPolling = true
        val seen = try { prefs.getLong("last_sync_at", 0L) } catch (e: Exception) { 0L }
        val poll = object : Runnable {
            override fun run() {
                if (!syncPolling) return
                val now = try { prefs.getLong("last_sync_at", 0L) } catch (e: Exception) { 0L }
                refreshStatus()
                if (now != seen) {
                    syncPolling = false
                    return
                }
                uiHandler.postDelayed(this, 500)
            }
        }
        uiHandler.postDelayed(poll, 500)
    }

    private fun stopSyncPolling() {
        syncPolling = false
        uiHandler.removeCallbacksAndMessages(null)
    }

    // ---------- ⑦ 导出诊断信息 [A4] ----------

    /** 生成诊断文本 → **写文件**（与 PC 端语义一致，不用剪贴板）→ 告知完整路径 */
    private fun exportDiagnostics() {
        statusView.text = "正在生成诊断信息…"
        Thread {
            val file = Diagnostics.export(applicationContext)
            runOnUiThread {
                if (file == null) {
                    statusView.text = "诊断信息导出失败（无可写目录）"
                    AlertDialog.Builder(this)
                        .setTitle("导出诊断信息失败")
                        .setMessage("没有找到可写的目录，请检查存储空间后重试。")
                        .setPositiveButton("好", null).show()
                } else {
                    refreshStatus()
                    AlertDialog.Builder(this)
                        .setTitle("诊断信息已导出")
                        .setMessage("完整路径：\n${file.absolutePath}\n\n" +
                            "（文件管理器 → 内部存储 → Android → data → com.photobeam.app → files 可找到）")
                        .setPositiveButton("好", null).show()
                }
            }
        }.start()
    }

    /** 配对流程：发现 → 握手 → SAS 双端比对 → 保存
     *  [manualIP] 用户手工填的电脑 IP（可空）。填了就**优先直连它**（通道①），
     *  用于广播被挡（AP 隔离 / 不同 VLAN）时三通道都找不到电脑的场景。 */
    private fun pairFlow(manualIP: String? = null) {
        statusView.text = "正在发现电脑…"
        Thread {
            var sasTimedOut = false
            // [Stage 5 第五轮] PC 明确拒绝（H）/ 对方断开
            var peerRejected = false
            var peerRejectReason: String? = null
            try {
                // 手工填的 IP 优先；没填就用上次记录里的
                val lastIP = manualIP?.takeIf { it.isNotBlank() }
                    ?: prefs.getString("pc_ip", null)
                val lastPort = prefs.getInt("pc_port", 0)
                // [Stage 5 修正] 把已配对 PC 的指纹一并传下去：同一局域网里可能有多台
                // PhotoBeam PC，靠指纹才能分清「自己那台」和「别人的那台」。
                val pc = Discovery.resolve(this, lastIP, lastPort, prefs.getString("pc_fp", null))
                if (pc == null) {
                    runOnUiThread {
                        statusView.text = "未发现电脑。\n请确认：手机与电脑连同一个 WiFi；" +
                            "电脑端 PhotoBeam 正在运行。\n（也可以在上面直接填电脑的 IP 再点 ②）"
                    }
                    return@Thread
                }


                runOnUiThread { statusView.text = "已发现 ${pc.name.ifEmpty { pc.ip }}，正在配对…" }

                val engine = PairingEngine(prefs, IdentityKeys.loadOrCreate(prefs), Build.MODEL)
                val sock = java.net.Socket()
                sock.tcpNoDelay = true
                sock.connect(java.net.InetSocketAddress(pc.ip, pc.port), 3000)
                sock.soTimeout = 90_000
                val res = engine.handshake(sock, sasConfirm = { sas, peerName ->
                    // UI 线程弹 SAS 比对对话框，同步等待用户确认
                    // [Stage 5 修订] 等待上限 15s → 60s：人眼核对 6 位数字再点按钮，15s 不够
                    //（实测首轮验收时手机端因等待超时直接发出了 pair_reject）。
                    // **两端必须一致**：PC 端向导窗同样是 60s（pc/pairing_windows.go pairTimeout）。
                    android.util.Log.d("PhotoBeam", "SAS 回调进入: $sas (对方电脑=$peerName)")
                    val latch = java.util.concurrent.CountDownLatch(1)
                    var agreed = false
                    val peerGone = java.util.concurrent.atomic.AtomicBoolean(false)
                    var dlg: android.app.AlertDialog? = null
                    // [H] 等待期间监测对端是否已断开 ——
                    // PC 点「不一致」会立刻发 pair_reject 并断连，旧实现下手机端的核对框
                    // 还要再挂满 60 秒。这里用一个只读探针线程守着 socket：
                    // 一旦对端发数据或断开 → 立刻关掉核对框（探针只读，不参与协议解析）。
                    val stopWatch = java.util.concurrent.atomic.AtomicBoolean(false)
                    val watcher = Thread {
                        try {
                            sock.soTimeout = 500
                            val input = sock.getInputStream()
                            while (!stopWatch.get()) {
                                val b = try {
                                    input.read()
                                } catch (e: java.net.SocketTimeoutException) {
                                    continue
                                } catch (e: Exception) {
                                    -1
                                }
                                if (b < 0) {
                                    peerGone.set(true) // 对端关闭了连接
                                    latch.countDown()
                                    break
                                }
                                // 等待期间对端只可能发 pair_reject（还没到 pair_confirm 阶段）
                                peerGone.set(true)
                                latch.countDown()
                                break
                            }
                        } catch (e: Exception) {
                        }
                    }
                    watcher.isDaemon = true
                    watcher.start()
                    runOnUiThread {
                        dlg = AlertDialog.Builder(this)
                            .setTitle("请在电脑上核对数字")
                            // [C1] 带上**对方电脑名**：同一局域网里可能跑着别人的 PhotoBeam，
                            // 用户要能核对「对面那台是不是我的电脑」。
                            .setMessage("对方电脑：${peerName.ifEmpty { "（名称未知）" }}\n\n" +
                                "电脑屏幕与手机应显示相同的 6 位数字：\n\n$sas\n\n" +
                                "两端一致请点『一致』；不一致请点『不一致』。\n" +
                                "（60 秒内未确认会自动取消）")
                            .setPositiveButton("一致") { _, _ ->
                                android.util.Log.d("PhotoBeam", "用户点了一致")
                                agreed = true; latch.countDown() }
                            .setNegativeButton("不一致") { _, _ -> agreed = false; latch.countDown() }
                            .setOnCancelListener { latch.countDown() }
                            .show()
                    }
                    val answered = latch.await(60, java.util.concurrent.TimeUnit.SECONDS)
                    // 探针必须先退出，否则会和后续握手抢同一个 socket
                    stopWatch.set(true)
                    try { watcher.join(1500) } catch (e: Exception) { }
                    sock.soTimeout = 90_000
                    if (!answered) {
                        // [Stage 5 修复] 等待超时后必须把对话框关掉：
                        // 否则屏幕上会留一个已经失效的核对框，用户再点它也没有任何作用。
                        sasTimedOut = true
                        android.util.Log.w("PhotoBeam", "SAS 核对超时（60s），自动取消")
                        runOnUiThread { dlg?.dismiss() }
                        return@handshake SAS_TIMEOUT
                    }
                    if (peerGone.get()) {
                        // [H] 对方已经拒绝/断开 → 不再发 pair_reject（连接已经没了）
                        peerRejected = true
                        runOnUiThread { dlg?.dismiss() }
                        return@handshake SAS_TIMEOUT
                    }
                    if (agreed) SAS_CONFIRM else SAS_REJECT
                })
                // [Stage 5 第五轮] PC 可能用 MODE.err 明确拒绝（例如 receiver_not_ready）
                if (res.versionErr != null) {
                    peerRejected = true
                    peerRejectReason = res.versionErr
                } else {
                // 设备记录已在 PairingEngine.handshake 内部持久化（协议 §3.3）
                SenderService.start(applicationContext)
                runOnUiThread {
                    statusView.text = "配对成功！截图/存图会自动同步到电脑。"
                    refreshStatus()
                }
                }
            } catch (e: Exception) {
                runOnUiThread {
                    statusView.text = when {
                        sasTimedOut ->
                            "配对超时：60 秒内没有完成核对。\n请确认电脑上的配对窗口还在，再点一次 ②。"
                        peerRejected ->
                            // [Stage 5 第五轮] PC 明确拒绝了（未在待命态 / 版本不符 / 对方已断开）
                            when (peerRejectReason) {
                                "receiver_not_ready" ->
                                    "对方电脑**没有在等待配对**。\n" +
                                        "请在电脑上点托盘图标 →「配对新设备」，然后再点一次 ②。"
                                "version_unsupported" ->
                                    "对方电脑上的 PhotoBeam 版本太旧，请更新后再试。"
                                else ->
                                    "对方电脑已经拒绝或断开了配对（连接已关闭）。\n请重新点一次 ②。"
                            }
                        else ->
                            // [A10] 原始异常可能是英文/技术串 → 换成中文说明
                            friendlyPairError(e)
                    }
                }
            }
        }.start()
    }
}