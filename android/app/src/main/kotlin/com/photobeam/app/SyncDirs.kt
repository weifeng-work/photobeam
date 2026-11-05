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
// 持久化：SharedPreferences（StringSet），键 `sync_dirs`；另有 `sync_dirs_picked`
// 标记是否已完成过首次选择（用于首次启动引导）。

import android.content.Context
import android.provider.MediaStore

/** 同步目录筛选的持久化管理 */
object SyncDirs {

    private const val PREFS = "photobeam"
    private const val KEY_DIRS = "sync_dirs"
    private const val KEY_PICKED = "sync_dirs_picked"

    /** 用户已勾选的目录（RELATIVE_PATH 去掉尾斜杠的形式，如 "DCIM/Screenshots"） */
    fun selected(ctx: Context): Set<String> {
        val sp = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        return sp.getStringSet(KEY_DIRS, emptySet()) ?: emptySet()
    }

    /** 保存勾选结果 */
    fun save(ctx: Context, dirs: Set<String>) {
        val sp = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        sp.edit()
            .putStringSet(KEY_DIRS, dirs)
            .putBoolean(KEY_PICKED, true)
            .apply()
    }

    /** 用户是否已经做过选择（用于首次启动引导：没选过才弹） */
    fun hasPicked(ctx: Context): Boolean =
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getBoolean(KEY_PICKED, false)

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
     */
    fun buildSelectionSql(ctx: Context): String? {
        val sel = selected(ctx)
        if (sel.isEmpty()) return null   // 空选 → 查不到任何东西（下面特殊处理）
        val ors = sel.joinToString(" OR ") { s ->
            // 转义 LIKE 的通配符（目录名里可能有 % 或 _）
            val esc = s.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
            "(relative_path LIKE '$esc/%' ESCAPE '\\' OR relative_path = '$esc/')"
        }
        return ors
    }
}