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
import android.provider.Settings
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.widget.Button
import android.widget.LinearLayout
import android.widget.TextView

object Notifier {
    const val CH_EVENTS = "photobeam_events"
    fun channel(ctx: Context): NotificationManager {
        val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.createNotificationChannel(
            NotificationChannel(CH_EVENTS, "事件通知", NotificationManager.IMPORTANCE_DEFAULT))
        return nm
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
}

class MainActivity : Activity() {
    private lateinit var prefs: android.content.SharedPreferences
    private lateinit var queue: QueueStore
    private lateinit var statusView: TextView

    private val needed = mutableListOf(
        Manifest.permission.READ_MEDIA_IMAGES,
        Manifest.permission.POST_NOTIFICATIONS)

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

        val bPerm = btn("① 授权照片与通知访问")
        val bPair = btn("② 配对电脑（输入/核对 6 位数字）")
        val bSync = btn("③ 立即同步")
        val bService = btn("后台同步服务：点此启动/刷新")
        val bBattery = btn("④ 允许后台常驻（关闭电池优化）")

        root.addView(title)
        root.addView(statusView)
        root.addView(bPerm)
        root.addView(bPair)
        root.addView(bSync)
        root.addView(bService)
        root.addView(bBattery)
        setContentView(root)

        bPerm.setOnClickListener {
            requestPermissions(needed.toTypedArray(), 100)
        }
        bPair.setOnClickListener { pairFlow() }
        bSync.setOnClickListener {
            SenderService.start(this)
            statusView.text = "已触发同步，请稍候…"
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
                    .setData(android.net.Uri.parse("package:$packageName")),
                // MIUI 自启动/省电策略管理页
                Intent("miui.intent.action.OP_AUTO_START"),
                Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS),
                Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                    .setData(android.net.Uri.parse("package:$packageName"))
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

        requestPermissions(needed.toTypedArray(), 100)
        refreshStatus()
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        refreshStatus()
    }

    override fun onResume() {
        super.onResume()
        // [修复 2026-11-02] 应用回到前台 = 允许后台启动 FGS 的时机：
        // 顺便把"降级态"（startForeground 被拒）自动恢复成正规前台服务通知。
        SenderService.start(this, SenderService.ACTION_ENSURE_FGS)
        refreshStatus()
    }

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
        statusView.text = buildString {
            append("电脑：${paired ?: "未配对"}\n")
            append("待发送：$pending 张\n")
            append("服务：${if (SenderService.running) "运行中" else "未运行"}\n")
            // 状态栏图标状态如实显示：图标亮 = 已连接，所以图标本身必须一直在
            append("状态栏图标：${when (mode) {
                "fgs" -> "正常（前台服务）"
                "fallback" -> "降级（普通通知）"
                else -> "无（服务未运行）"
            }}\n")
            append("后台常驻：${if (exempt) "已放行（可后台自启）" else "未放行（后台自启会被拦）"}\n")
            if (!exempt) {
                append("⚠ 未加入电池优化白名单 → 点 ④ 允许后台常驻，否则图标会时有时无\n")
            } else if (mode != "fgs") {
                append("⚠ 状态栏处于降级态：点『后台同步服务』恢复正规前台服务\n")
            }
            val denied = needed.filter {
                checkSelfPermission(it) != PackageManager.PERMISSION_GRANTED
            }
            if (denied.isNotEmpty()) append("⚠ 尚有权限未授予（点 ①）")
        }
    }

    /** 是否已加入电池优化白名单（= deviceidle whitelist） */
    private fun isIgnoringBatteryOptimizations(): Boolean = try {
        val pm = getSystemService(Context.POWER_SERVICE) as android.os.PowerManager
        pm.isIgnoringBatteryOptimizations(packageName)
    } catch (e: Exception) {
        false
    }

    /** 配对流程：发现 → 握手 → SAS 双端比对 → 保存 */
    private fun pairFlow() {
        statusView.text = "正在发现电脑…"
        Thread {
            try {
                val lastIP = prefs.getString("pc_ip", null)
                val lastPort = prefs.getInt("pc_port", 0)
                val pc = Discovery.resolve(this, lastIP, lastPort)
                if (pc == null) {
                    runOnUiThread {
                        statusView.text = "未发现电脑。\n请确认：手机与电脑连同一个 WiFi；" +
                            "电脑端 PhotoBeam 正在运行。\n（或手动输入电脑 IP，见电脑托盘诊断信息）"
                    }
                    return@Thread
                }


                runOnUiThread { statusView.text = "已发现 ${pc.name.ifEmpty { pc.ip }}，正在配对…" }

                val engine = PairingEngine(prefs, IdentityKeys.loadOrCreate(prefs), Build.MODEL)
                val sock = java.net.Socket()
                sock.tcpNoDelay = true
                sock.connect(java.net.InetSocketAddress(pc.ip, pc.port), 3000)
                sock.soTimeout = 20_000
                val res = engine.handshake(sock, sasConfirm = { sas ->
                    // UI 线程弹 SAS 比对对话框，同步等待用户确认（PC 端 15s 握手超时内回答）
                    android.util.Log.d("PhotoBeam", "SAS 回调进入: $sas")
                    val latch = java.util.concurrent.CountDownLatch(1)
                    var agreed = false
                    runOnUiThread {
                        AlertDialog.Builder(this)
                            .setTitle("请在电脑上核对数字")
                            .setMessage("电脑屏幕与手机应显示相同的 6 位数字：\n\n$sas\n\n" +
                                "两端一致请点『一致』；不一致请点『不一致』。")
                            .setPositiveButton("一致") { _, _ ->
                                android.util.Log.d("PhotoBeam", "用户点了一致")
                                agreed = true; latch.countDown() }
                            .setNegativeButton("不一致") { _, _ -> agreed = false; latch.countDown() }
                            .setOnCancelListener { latch.countDown() }
                            .show()
                    }
                    latch.await(15, java.util.concurrent.TimeUnit.SECONDS)
                    agreed
                })
                // 设备记录已在 PairingEngine.handshake 内部持久化（协议 §3.3）
                SenderService.start(applicationContext)
                runOnUiThread {
                    statusView.text = "配对成功！截图/存图会自动同步到电脑。"
                    refreshStatus()
                }
            } catch (e: Exception) {
                runOnUiThread {
                    statusView.text = "配对失败：${e.message}\n请重试（②）"
                }
            }
        }.start()
    }
}
