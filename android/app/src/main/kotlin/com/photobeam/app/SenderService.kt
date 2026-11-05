package com.photobeam.app

// 前台服务（specialUse/dataSync）+ 捕获 + 发送循环 + 开机自启
// 架构：手机主动按需发送即探活（无独立 probeLink）——发送成功 = 链路通；
// 发送/发现失败 = 链路断（通知图标熄灭），队列保留，30s 后自动重试 [协议 §2/§7]

import android.app.AlarmManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.provider.MediaStore
import java.net.Socket

class SenderService : Service() {
    companion object {
        const val CH_ON = "photobeam_fg_on_v2" // 已连接（V2：LOW——状态栏图标照常显示但不响铃；旧 id 已提交 DEFAULT 不可改，审核§七②）
        const val CH_OFF = "photobeam_fg_off"  // 未连接（状态栏无图标，仅下拉可见）
        /** 降级/故障提示通道：IMPORTANCE_MIN → 只在下拉通知里可见，**不占状态栏**。
         *  这样"状态栏图标"这个位置只承载"已连接"这一个语义，用户不会误读。 */
        const val CH_DOWN = "photobeam_fg_down"
        const val ACTION_STOP = "photobeam.stop"
        /** 应用已在前台 → 重试恢复正规前台服务通知（解除降级态） */
        const val ACTION_ENSURE_FGS = "photobeam.ensure.fgs"
        /** ③ 立即同步 → 立刻跑一轮对账+冲刷，并把结果写回 prefs 供 UI 回显 [A3] */
        const val ACTION_SYNC_NOW = "photobeam.sync.now"
        /** 正规前台服务通知 id */
        const val NOTI_ID = 1
        /** 降级（普通通知）id：必须与 NOTI_ID 分离。
         *  [修复 2026-11-02 v2] 被系统"静默拒绝"的 startForeground 会**异步 cancel 掉同 id 的通知**，
         *  若降级通知复用同一个 id，就会被系统的取消动作连带抹掉 → 图标刚亮又灭。
         *  实测证据：08:34:29 降级通道切到 fg_on_v2 后图标亮起，但同 id 被系统异步 cancel。
         */
        const val NOTI_ID_FALLBACK = 2
        /** "后台服务未能启动"提示通知 id（与状态图标分开，互不干扰） */
        const val NOTI_ID_DOWN = 3
        /** 降级通知的自动清理时限（ms）：
         *  普通通知**不会**随进程死亡一起消失 → 若进程异常死亡，状态栏会留下
         *  "以为还连着"的假图标，破坏"图标亮=已连接"的语义。
         *  用超时兜底：服务活着时每 30s 对账会重新发布并顺延时限。
         */
        const val FALLBACK_TIMEOUT_MS = 120_000L
        var running = false

        /**
         * 状态栏图标当前是靠什么撑着的：
         *  fgs      = 正常前台服务通知
         *  fallback = startForeground 被系统拒绝（Android 12+ 后台启动限制）→ 退化为普通通知，图标照常可见
         *  none     = 没有图标（例如服务根本没起来）
         * 供 MainActivity 如实显示，避免"图标=已连接"的语义被破坏。
         */
        @Volatile var iconMode = "none"

        // ---------- [A6] 前台服务超时（Android 14+）降级开关 ----------

        /** 前台服务被系统超时停止的连续次数（prefs key） */
        const val PREFS_TIMEOUT_STREAK = "fgs_timeout_streak"
        /** 「仅闹钟模式」开关（prefs key）：true = 不再申请前台服务身份，只靠心跳闹钟拉起 */
        const val PREFS_ALARM_ONLY = "alarm_only"
        /** 连续超时多少次后降级为「仅闹钟模式」 */
        const val TIMEOUT_DEGRADE_THRESHOLD = 3

        /**
         * 「仅闹钟模式」：Android 14+ 上 dataSync/specialUse 前台服务会被系统超时停止 →
         * onDestroy 撤通知 → **状态栏图标整体消失**（Stage 4 修好的失效模式在 API 34+ 复发）。
         * 连续超时 [TIMEOUT_DEGRADE_THRESHOLD] 次后置位：不再 startForeground（再申请还是会被超时杀），
         * 只靠心跳闹钟拉起服务。应用回到前台时自动解除（见 onStartCommand 的 ACTION_ENSURE_FGS）。
         */
        @Volatile var alarmOnlyMode = false

        /**
         * 「停止」动作用途的服务实例标记。
         * 用它避免为"马上要停的服务"先建一次前台服务通知（状态栏图标会闪一下）。
         */
        @Volatile var stopRequested = false

        /** 上传动画 4 帧（独立 drawable，逐帧切换以保证状态栏重绘） */
        val UPLOAD_FRAMES = intArrayOf(
            R.drawable.ic_stat_up_0, R.drawable.ic_stat_up_1,
            R.drawable.ic_stat_up_2, R.drawable.ic_stat_up_3
        )
        fun start(ctx: Context, action: String? = null) {
            val i = Intent(ctx, SenderService::class.java)
            if (action != null) i.action = action
            // [A1] 停止动作：**不能**用 startForegroundService —— 系统会等 5 秒内 startForeground()，
            // 而服务马上就 stopSelf 了 → 触发 ForegroundServiceDidNotStartInTime（正是 Stage 4 修掉的坑）。
            // 停止不需要前台服务身份，用普通 startService 即可。
            if (action == ACTION_STOP) {
                stopRequested = true
                try {
                    ctx.startService(i)
                } catch (e: Exception) {
                    android.util.Log.w("PhotoBeam", "停止动作启动服务失败: ${e.message}")
                    try { ctx.stopService(i) } catch (e2: Exception) { }
                    stopRequested = false
                }
                return
            }
            // [A6] 仅闹钟模式：不再申请前台服务身份（Android 14+ 连续超时后系统会停掉前台服务 → 图标消失）。
            // 普通 startService 不会触发"必须调 startForeground"的硬约束 → 不会自杀式崩溃。
            if (alarmOnlyMode) {
                try {
                    ctx.startService(i)
                    android.util.Log.d("PhotoBeam", "仅闹钟模式启动服务（非前台服务）")
                } catch (e: Exception) {
                    android.util.Log.w("PhotoBeam", "仅闹钟模式启动服务被拒: ${e.message}")
                }
                return
            }
            try {
                ctx.startForegroundService(i)
            } catch (e: Exception) {
                // [修复 2026-11-02 v2] startForegroundService 被拒（Android 14+ 禁止从
                // BOOT_COMPLETED 起 FGS；或 MIUI 后台管控）→ 退化成**普通后台服务**：
                // 服务照样能起来、能发降级通知、能跑同步循环，只是拿不到 FGS 身份。
                android.util.Log.w("PhotoBeam", "startForegroundService 被拒: ${e.message} → 退化为后台服务")
                try {
                    ctx.startService(i)
                } catch (e2: Exception) {
                    // 连"启动后台服务"都被拒 → 服务起不来，
                    // 至少把状态如实显示出来（图标=未连接），别让用户面对空白。
                    android.util.Log.w("PhotoBeam", "startService 也被拒: ${e2.message}")
                    iconMode = "none"
                    postServiceDownNotice(ctx, e2.message ?: "后台启动被系统限制")
                }
            }
        }

        /** 服务起不来时的兜底通知：如实显示"服务未运行"，点一下可打开应用 */
        fun postServiceDownNotice(ctx: Context, reason: String) {
            try {
                val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
                nm.createNotificationChannel(
                    NotificationChannel(CH_DOWN, "PhotoBeam · 服务未运行", NotificationManager.IMPORTANCE_MIN))
                val pi = PendingIntent.getActivity(
                    ctx, 0, Intent(ctx, MainActivity::class.java),
                    PendingIntent.FLAG_IMMUTABLE)
                // 用独立 id（NOTI_ID_DOWN）：不能占用状态栏通道 CH_OFF，
                // 否则"服务没起来"会和"已连接"共用一个状态栏图标位，破坏"图标⟺已连接"语义。
                nm.notify(NOTI_ID_DOWN, Notification.Builder(ctx, CH_DOWN)
                    .setContentTitle("PhotoBeam")
                    .setContentText("后台同步服务未能启动 · 点此打开应用")
                    .setSmallIcon(R.drawable.ic_stat_p_off)
                    .setContentIntent(pi)
                    .build())
            } catch (e: Exception) {
                android.util.Log.w("PhotoBeam", "兜底通知失败", e)
            }
        }
    }

    private lateinit var queue: QueueStore
    private lateinit var sync: MediaStoreSync
    private lateinit var pairing: PairingEngine
    private lateinit var prefs: android.content.SharedPreferences
    private val handler = Handler(Looper.getMainLooper())
    private val workExecutor = java.util.concurrent.Executors.newSingleThreadExecutor()
    private var linkOk = false // 与电脑的连接状态（通知图标 = 此状态）
    // [修复 2026-11-02 v2] startForeground 曾经被系统拒绝过 → 仅作**UI 提示**，
    // **绝不再用于"跳过 startForeground()"**：
    //   跳过 = 30 秒后 ForegroundServiceDidNotStartInTimeException 杀进程（实测两次），
    //   而杀进程正是"状态栏图标整体消失"的直接原因。
    //   现在每次 postStatus 都无条件尝试 startForeground，被拒就降级成普通通知。
    @Volatile private var fgsDenied = false
    private var flushedSomething = false // 本轮冲刷是否真的发出过图（空队列时用轻探测代替 [F-08]）
    // 传输动画 [用户需求]：仅在上传期间播放（4 帧推进），空闲静止
    private var uploading = false
    private val animHandler = android.os.Handler(android.os.Looper.getMainLooper())
    private var animLevel = 0
    private val animTick = object : Runnable {
        override fun run() {
            if (!uploading) return
            animLevel = (animLevel + 1) % 4
            try { startForegroundCompat(animLevel) } catch (e: Exception) { }
            animHandler.postDelayed(this, 180)
        }
    }
    private fun setUploading(v: Boolean) {
        if (uploading == v) return
        uploading = v
        animHandler.removeCallbacks(animTick)
        if (v) {
            animLevel = 0
            // [修复 2026-11-02] 立即刷新通知显示第 0 帧（原实现只把 animTick 排到 180ms 后，
            // 单张实测传输仅 127ms < 180ms → animTick 一次都没跑，传输中状态栏仍是普通白 P，
            // "上传动画"实际从未出现过）。先补上第 0 帧，任何长度的传输都能看到上传态图标。
            try { startForegroundCompat(0) } catch (e: Exception) { }
            animHandler.postDelayed(animTick, 180)
        } else { try { startForegroundCompat() } catch (e: Exception) { } }
    }

    private val tick = object : Runnable {
        override fun run() {
            reconcileAndFlush()
            handler.postDelayed(this, 30_000)
        }
    }

    /** MediaStore 变更去抖合并用（见 onCreate 里的注释） */
    private val flushRunnable = Runnable { reconcileAndFlush() }

    /**
     * [Stage 6 · 延迟优化] 自适应去抖：单张截图立即发，连拍仍去抖合并。
     *
     * 原实现固定 250ms 去抖（为解决「连拍 N 张 = N 次回调 → 每张单独建连接 →
     * 180ms/帧的上传动画永远停在第 0 帧」）。但固定 250ms 对**单张截图**是纯浪费：
     * 实测端到端延迟中，这 250ms 白占了 7%。
     *
     * 新策略：观察到一个 MediaStore 变更时，判断「队列里是否已有待发项」——
     *   · 没有（单张截图，队列空）→ **立即冲刷**，不等去抖 → 单张延迟少 250ms；
     *   · 有（连拍/积压）→ 沿用 250ms 去抖合并 → 保持「一批图走同一条连接」的行为，
     *     上传动画不受影响。
     *
     * 去抖窗口内的后续回调仍会取消并重排 flushRunnable（见 onCreate），
     * 所以连拍场景下依然是「最后一次变更后 250ms 冲刷」。
     */
    private fun scheduleAdaptiveFlush() {
        handler.removeCallbacks(flushRunnable)
        val pending = queue.pendingCount()
        if (pending == 0) {
            // 队列空 = 用户刚截了一张图 → 不等去抖，立刻发
            handler.post(flushRunnable)
        } else {
            // 队列有积压 = 连拍/上一批还没发完 → 去抖合并，避免每张单独建连接
            handler.postDelayed(flushRunnable, 250)
        }
        // [Stage 6 · 缺陷修复兜底] 本轮若遇到 is_pending=1 的行（文件还没写完），
        // 那张图这轮入不了队。正常情况 MediaStore 清零 is_pending 时会再次触发
        // ContentObserver；但部分 ROM 不通知 UPDATE 变更 → 补一次 800ms 后的重扫，
        // 保证「pending 图永远不会丢」不依赖 ROM 的通知行为。
        if (sync.sawPendingRow) {
            handler.removeCallbacks(rescanRunnable)
            handler.postDelayed(rescanRunnable, 800)
        }
    }

    /** pending 行重扫兜底（见 scheduleAdaptiveFlush 的说明） */
    private val rescanRunnable = Runnable { reconcileAndFlush() }

    override fun onCreate() {
        super.onCreate()
        running = true
        prefs = getSharedPreferences("photobeam", Context.MODE_PRIVATE)
        // [A1] 「停止」动作用途的实例：不建前台服务通知、不挂心跳闹钟（马上要停，别闪图标）
        val stopping = stopRequested
        stopRequested = false
        // [A6] 恢复「仅闹钟模式」标记（降级态跨进程保留）
        alarmOnlyMode = try { prefs.getBoolean(PREFS_ALARM_ONLY, false) } catch (e: Exception) { false }
        // 记住"后台 startForeground 已被系统拒绝"：闹钟拉起的是新实例，
        // 不记住就会每次都去试 → 又触发系统对同 id 通知的异步取消 → 图标闪没。
        fgsDenied = try { prefs.getBoolean("fgs_denied", false) } catch (e: Exception) { false }
        queue = QueueStore(this).also { it.load() }
        IdentityKeys.loadOrCreate(prefs)
        pairing = PairingEngine(prefs, IdentityKeys.loadOrCreate(prefs), android.os.Build.MODEL)
        sync = MediaStoreSync(this, queue) {
            // [修复 2026-11-02] MediaStore 每次变更都会回调 onChange（连拍 N 张 = N 次回调）。
            // 原实现每次回调都立刻 reconcile+flush → 每张图单独建一次连接（实测每次 ~100ms），
            // uploading 被切成上百个 ~100ms 的碎片，180ms/帧的动画永远停在第 0 帧
            // （实测 dumpsys 只看到 ic_stat_up_0，状态栏看不到波点变化）。
            // 改为 250ms 去抖合并：一次冲刷把这一批图放在同一条连接里发完，
            // uploading 持续数秒 → 4 帧动画真正转起来；
            // 发送延迟 +250ms，仍远在协议 §9「截图 ≤3 秒落盘」之内。
            scheduleAdaptiveFlush()
        }
        sync.register() // [E-01/C-04] ContentObserver 推送：截图落库即触发对账（不能只靠 30s 轮询）
        // 网络恢复即对账+发送 [协议 §7.2]
        val cm = getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        cm.registerNetworkCallback(
            NetworkRequest.Builder().addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET).build(),
            object : ConnectivityManager.NetworkCallback() {
                override fun onAvailable(network: Network) {
                    reconcileAndFlush()
                }
            })
        // [A1] 「停止」动作拉起的实例：跳过前台服务通知与心跳排程（onStartCommand 会 stopSelf）
        if (!stopping) {
            startForegroundCompat()
            scheduleHeartbeat()
            handler.post(tick)
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // [A1] 「停止」标记只对本次启动有效：
        // 服务本来就在跑时 onCreate 不会执行（标记不会被消费），必须在这里清掉，
        // 否则下一次闹钟拉起的新实例会误判成"停止用途"→ 不建前台服务通知、不挂心跳。
        stopRequested = false
        if (intent?.action == ACTION_STOP) {
            // [A1] 停止同步服务：撤掉心跳闹钟链（否则 5 分钟内又被闹钟拉起）+ 停服务
            // → onDestroy 撤通知 → iconMode="none" → 状态栏图标消失（如实反映"服务未运行"）。
            android.util.Log.d("PhotoBeam", "收到停止动作 → 停止同步服务")
            cancelHeartbeat(this)
            stopSelf()
            return START_NOT_STICKY
        }
        // [A3] ③ 立即同步：立刻跑一轮对账+冲刷，结束时把结果写回 prefs（UI 轮询回显）
        if (intent?.action == ACTION_SYNC_NOW) {
            android.util.Log.d("PhotoBeam", "收到立即同步请求")
            reconcileAndFlush(manual = true)
        }
        // [修复 2026-11-02] 服务已在跑时再被拉起（用户打开应用 / ④ 按钮 / 闹钟），
        // 重新发一次状态通知：若之前是"后台被拒"的降级态，此时应用在前台，
        // startForeground 会成功 → 自动恢复成正规前台服务通知。
        if (intent?.action == ACTION_ENSURE_FGS) {
            // 用户把应用拉到前台了 → 强制重试一次正规前台服务通知（此时系统允许 FGS）
            android.util.Log.d("PhotoBeam", "前台重试 FGS")
            // [A6] 应用回到前台 = 用户有机会处理省电白名单 → 解除「仅闹钟模式」降级态，
            // 恢复正规前台服务（若还是被系统超时，连续计数会重新开始）。
            if (alarmOnlyMode) {
                alarmOnlyMode = false
                try { prefs.edit().putBoolean(PREFS_ALARM_ONLY, false).apply() } catch (e: Exception) { }
                android.util.Log.d("PhotoBeam", "应用回到前台 → 解除仅闹钟降级态")
            }
        }
        try { startForegroundCompat() } catch (e: Exception) { }
        return START_STICKY
    }

    /**
     * Android 14+ 前台服务超时回调（协议 §7.5 [R-16]）
     *
     * 系统超时后会**停掉服务** → onDestroy 撤掉通知 → **状态栏图标整体消失**
     * （这正是 Stage 4 刚修好的失效模式在 API 34+ 复发），只能等心跳闹钟（≤5 分钟）重拉。
     *
     * 这里：记日志 → 重新拉起服务（走 startForegroundCompat 那条路）→ 维护「连续超时次数」；
     * 连续 [TIMEOUT_DEGRADE_THRESHOLD] 次 → 降级为「仅闹钟模式」（不再 startForeground，
     * 只靠心跳闹钟拉起），并用 Notifier 事件通道通知用户去授予省电白名单。
     *
     * 注：API 33 上系统没有这个回调（minSdk=33，不会被调用）。
     */
    override fun onTimeout(startId: Int, fgsType: Int) {
        super.onTimeout(startId, fgsType)
        val streak = try { prefs.getInt(PREFS_TIMEOUT_STREAK, 0) + 1 } catch (e: Exception) { 1 }
        try { prefs.edit().putInt(PREFS_TIMEOUT_STREAK, streak).apply() } catch (e: Exception) { }
        android.util.Log.w("PhotoBeam",
            "前台服务被系统超时停止: startId=$startId fgsType=$fgsType 连续=$streak")
        try {
            if (streak >= TIMEOUT_DEGRADE_THRESHOLD) {
                alarmOnlyMode = true
                try { prefs.edit().putBoolean(PREFS_ALARM_ONLY, true).apply() } catch (e: Exception) { }
                android.util.Log.w("PhotoBeam", "连续超时 $streak 次 → 降级为仅闹钟模式")
                // 提示用户：授予省电白名单后前台服务才不会被反复超时
                Notifier.batteryAdvice(applicationContext)
            }
            // 重新拉起服务（已降级则只走普通 startService，不申请前台服务身份）
            SenderService.start(applicationContext)
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "超时后重拉服务失败: ${e.message}")
        }
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onDestroy() {
        running = false
        // [修复 2026-11-02] 服务没了就别留图标：否则降级成普通通知后，
        // 进程被杀/服务停止时状态栏还挂着"已连接"的旧图标 → 破坏"图标=已连接"的语义。
        // 两个 id 都要撤（FGS id=1 + 降级 id=2）。
        try {
            (getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager)
                .apply {
                    cancel(NOTI_ID)
                    cancel(NOTI_ID_FALLBACK)
                }
        } catch (e: Exception) { }
        iconMode = "none"
        try { prefs.edit().putString("icon_mode", "none").apply() } catch (e: Exception) { }
        sync.unregister()
        handler.removeCallbacksAndMessages(null)
        super.onDestroy()
    }

    // ---------- 通知（连接状态指示灯） ----------

    private fun startForegroundCompat(iconLevel: Int = -1) {
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.createNotificationChannel(
            NotificationChannel(CH_ON, "PhotoBeam · 已连接", NotificationManager.IMPORTANCE_LOW))
        nm.createNotificationChannel(
            NotificationChannel(CH_OFF, "PhotoBeam · 未连接", NotificationManager.IMPORTANCE_MIN))
        val pending = pendingIntent(Intent(this, MainActivity::class.java))
        // [A1] 通知里的「停止」动作：不打开应用也能停掉服务。
        // 用 BroadcastReceiver + stopService（后台 stopService 不受"后台不能起服务"限制）。
        val stopPi = PendingIntent.getBroadcast(this, 3,
            Intent(this, StopReceiver::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE)
        val pc = pairing.loadPairedPC()
        val on = linkOk && pc != null
        val chId = if (on) CH_ON else CH_OFF
        // 图标 [用户需求]：自绘空心 P（不再用系统上传箭头，防撞车/误会）
        val icon = when {
            uploading -> UPLOAD_FRAMES[0]
            on -> R.drawable.ic_stat_p
            else -> R.drawable.ic_stat_p_off
        }
        val text = when {
            pc == null -> "未配对 · 打开应用完成配对"
            on -> "已连接 ${pc.name} · 截图/存图将自动同步" +
                (if (queue.pendingCount() > 0) " · 待发送 ${queue.pendingCount()} 张" else "")
            else -> "未连接电脑 · 图片将保存在队列，恢复连接后自动补发" +
                (if (queue.pendingCount() > 0) " · 待补发 ${queue.pendingCount()} 张" else "")
        }
        // 同一个 channel / icon / 文案 → 降级通知与 FGS 通知的"可见性语义"完全一致：
        //   状态栏出现 P ⟺ 已连接（CH_ON=LOW 显示图标；CH_OFF=MIN 只在下拉可见）。
        // 唯一差别：降级版用独立 id + 自动过期（见 postStatus 注释）。
        fun build(timeoutMs: Long): Notification =
            Notification.Builder(this, chId)
                .setContentTitle("PhotoBeam")
                .setContentText(notifyText ?: text)
                .setContentIntent(pending)
                .setOngoing(true)
                // [A1] 通知动作：停止同步服务（不打开应用也能停）
                .addAction(android.R.drawable.ic_menu_close_clear_cancel, "停止", stopPi)
                .setSmallIcon(
                    if (iconLevel >= 0 && uploading)
                        UPLOAD_FRAMES[animLevel % UPLOAD_FRAMES.size]
                    else icon
                )
                .apply { if (timeoutMs > 0) setTimeoutAfter(timeoutMs) }
                .build()
        postStatus(build(0), build(FALLBACK_TIMEOUT_MS))
    }

    /**
     * 发布状态通知 [修复 2026-11-02 v2]
     *
     * ══ v1 的两个致命缺陷（均已实机复现）══
     *
     * 缺陷①【自造崩溃·致命】v1 用持久化的 `fgs_denied` 标志决定"是否调用 startForeground()"，
     *   一旦置 true 就**永久跳过** startForeground()。但 startForegroundService() 已经调用过 →
     *   系统等 30 秒后抛 ForegroundServiceDidNotStartInTimeException **直接杀进程**：
     *     08:20:15 拉起 → 08:20:45 "Bringing down service while still waiting for start foreground"
     *                  → RemoteServiceException$ForegroundServiceDidNotStartInTimeException
     *     08:34:29 拉起 → 08:34:59 同样崩溃（加了白名单也没用，因为压根没调 startForeground）
     *   → 进程一死，通知随之消失，**状态栏图标整体消失**。
     *   ⇒ 结论：图标消失的**直接原因不是系统拒绝，而是我们自己的跳过逻辑导致的崩溃**。
     *
     * 缺陷②【通知 id 冲突】降级通知与 FGS 通知复用同一个 id=1：
     *   被系统拒绝的 startForeground 会**异步 cancel 掉同 id 的通知**，
     *   降级通知刚发出去就被系统的取消动作连带抹掉 → 图标闪一下又消失。
     *
     * ══ v2 修法 ══
     *  ① startForeground() **无条件调用**（try/catch 兜异常），绝不再因历史标志而跳过。
     *     这既满足"startForegroundService 后 5 秒内必须调 startForeground"的硬约束，
     *     又能在系统真正拒绝时（抛异常）安全降级。
     *  ② FGS 通知与降级通知用**不同 id**（1 / 2），系统异步 cancel id=1 不会波及降级通知。
     *  ③ 降级通知带 setTimeoutAfter：进程若被系统杀掉，普通通知不会随进程消失，
     *     必须让它自动过期，否则状态栏会留下"以为还连着"的假图标。
     */
    private fun postStatus(n: Notification, fallback: Notification) {
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        // [A6] 仅闹钟模式：Android 14+ 前台服务被系统超时停止后，连续 3 次即降级。
        // 此时**不再**调用 startForeground()：
        //   ① 再申请身份 → 系统照样超时再杀（图标整体消失的循环）
        //   ② 跳过 startForeground 而服务又是 startForegroundService 起的 → 30 秒后
        //      ForegroundServiceDidNotStartInTime 杀进程（Stage 4 修掉的坑，不能再踩）
        // 降级期间只发普通通知（独立 id=2，带自动过期），靠心跳闹钟拉起服务。
        if (alarmOnlyMode) {
            try { nm.cancel(NOTI_ID) } catch (e: Exception) { }
            try { nm.notify(NOTI_ID_FALLBACK, fallback) } catch (e: Exception) { }
            iconMode = "alarm"
            try { prefs.edit().putString("icon_mode", iconMode).apply() } catch (e: Exception) { }
            android.util.Log.d("PhotoBeam", "状态通知已发 mode=$iconMode（仅闹钟模式）")
            return
        }
        // ① 无条件调用 startForeground() —— 这是 startForegroundService() 的硬性对价：
        //    跳过它 = 30 秒后被系统杀进程（见上方缺陷①）。
        //    v1 用持久化 fgsDenied 标志跳过 → 08:20:45 / 08:34:59 两次
        //    ForegroundServiceDidNotStartInTimeException 杀进程 → 状态栏图标整体消失。
        //    实测反证：两次崩溃前 AM 都已放行 "Background started FGS: Allowed"
        //    （08:20 是 PACKAGE_REPLACED 临时许可，08:34 是 SYSTEM_ALLOW_LISTED 永久许可）
        //    —— 系统从头到尾**没有拒绝过**，是我们自己没调。
        //
        // ② **不做** activeNotifications 复核（实测是假阴性）：
        //    09:27 实测 startForeground 成功、dumpsys 里 id=1 的 NotificationRecord
        //    flags=0x62（含 FLAG_FOREGROUND_SERVICE）确实存在，
        //    但 nm.activeNotifications 查不到它 → 误判成"被拒"→ 白白多发一条 id=2
        //    → 状态栏出现**两个** PhotoBeam P 图标。
        //    判据改为：startForeground() 抛异常 = 真被拒；不抛 = 成功。
        var ok = true
        try {
            // API 34+ 用 specialUse；API 33 用 dataSync（协议 §7.5）
            if (android.os.Build.VERSION.SDK_INT >= 34) {
                startForeground(NOTI_ID, n,
                    android.content.pm.ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
            } else {
                startForeground(NOTI_ID, n,
                    android.content.pm.ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
            }
        } catch (e: Exception) {
            ok = false
            android.util.Log.w("PhotoBeam", "startForeground 被拒(${e.message}) → 降级为普通通知")
        }
        if (ok) {
            // 正规前台服务通知已建立 → 撤掉可能残留的降级通知（状态栏只留一个图标）
            try { nm.cancel(NOTI_ID_FALLBACK) } catch (e: Exception) { }
            iconMode = "fgs"
            fgsDenied = false
            try { prefs.edit().putBoolean("fgs_denied", false).apply() } catch (e: Exception) { }
            try { prefs.edit().putString("icon_mode", iconMode).apply() } catch (e: Exception) { }
            // [A6] 前台服务恢复正常 → 连续超时计数清零（"连续"的语义）
            try {
                if (prefs.getInt(PREFS_TIMEOUT_STREAK, 0) > 0) {
                    prefs.edit().putInt(PREFS_TIMEOUT_STREAK, 0).apply()
                }
            } catch (e: Exception) { }
            android.util.Log.d("PhotoBeam", "状态通知已发 mode=$iconMode（正规前台服务）")
        } else {
            postFallback(nm, fallback)
        }
    }

    /**
     * 降级为普通通知 [修复 2026-11-02 v2]
     *
     * ② 独立 id（NOTI_ID_FALLBACK）：被系统拒绝的 startForeground 会**异步 cancel 掉同 id 的通知**，
     *    同 id 会让降级通知刚发出就被抹掉 → 图标闪一下又消失。
     * ③ 带 setTimeoutAfter：普通通知**不随进程死亡消失**，若进程被系统杀掉，
     *    状态栏会留下"以为还连着"的假图标 → 自动过期兜底。
     * 通道/图标/文案与 FGS 完全一致 → "状态栏图标可见 ⟺ 已连接"语义不变。
     */
    private fun postFallback(nm: NotificationManager, fallback: Notification) {
        // startForeground 抛异常 = 系统真的拒绝了 → 此时没有 FGS 通知，
        // 直接发降级通知。**不要**用 activeNotifications 反查（实测假阴性，
        // 会导致"明明成功却多发一条"→ 状态栏两个 P 图标）。
        try { nm.notify(NOTI_ID_FALLBACK, fallback) } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "降级通知发布失败", e)
        }
        // 记录"被拒"（仅用于 UI 提示；**不再用于跳过 startForeground** —— 见缺陷①）
        fgsDenied = true
        try { prefs.edit().putBoolean("fgs_denied", true).apply() } catch (e: Exception) { }
        iconMode = "fallback"
        android.util.Log.d("PhotoBeam", "状态通知已发 mode=$iconMode（降级普通通知）")
        try { prefs.edit().putString("icon_mode", iconMode).apply() } catch (e: Exception) { }
    }

    private var notifyText: String? = null
    private fun updateNotification(text: String? = null) {
        notifyText = text
        try {
            startForegroundCompat() // 重发通知以切换通道/图标
        } catch (e: Exception) {
        }
    }

    private fun pendingIntent(i: Intent, flags: Int = 0): PendingIntent =
        PendingIntent.getActivity(this, 0, i, PendingIntent.FLAG_IMMUTABLE or flags)

    private fun scheduleHeartbeat() {
        // [修复 2026-11-02] 心跳改用**精确闹钟**（惰性闹钟在 MIUI 上连服务都拉不起来），
        // 且必须去重：每 30 秒的 reconcile 反复重排会把闹钟无限推后、永远不响。
        scheduleHeartbeatChain(this)
    }

    // ---------- 对账与发送（全部在 workExecutor 线程） ----------

    /**
     * 对账 + 冲刷一轮。
     * @param manual true = 由「③ 立即同步」触发（结果照样写回 prefs，UI 据此回显）
     */
    fun reconcileAndFlush(manual: Boolean = false) {
        workExecutor.execute {
            try {
                doReconcileAndFlush(manual)
            } catch (e: Exception) {
                android.util.Log.w("PhotoBeam", "reconcileAndFlush 异常", e)
            }
        }
    }

    /** 本轮真正发出去的张数（用于「已发送 N 张」回执） */
    private var flushedCount = 0

    /** 把本轮同步结果写进 prefs（MainActivity 轮询回显）[A3] */
    private fun recordSync(ok: Boolean, result: String, manual: Boolean) {
        try {
            prefs.edit()
                .putLong("last_sync_at", System.currentTimeMillis())
                .putBoolean("last_sync_ok", ok)
                .putString("last_sync_result", result)
                .putBoolean("last_sync_manual", manual)
                .apply()
            android.util.Log.d("PhotoBeam", "同步回执[$result] ok=$ok manual=$manual")
        } catch (e: Exception) { }
    }

    // ---------- [B3] 逐张「已到电脑」回执 ----------
    // 之前手机端只有「最近同步：HH:mm:ss 成功/失败」这种**汇总**回执，
    // 用户看不到「这一张图到底有没有到电脑」→ 补逐张回执。
    // PC 端每落盘一张就回一条 ACK（协议 §5.2），这里把它记进 prefs 供状态区回显。

    /** 一张图已被电脑确认落盘 */
    private fun recordArrival(name: String) {
        try {
            val n = prefs.getInt("arrived_count", 0) + 1
            prefs.edit()
                .putInt("arrived_count", n)
                .putString("arrived_last_name", name)
                .putLong("arrived_last_at", System.currentTimeMillis())
                // 成功一张就把「最近失败」清掉：失败提示只反映当前状态，不是历史
                .remove("failed_last_name")
                .remove("failed_last_reason")
                .apply()
            android.util.Log.d("PhotoBeam", "到达回执: $name（累计 $n 张）")
            // [E-a] 合并式通知：连续到达只更新同一条（低优先级通道，不连环响）
            try { Notifier.arrived(applicationContext, n, name) } catch (e: Exception) { }
        } catch (e: Exception) { }
    }

    /** 一张图被电脑拒绝/失败 */
    private fun recordArrivalFailure(name: String, reason: String) {
        try {
            prefs.edit()
                .putString("failed_last_name", name)
                .putString("failed_last_reason", reason)
                .putLong("failed_last_at", System.currentTimeMillis())
                .apply()
            android.util.Log.w("PhotoBeam", "失败回执: $name reason=$reason")
        } catch (e: Exception) { }
    }

    private fun doReconcileAndFlush(manual: Boolean = false) {
        val pc = pairing.loadPairedPC()
        // 未配对也要对账入队（协议 §7.1：截图先进队列，配对后补发）
        val n = sync.reconcile()
        if (pc == null) {
            linkOk = false
            android.util.Log.d("PhotoBeam", "未配对：对账新增 $n，队列 ${queue.pendingCount()}")
            updateNotification()
            scheduleHeartbeat()
            recordSync(false, "未配对", manual)
            return
        }
        // 网络指纹前置判定 [N-06][P1-02]：不一致 → 立即提示并跳过发送（<1s，不进三通道扫描）
        val fpNow = Discovery.currentFingerprint(this)
        val fpPaired = pairing.pairedNetworkFingerprint()
        if (fpPaired != null && fpNow != fpPaired) {
            linkOk = false
            android.util.Log.d("PhotoBeam", "网络指纹变化: $fpNow 不等于 $fpPaired，跳过发送")
            sync.reconcile()
            updateNotification("当前 WiFi 与配对时不同，请切回原网络")
            scheduleHeartbeat()
            recordSync(false, "网络已切换 · 已跳过发送", manual)
            return
        }
        android.util.Log.d("PhotoBeam", "对账新增 $n")
        assignBatches()
        val allOk = flushQueue()
        // 空队列时 flushQueue 不碰网络 → linkOk 会恒真（灯常亮不真实）[F-08]：
        // 队列空且本轮无发送成功时，做一次直连 TCP 轻探测更新连接状态
        linkOk = if (queue.pendingItems().isEmpty() && !flushedSomething) {
            probeLinkAlive(pc)
        } else {
            allOk
        }
        // [A3] 本轮回执：已发送 N 张 / 未连接电脑 · 队列 N 张 / 无可发送图片
        val result = when {
            flushedCount > 0 -> "已发送 $flushedCount 张"
            !allOk -> "未连接电脑 · 队列 ${queue.pendingCount()} 张"
            else -> "无可发送图片"
        }
        recordSync(allOk || flushedCount > 0, result, manual)
        updateNotification()
        scheduleHeartbeat() // AlarmManager 自检心跳链 [P0-13]
    }

    /** 轻量连通探测：直连上次 IP:Port 再立即关闭（<1.5s，不发送任何数据）[F-08] */
    private fun probeLinkAlive(pc: PairingEngine.PairedPC): Boolean {
        return try {
            val s = java.net.Socket()
            s.connect(java.net.InetSocketAddress(pc.lastIP, pc.lastPort), 1500)
            s.close()
            true
        } catch (e: Exception) {
            android.util.Log.d("PhotoBeam", "连通探测失败: ${e.message}")
            false
        }
    }

    /**
     * 批次语义 [P1-12][P0-04]：capturedAt（date_added 秒）间隔 ≤3s 同批；批内最后一张 is_latest=true。
     * [R-3] batch_id 入队即冻结：已带 batchId 的项不改号（只随新成员重算 is_latest/batch_size）；
     * 空批号新项与上一组间隔 ≤3s 则并入（沿用组批号），否则领新批号。
     */
    private fun assignBatches() {
        val pend = queue.pendingItems().sortedBy { it.capturedAt }
        var batch: String? = null
        var batchStart = 0L
        var members = ArrayList<QueueItem>()
        val closeBatch = { m: List<QueueItem> ->
            m.lastOrNull()?.isLatestInBatch = true
            for (q in m) q.batchSize = m.size
        }
        for (it in pend) {
            val joinPrev = batch != null && (it.capturedAt - batchStart) <= 3
            when {
                it.batchId.isNotEmpty() && it.batchId == batch -> members.add(it)
                it.batchId.isEmpty() && joinPrev -> {
                    it.batchId = batch!!
                    it.isLatestInBatch = false
                    members.add(it)
                }
                else -> {
                    closeBatch(members)
                    members = ArrayList()
                    if (it.batchId.isEmpty()) {
                        queue.batchSeq++
                        it.batchId = "${pairing.loadPairedPC()?.fp?.take(8) ?: "00000000"}-${queue.batchSeq}"
                    }
                    batch = it.batchId
                    batchStart = it.capturedAt
                    it.isLatestInBatch = false
                    members.add(it)
                }
            }
        }
        closeBatch(members)
        queue.save()
    }

    /** 冲刷队列 [C-10]：一次发现 + 一次连接复用，逐项就绪判定与传输 */
    private fun flushQueue(): Boolean {
        val pc = pairing.loadPairedPC() ?: return true
        var ok = true
        flushedSomething = false
        flushedCount = 0 // [A3] 本轮发送计数（回执用）
        val pend = queue.pendingItems().sortedBy { it.capturedAt }
        if (pend.isEmpty()) {
            // 空队列 = 本轮不传输 → 动画必须停。
            // [修复 2026-11-02] 原先这里直接 return，uploading 永远停在 true：
            // animTick 每 180ms 重发一次前台通知，状态栏图标一直卡在动画帧，
            // 空闲也在动（实测状态栏图标区在空闲时 250ms 间隔两帧像素不同）。
            // 连带问题：每 30 秒一轮的空队列冲刷让进程空转（实测 CPU 3%）。
            setUploading(false)
            android.util.Log.d("PhotoBeam", "冲刷队列: 无待发")
            return true
        }
        setUploading(true) // 有东西要发才播动画（传输中才动，空闲静止）
        android.util.Log.d("PhotoBeam", "冲刷队列: 待发 ${pend.size} 张 → ${pc.name}")

        // 一次发现（三通道）
        val hit = try {
            // [Stage 5 修正] 传入已配对 PC 的指纹：局域网里有多台 PhotoBeam 时只认自己那台
            Discovery.resolve(this, pc.lastIP, pc.lastPort, pc.fp)
                ?: throw java.io.IOException("未发现电脑（三通道均失败）")
        } catch (e: Exception) {
            linkOk = false
            android.util.Log.w("PhotoBeam", "发现失败: ${e.message}")
            return false
        }
        linkOk = true
        // found 应答携带 PC 实际监听端口 [P1-01/B-10]
        pairing.updateLastConnection(hit.ip, hit.port)

        // 一次连接 + 一次握手（复用整轮冲刷）
        var sock: Socket? = null
        var ch: SecureChannel? = null
        try {
            sock = Socket()
            sock.tcpNoDelay = true
            sock.connect(java.net.InetSocketAddress(hit.ip, hit.port), 1500)
            sock.soTimeout = 45_000
            val hs = pairing.handshake(sock, { _, _ -> SAS_CONFIRM }, allowPairing = false)
            if (hs.mode != "reconnect") {
                throw java.io.IOException("对端要求重新配对（本端在后台发送路径，已断开）")
            }
            ch = hs.channel
            val out = sock.getOutputStream()
            val inp = sock.getInputStream()

            for (it in pend) {
                try {
                    it.state = "sending"
                    queue.save()
                    // 发送时就绪判定 + 哈希 [方案 A/C]：读不到 → 分两种情况处理
                    //   · URI 确实失效（MediaStore 行已删除）→ 标 dropped，不再重试 [协议 §7.3]
                    //   · 文件还没写完/暂不可读 → 保持 pending，下轮重试（永不丢弃）
                    val data = Sender.readReady(this, it)
                    if (data == null) {
                        it.unreadyTries++
                        if (Sender.uriDead(this, it)) {
                            it.state = "dropped"
                            it.dropReason = "uri_dead"
                            queue.save()
                            android.util.Log.w("PhotoBeam", "URI 已失效，丢弃: ${it.name} (unready=${it.unreadyTries})")
                            Notifier.localDropped(applicationContext, it.name)
                            continue
                        }
                        it.state = "pending"
                        queue.save()
                        // 日志限流：前 3 次每次告警，之后每 20 次（≈10 分钟）告警一次，其余降为 debug。
                        // [修复 2026-11-02] 此前每 30 秒刷一条 warn，个别长期不可读的文件会把日志刷爆。
                        if (it.unreadyTries <= 3 || it.unreadyTries % 20 == 0) {
                            android.util.Log.w("PhotoBeam", "文件未就绪: ${it.name} unready=${it.unreadyTries}")
                        } else {
                            android.util.Log.d("PhotoBeam", "文件未就绪(限流): ${it.name} unready=${it.unreadyTries}")
                        }
                        continue
                    }
                    it.fileId = sha256(data).joinToString("") { "%02x".format(it) }
                    it.size = data.size.toLong()
                    // file_meta
                    ch.writeFrame(out, FRAME_CTRL, org.json.JSONObject()
                        .put("type", "file_meta")
                        .put("file_id", it.fileId)
                        .put("name", it.name)
                        .put("size", it.size)
                        .put("mtime", it.capturedAt)
                        .put("mime", it.mime)
                        .put("captured_at", it.capturedAt.toString())
                        .put("batch_id", it.batchId)
                        .put("is_latest_in_batch", it.isLatestInBatch)
                        .put("batch_size", it.batchSize)
                        // [Stage 7] 手机端相对目录（PC 端做路径消毒后建子目录）
                        .put("rel_path", it.relPath.trim('/'))
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
                        .put("type", "file_end").put("file_id", it.fileId)
                        .toString().toByteArray())
                    // ack
                    val (aft, apt) = ch.readFrame(inp)
                    require(aft == FRAME_CTRL) { "ack 帧类型异常" }
                    val ack = org.json.JSONObject(String(apt, Charsets.UTF_8))
                    require(ack.optString("type") == "ack") { "预期 ack" }
                    val status = ack.optString("status")
                    val errClass = ack.optString("err_class", "transient")
                    val reason = ack.optString("reason")
                    if (status != "ok") throw SendException(errClass, reason)
                    it.state = "sent"
                    queue.save()
                    flushedSomething = true
                    flushedCount++ // [A3] 回执：已发送 N 张
                    recordArrival(it.name) // [B3] 逐张「已到电脑」回执
                    android.util.Log.d("PhotoBeam", "落盘确认: ${it.name}")
                } catch (e: SendException) {
                    android.util.Log.w("PhotoBeam", "发送失败(${e.errClass}): ${it.name} reason=${e.reason}")
                    it.state = if (e.errClass == "permanent") "dropped" else "pending"
                    it.dropReason = e.reason
                    if (e.errClass == "transient") it.tries++ // [R-4] transient 也要计数
                    if (e.errClass == "permanent") Notifier.dropped(applicationContext, it.name, e.reason)
                    recordArrivalFailure(it.name, e.reason) // [B3] 失败也要有回执
                    queue.save()
                } catch (e: Exception) {
                    // 连接级/传输级异常 → 该图回 pending，断开重连下一轮再试 [E-02]
                    android.util.Log.w("PhotoBeam", "发送异常 ${it.name}: ${e.message}")
                    it.state = "pending"
                    it.tries++
                    queue.save()
                    ok = false
                    break
                }
            }
            // bye
            try { ch.writeFrame(out, FRAME_CTRL, "{\"type\":\"bye\"}".toByteArray()) } catch (e: Exception) { }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "连接级失败: ${e.message}")
            ok = false
        } finally {
            setUploading(false)
            try { sock?.close() } catch (e: Exception) { }
        }
        return ok
    }
}

class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action == Intent.ACTION_BOOT_COMPLETED) {
            // targetSdk 36 触发 BOOT_COMPLETED FGS 禁令 → 起服务可能失败，
            // 但**必须尝试**（实测 MIUI 在有"省电策略=无限制"时放行 SYSTEM_ALLOW_LISTED）。
            // 双保险：① 直接起服务 ② 同时挂精确闹钟兜底。
            // requestCode 2：与周期心跳链互不覆盖。
            android.util.Log.d("PhotoBeam", "BOOT_COMPLETED → 自起服务")
            SenderService.start(ctx)
            scheduleAlarmAt(ctx, System.currentTimeMillis() + 10_000, exact = true, requestCode = 2)
        }
    }
}

class AlarmReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        val pending = goAsync()
        try {
            // 闹钟已响：清掉"下次心跳"标记，再续上下一跳
            try {
                ctx.getSharedPreferences("photobeam", Context.MODE_PRIVATE)
                    .edit().putLong("next_alarm_at", 0L).apply()
            } catch (e: Exception) { }
            SenderService.start(ctx)
            scheduleHeartbeatChain(ctx)
        } finally {
            pending.finish()
        }
    }
}

/** 应用自身被覆盖安装后 FGS/闹钟全部被取消 → 立即自起服务 [Stage3-R] */
class UpgradeReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action == Intent.ACTION_MY_PACKAGE_REPLACED) {
            // [修复 2026-11-02 v2] 实测证明：08:20 那次崩溃前 AM 已放行
            //   "Background started FGS: Allowed [... code:PACKAGE_REPLACED ...]"
            //   —— 广播临时许可**足以**让 startForegroundService 通过，
            //   真正杀死进程的是我们没调 startForeground（见 postStatus 缺陷①）。
            //   → 现在广播里**直接起服务**，不用等闹钟（闹钟在 MIUI 上还可能被
            //   "Pending user blocked background alarms" 挂起）。
            //   同时保留精确闹钟兜底（requestCode 2，与心跳链互不覆盖）。
            android.util.Log.d("PhotoBeam", "MY_PACKAGE_REPLACED → 自起服务")
            SenderService.start(ctx)
            scheduleAlarmAt(ctx, System.currentTimeMillis() + 5_000, exact = true, requestCode = 2)
        }
    }
}

/**
 * 通知里的「停止」动作（PendingIntent 带 ACTION_STOP）[A1]
 *
 * 用 BroadcastReceiver + stopService 而不是 startForegroundService(ACTION_STOP)：
 *   ① 后台"起服务"在 Android 8+ 受限，而"停服务"不受限；
 *   ② startForegroundService 会给系统一个"5 秒内必须 startForeground"的欠条，
 *      而停止流程根本不会调 startForeground → 触发 ForegroundServiceDidNotStartInTime。
 */
class StopReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action != SenderService.ACTION_STOP) return
        android.util.Log.d("PhotoBeam", "收到通知「停止」动作 → 停止同步服务")
        try {
            ctx.stopService(Intent(ctx, SenderService::class.java))
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "stopService 失败: ${e.message}")
        }
        // 撤掉心跳闹钟链（否则 ≤5 分钟后又被闹钟拉起，"停止"名不副实）
        cancelHeartbeat(ctx)
        // 兜底撤通知（服务没在跑时不会有 onDestroy）
        try {
            (ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager)
                .apply {
                    cancel(SenderService.NOTI_ID)
                    cancel(SenderService.NOTI_ID_FALLBACK)
                }
        } catch (e: Exception) { }
        SenderService.iconMode = "none"
        try {
            ctx.getSharedPreferences("photobeam", Context.MODE_PRIVATE)
                .edit().putString("icon_mode", "none").apply()
        } catch (e: Exception) { }
    }
}

/** 精确闹钟是否可用：API 31/32 需 SCHEDULE_EXACT_ALARM（用户授权），API 33+ 用 USE_EXACT_ALARM */
fun canExactAlarm(ctx: Context): Boolean {
    if (android.os.Build.VERSION.SDK_INT < 31) return true
    val granted = { p: String ->
        ctx.checkSelfPermission(p) == android.content.pm.PackageManager.PERMISSION_GRANTED
    }
    return granted(android.Manifest.permission.SCHEDULE_EXACT_ALARM) ||
        (android.os.Build.VERSION.SDK_INT >= 33 && granted(android.Manifest.permission.USE_EXACT_ALARM))
}

/**
 * 取消心跳闹钟链 [A1]
 *
 * 「停止同步服务」必须同时撤掉心跳闹钟：否则 ≤5 分钟后 AlarmReceiver 又把服务拉起来，
 * "停止"只停了 5 分钟。
 * requestCode 1 = 周期心跳链（与"立刻起服务"的一次性闹钟 requestCode 2 互不覆盖）。
 */
fun cancelHeartbeat(ctx: Context) {
    try {
        val am = ctx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
        val pi = PendingIntent.getBroadcast(ctx, 1, Intent(ctx, AlarmReceiver::class.java),
            PendingIntent.FLAG_IMMUTABLE)
        am.cancel(pi)
    } catch (e: Exception) {
        android.util.Log.w("PhotoBeam", "取消心跳闹钟失败: ${e.message}")
    }
    try {
        ctx.getSharedPreferences("photobeam", Context.MODE_PRIVATE)
            .edit().putLong("next_alarm_at", 0L).apply()
    } catch (e: Exception) { }
}

/**
 * 排下一次心跳（精确闹钟）。
 *
 * [修复 2026-11-02] 关键坑：服务的 reconcile 每 30 秒就调一次心跳排程，
 * 若每次都"重排一个 5 分钟后的闹钟"，闹钟会被**无限推后、永不触发**
 * （实测：08:11→08:17 期间 alarm 一直被推到 5 分钟后，后台自起链彻底断掉）。
 * → 只有在"没有待触发闹钟"时才补排；闹钟响时由 AlarmReceiver 清标记并续下一跳。
 */
fun scheduleHeartbeatChain(ctx: Context) {
    val now = System.currentTimeMillis()
    val prefs = ctx.getSharedPreferences("photobeam", Context.MODE_PRIVATE)
    val next = try { prefs.getLong("next_alarm_at", 0L) } catch (e: Exception) { 0L }
    if (next > now + 15_000) return          // 已有待触发闹钟，别再推后
    val at = now + 5 * 60_000
    try { prefs.edit().putLong("next_alarm_at", at).apply() } catch (e: Exception) { }
    scheduleAlarmAt(ctx, at, exact = true)
}

/**
 * 调度闹钟。
 * @param exact true = 精确闹钟（后台拉起 FGS 的豁免条件，只用于开机/覆盖安装/心跳等场景）
 */
fun scheduleAlarm(ctx: Context, delayMs: Long, exact: Boolean = false) {
    scheduleAlarmAt(ctx, System.currentTimeMillis() + delayMs, exact)
}

fun scheduleAlarmAt(ctx: Context, atMs: Long, exact: Boolean = false, requestCode: Int = 1) {
    val am = ctx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
    // requestCode 1 = 周期心跳链；2 = "立刻起服务"的一次性闹钟（与心跳互不覆盖）
    val pi = PendingIntent.getBroadcast(ctx, requestCode, Intent(ctx, AlarmReceiver::class.java),
        PendingIntent.FLAG_IMMUTABLE)
    if (exact && canExactAlarm(ctx)) {
        try {
            am.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, atMs, pi)
            android.util.Log.d("PhotoBeam", "精确闹钟已挂: ${atMs - System.currentTimeMillis()}ms 后")
            return
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "精确闹钟被拒，退回惰性闹钟: ${e.message}")
        }
    }
    // 惰性闹钟：非 exact，无需 SCHEDULE_EXACT_ALARM [P0-13]
    am.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, atMs, pi)
    android.util.Log.d("PhotoBeam", "惰性闹钟已挂: ${atMs - System.currentTimeMillis()}ms 后")
}
