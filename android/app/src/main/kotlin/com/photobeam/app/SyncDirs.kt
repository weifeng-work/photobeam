package com.photobeam.app

// [Stage 8] 同步目录筛选 —— 用户选择哪些手机相册目录参与同步
//
// 设计要点（与协议 §7.2 增量模型的关系）：
//   · **默认空选 = 不同步任何东西**。这是产品要求：用户没主动选目录之前，
//     一张图都不该离开手机（隐私默认拒绝，而不是默认全量同步）。
//   · 目录筛选**只影响"采集范围"**，不改变增量语义：仍然是「新增图片被同步」，
//     不是文件夹全量镜像。取消勾选某目录后，该目录的历史图不会被补发。
//   · 勾选是**前缀匹配**：勾了 `DCIM/Camera` 则 `DCIM/Camera/SubDir/` 也算在内
//     （选一个文件夹理应包含它的子目录）。
//   · 不需要 SAF 目录授权：MediaStore 的 `RELATIVE_PATH` 已经能列出所有图片目录，
//     现有「允许访问所有照片」权限就够读目录名，不需要额外的存储授权。
//
// [Stage 14 · 缺陷修复] 「历史图片不补传」在新装手机上被绕过了：
//   新装时全局 _id 水位是 0，用户选完目录后第一次对账的 SQL 是
//   `_id > 0 AND (目录过滤)` → 选中目录里**所有历史照片**都被当成"新增"入队。
//   实测（用户 2026-11-06 反馈）：新机选了 4 个文件夹 → 100+ 张历史图被推给电脑。
//   这与 UI 弹窗里明写的「只有之后新增的图片会被同步，历史图片不会补传」矛盾。
//
//   修法（**不动增量核心的水位模型**）：给**每个目录**记一个「同步起点」
//   （选它那一刻的全局最大 _id），查询时每个目录分支各带各的 `_id > 起点`。
//   · 新选目录 → 起点 = 当前全局 max → 该目录的历史图不补传 ✓
//   · 补传模式 → 起点 = 0 → 该目录的历史图照传 ✓
//   · 已选目录的起点保持不变 → 它的增量不受影响，也不会丢图 ✓
//
// 持久化：SharedPreferences（StringSet），键 `sync_dirs`；另有 `sync_dirs_picked`
// 标记是否已完成过首次选择（用于首次启动引导）；`sync_dir_starts` 存每目录起点
// （格式 "目录=起点"）；`sync_backfill` 存用户是否勾了「同时补传已有照片」。

import android.content.Context
import android.provider.MediaStore

/** 同步目录筛选的持久化管理 */
object SyncDirs {

    private const val PREFS = "photobeam"
    private const val KEY_DIRS = "sync_dirs"
    private const val KEY_PICKED = "sync_dirs_picked"
    // [Stage 12] 首次引导是否已被用户明确关闭过（"暂不选择"）。
    // 与 KEY_PICKED 分开：KEY_PICKED = 用户主动保存过一次选择（哪怕是空集）；
    // KEY_GUIDE_DISMISSED = 用户明确说"暂不选择"、不想再被弹窗打扰。
    private const val KEY_GUIDE_DISMISSED = "sync_guide_dismissed"
    // [Stage 14] 每目录的同步起点：格式 "目录=起点_id"（MediaStore _id）。
    // 0 = 不限制（补传模式）。
    private const val KEY_DIR_STARTS = "sync_dir_starts"

    /** 用户已勾选的目录（RELATIVE_PATH 去掉尾斜杠的形式，如 "DCIM/Screenshots"）
     *
     *  [Stage 12 · 缺陷修复] **必须返回副本**：SharedPreferences.getStringSet 返回的是
     *  prefs 内部持有的那个 Set 实例，直接修改它会抛 ConcurrentModificationException，
     *  而且在某些 ROM 上会静默写坏存储。这里返回 HashSet 副本，调用方随便改都安全。 */
    fun selected(ctx: Context): Set<String> {
        val sp = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val raw = sp.getStringSet(KEY_DIRS, null) ?: return emptySet()
        return HashSet(raw)
    }

    /** 保存勾选结果
     *
     *  [Stage 12 · 缺陷修复] 同样存**副本**：putStringSet 会持有传入集合的引用，
     *  调用方之后改它就等于改了 prefs 内部状态（Android 官方文档明确要求传副本）。
     *
     *  [Stage 14] 额外存每目录的同步起点（starts）。
     *  starts 由 [computeStartIds] 预先算好（要查 MediaStore，耗时，必须在后台线程做）。
     *
     *  [Stage 14] 「补传」是**一次性动作**，不持久化 —— 下次打开弹窗默认不勾，
     *  免得用户只是改个勾选就莫名其妙触发一次大批量补传。 */
    fun save(ctx: Context, dirs: Set<String>, starts: Map<String, Long>) {
        val sp = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        sp.edit()
            .putStringSet(KEY_DIRS, HashSet(dirs))
            .putBoolean(KEY_PICKED, true)
            .apply()
        saveStartIds(ctx, starts)
    }

    // ---------- [Stage 14] 每目录同步起点 ----------

    /** 读每目录起点表（目录 → 起点 _id） */
    fun startIds(ctx: Context): Map<String, Long> {
        val sp = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val raw = sp.getStringSet(KEY_DIR_STARTS, null) ?: return emptyMap()
        val out = HashMap<String, Long>()
        for (s in raw) {
            val i = s.lastIndexOf('=')
            if (i <= 0) continue
            val id = s.substring(i + 1).toLongOrNull() ?: continue
            out[s.substring(0, i)] = id
        }
        return out
    }

    /** 写每目录起点表（存副本，同 [save] 的理由） */
    private fun saveStartIds(ctx: Context, starts: Map<String, Long>) {
        val sp = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val encoded = HashSet<String>(starts.size)
        for ((k, v) in starts) encoded.add("$k=$v")
        sp.edit().putStringSet(KEY_DIR_STARTS, encoded).apply()
    }

    /**
     * 算出保存后的每目录起点表。**耗时**（要查 MediaStore），必须后台线程调用。
     *
     * 规则：
     *   · 补传模式 → 所有选中目录起点 = 0（不限制 → 历史图照传）。
     *   · 不补传   → **新选**的目录起点 = 当前全局最大 _id（历史图不补传）；
     *               **已选**的目录保留原起点（不动 → 它的增量不受影响、不会丢图）。
     */
    fun computeStartIds(ctx: Context, dirs: Set<String>, backfill: Boolean): Map<String, Long> {
        val old = startIds(ctx)
        val out = HashMap<String, Long>(old)
        if (backfill) {
            for (d in dirs) out[d] = 0L
            return out
        }
        val maxId = currentMaxImageId(ctx)
        for (d in dirs) if (!old.containsKey(d)) out[d] = maxId
        return out
    }

    /**
     * [Stage 14] 老用户升级兼容：**已选目录但没有起点记录**时，
     * 补一个「当前全局 max」作为起点 —— 等于保持原行为（不补传历史）。
     *
     * 没有这一步的话，v1.2.1 的老用户升级后起点表是空的 → 起点默认 0 →
     * 第一次对账会把已选目录里的历史图**再补传一次**（第二次意外全量）。
     *
     * 幂等：补齐后就不再查 MediaStore。调用时机必须在**第一次对账之前**
     * （放在 [MediaStoreSync.reconcile] 开头最保险）。
     */
    fun ensureStartIds(ctx: Context) {
        val sel = selected(ctx)
        if (sel.isEmpty()) return
        val old = startIds(ctx)
        val missing = sel.filter { !old.containsKey(it) }
        if (missing.isEmpty()) return
        val maxId = currentMaxImageId(ctx)
        val merged = HashMap<String, Long>(old)
        for (d in missing) merged[d] = maxId
        saveStartIds(ctx, merged)
        android.util.Log.d("PhotoBeam", "补齐目录同步起点：${missing.size} 个目录，起点=$maxId")
    }

    /**
     * 当前 MediaStore 里的**最大**图片 _id（=「此刻之前的所有图片」的上界）。
     *
     * 遍历全表取最大值，不用 `MAX(_id)` 聚合（MediaProvider 对聚合函数的投影支持不稳）
     * 也不用只信 sortOrder（部分 ROM 会忽略 `_id DESC`）。几千行遍历约几十毫秒，
     * 只在用户点「保存」时和升级兼容时各跑一次。
     */
    private fun currentMaxImageId(ctx: Context): Long {
        var max = 0L
        try {
            ctx.contentResolver.query(
                MediaStore.Images.Media.EXTERNAL_CONTENT_URI,
                arrayOf(MediaStore.Images.Media._ID), null, null, null
            )?.use { c ->
                val idx = c.getColumnIndex(MediaStore.Images.Media._ID)
                if (idx >= 0) {
                    while (c.moveToNext()) {
                        val id = c.getLong(idx)
                        if (id > max) max = id
                    }
                }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "查 MediaStore 最大 _id 失败: ${e.message}")
        }
        return max
    }

    /** 用户是否已经做过选择（用于首次启动引导：没选过才弹） */
    fun hasPicked(ctx: Context): Boolean =
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getBoolean(KEY_PICKED, false)

    /**
     * [Stage 12] 用户是否明确关闭过首次引导（点过「暂不选择」）。
     * 只有明确关闭过才不再自动弹窗；单纯"还没保存过选择"时继续引导。
     */
    fun isGuideDismissed(ctx: Context): Boolean =
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getBoolean(KEY_GUIDE_DISMISSED, false)

    /** 标记首次引导已被明确关闭 */
    fun dismissGuide(ctx: Context) {
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
            .putBoolean(KEY_GUIDE_DISMISSED, true).apply()
    }

    /**
     * 列出手机上**有图片的目录**，按图片数降序。
     * 返回：relative_path（去掉尾斜杠）→ 图片数量。
     *
     * 只列出至少有 1 张图片的目录 —— 空的目录对用户没有意义（勾了也不会有东西同步）。
     */
    fun listDirs(ctx: Context): List<Pair<String, Int>> {
        val proj = arrayOf(MediaStore.Images.Media.RELATIVE_PATH)
        val counts = HashMap<String, Int>()
        try {
            ctx.contentResolver.query(
                MediaStore.Images.Media.EXTERNAL_CONTENT_URI, proj, null, null, null
            )?.use { c ->
                val idx = c.getColumnIndex(MediaStore.Images.Media.RELATIVE_PATH)
                if (idx >= 0) {
                    while (c.moveToNext()) {
                        val rp = c.getString(idx) ?: continue
                        val norm = rp.trim('/')
                        if (norm.isEmpty()) continue
                        counts[norm] = (counts[norm] ?: 0) + 1
                    }
                }
            }
        } catch (e: Exception) {
            android.util.Log.w("PhotoBeam", "列目录失败: ${e.message}")
        }
        // 按图片数降序（数量相同按字典序，保证顺序稳定）
        val entries: MutableList<Map.Entry<String, Int>> = ArrayList(counts.entries)
        entries.sortWith(Comparator { a: Map.Entry<String, Int>, b: Map.Entry<String, Int> ->
            val byCount = b.value.compareTo(a.value)
            if (byCount != 0) byCount else a.key.compareTo(b.key)
        })
        val out = ArrayList<Pair<String, Int>>(entries.size)
        for (e in entries) out.add(Pair(e.key, e.value))
        return out
    }

    /**
     * 判断某个 relative_path 是否落在已选目录内（前缀匹配）。
     * 空选 → 一律 false（什么都不同步）。
     */
    fun isSelected(ctx: Context, relPath: String): Boolean {
        val sel = selected(ctx)
        if (sel.isEmpty()) return false
        val norm = relPath.trim('/')
        if (norm.isEmpty()) return false
        for (s in sel) {
            if (norm == s) return true
            // 前缀匹配：选中 DCIM/Camera → DCIM/Camera/Sub 也算
            if (norm.startsWith("$s/")) return true
        }
        return false
    }

    /**
     * 生成 SQL 的目录过滤条件（用于 MediaStore 查询）。
     * 返回 null = 无需过滤（未启用筛选时）；返回空集合 = 不允许任何目录。
     *
     * [Stage 14] 每个目录分支各带各的 `_id > 起点`：
     *   起点 = 该目录被选中那一刻的全局最大 _id → 该目录的历史图不补传；
     *   起点 = 0（补传模式）→ 该目录的历史图照传。
     */
    fun buildSelectionSql(ctx: Context): String? {
        val sel = selected(ctx)
        if (sel.isEmpty()) return null   // 空选 → 查不到任何东西（下面特殊处理）
        val starts = startIds(ctx)
        val ors = sel.joinToString(" OR ") { s ->
            // 转义 LIKE 的通配符（目录名里可能有 % 或 _）
            val esc = s.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
            val start = starts[s] ?: 0L
            "((relative_path LIKE '$esc/%' ESCAPE '\\' OR relative_path = '$esc/') AND _id > $start)"
        }
        return ors
    }
}