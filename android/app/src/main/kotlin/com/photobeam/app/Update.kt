package com.photobeam.app

// [Stage 11] Android 端「检查更新」——只查版本、只给下载链接，不自动安装。
//
// 设计原则（与 PC 端一致）：
//   · **不自动下载/不自动安装**：装 APK 需要 REQUEST_INSTALL_PACKAGES 权限
//     （系统会显示吓人的权限说明），且国产 ROM 审核更严 → 本版不做，只给下载链接。
//   · **不引入新依赖**：用系统自带的 HttpURLConnection（项目原本零第三方 HTTP 依赖）。
//   · **离线可用**：查不到更新不是错误，只是"查不到"，如实告知即可。
//
// 版本号来源：构建时由 build-android.sh 注入到 AndroidManifest 的 versionName
// （与仓库根的 VERSION 文件、GitHub Release 的 tag v<VERSION> 三者一致）。

import android.content.Context
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

/** 检查更新的结果 */
data class UpdateResult(
    val latest: String = "",      // 最新版本号（不含 v）
    val current: String = "",     // 当前版本号
    val hasNew: Boolean = false,  // 是否有新版
    val pageUrl: String = "",     // Release 页面
    val notes: String = "",       // 发布说明首行
    val error: String = ""        // 非空 = 检查失败
)

object UpdateChecker {

    private const val REPO = "weifeng-work/photobeam"
    private const val API = "https://api.github.com/repos/$REPO/releases/latest"
    private const val PAGE = "https://github.com/$REPO/releases/latest"
    private const val TIMEOUT_MS = 8000

    /** 当前 App 版本号（读 manifest 里的 versionName，由构建脚本注入） */
    fun currentVersion(ctx: Context): String {
        return try {
            val pi = ctx.packageManager.getPackageInfo(ctx.packageName, 0)
            pi.versionName ?: ""
        } catch (e: Exception) {
            ""
        }
    }

    /**
     * 查 GitHub 最新 Release 并比对版本。
     *
     * **必须在后台线程调用**（网络请求）。
     */
    fun check(ctx: Context, current: String = currentVersion(ctx)): UpdateResult {
        val res = UpdateResult(current = current, pageUrl = PAGE)
        if (current.isBlank()) {
            return res.copy(error = "当前版本未标注（本地调试构建）")
        }
        return try {
            val conn = URL(API).openConnection() as HttpURLConnection
            conn.connectTimeout = TIMEOUT_MS
            conn.readTimeout = TIMEOUT_MS
            conn.requestMethod = "GET"
            // GitHub API 要求 User-Agent
            conn.setRequestProperty("User-Agent", "PhotoBeam/$current (update-check)")
            conn.setRequestProperty("Accept", "application/vnd.github+json")
            try {
                if (conn.responseCode != 200) {
                    return res.copy(error = "GitHub API 返回 ${conn.responseCode}")
                }
                val text = conn.inputStream.bufferedReader().use { it.readText() }
                val o = JSONObject(text)
                val latest = o.optString("tag_name", "").trim().removePrefix("v")
                val notes = firstLine(o.optString("body", ""))
                val url = o.optString("html_url", "").ifBlank { PAGE }
                res.copy(
                    latest = latest,
                    pageUrl = url,
                    notes = notes,
                    hasNew = latest.isNotBlank() && compareVersions(latest, current) > 0
                )
            } finally {
                conn.disconnect()
            }
        } catch (e: Exception) {
            res.copy(error = e.message ?: e.javaClass.simpleName)
        }
    }

    /** 取发布说明首行（去掉 markdown 标题符号） */
    private fun firstLine(s: String): String {
        for (raw in s.split("\n")) {
            val l = raw.trim().removePrefix("#").trim()
            if (l.isNotEmpty()) return l
        }
        return ""
    }

    /**
     * 比较两个点分版本号。返回 >0 表示 a 新于 b。
     *
     * 规则（与 PC 端 pc/update.go 的 compareVersions 一致）：
     *   · 逐段按数字比较（"1.10.0" > "1.9.0"，不是字符串比较）；
     *   · 段数不同 → 缺失段按 0（"1.2" == "1.2.0"）；
     *   · 带预发布后缀（"1.2.0-beta"）→ 比同数字的正式版旧。
     */
    fun compareVersions(a: String, b: String): Int {
        val an = parseVersion(a)
        val bn = parseVersion(b)
        val n = maxOf(an.nums.size, bn.nums.size)
        for (i in 0 until n) {
            val av = if (i < an.nums.size) an.nums[i] else 0
            val bv = if (i < bn.nums.size) bn.nums[i] else 0
            if (av != bv) return if (av > bv) 1 else -1
        }
        val aPre = an.pre.isNotEmpty()
        val bPre = bn.pre.isNotEmpty()
        if (aPre != bPre) return if (aPre) -1 else 1
        if (aPre && bPre && an.pre != bn.pre) {
            return if (an.pre > bn.pre) 1 else -1
        }
        return 0
    }

    private data class Parsed(val nums: IntArray, val pre: String)

    private fun parseVersion(v: String): Parsed {
        var s = v.trim().removePrefix("v")
        if (s.isEmpty()) return Parsed(IntArray(0), "")
        var pre = ""
        val idx = s.indexOfFirst { it == '-' || it == '+' }
        if (idx >= 0) {
            pre = s.substring(idx + 1)
            s = s.substring(0, idx)
        }
        val nums = s.split(".").map { p -> p.toIntOrNull() ?: 0 }.toIntArray()
        return Parsed(nums, pre)
    }
}
