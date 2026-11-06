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

    /** 降级通道的超时：比 API 通道短——它只在 API 已经失败后才走，不该再让用户多等 */
    private const val TIMEOUT_REDIRECT_MS = 5000

    /** 降级通道最多跳几跳（真实 GitHub 只跳 1 跳：/releases/latest → /releases/tag/vX.Y.Z） */
    private const val MAX_REDIRECT_HOPS = 6

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
                    // GitHub 匿名 API 限流（403/429）最常见 → 降级到网页通道
                    return checkViaRedirect(current)
                        ?: res.copy(error = "GitHub API 返回 ${conn.responseCode}")
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
            // 网络异常（含 API 不可达）→ 同样降级一次；降级也失败才报错
            checkViaRedirect(current)
                ?: res.copy(error = e.message ?: e.javaClass.simpleName)
        }
    }

    /**
     * 降级方案：访问 /releases/latest 页面并跟随重定向，从**最终 URL** 里取 tag。
     *
     * 为什么需要它：GitHub 匿名 API 只有 60 次/小时/IP（同一 WiFi 下的用户共享），
     * 很容易被用光 → API 返回 403/429，检查更新直接对用户报失败。
     * 这个方案走**网页通道**，不走 API，不受该限额影响。
     *
     * 实测（2026-11-06）：跟随重定向后最终 URL = https://github.com/<repo>/releases/tag/v1.2.0
     *
     * 失败返回 null —— 由调用方保留原始错误，不静默吞掉。
     */
    private fun checkViaRedirect(current: String): UpdateResult? {
        var target = PAGE
        var hops = 0
        while (hops < MAX_REDIRECT_HOPS) {
            val conn = URL(target).openConnection() as HttpURLConnection
            try {
                conn.connectTimeout = TIMEOUT_REDIRECT_MS
                conn.readTimeout = TIMEOUT_REDIRECT_MS
                conn.requestMethod = "GET"
                // 自己跟随重定向：HttpURLConnection 默认不跟跨协议跳转，且我们必须拿到**最终 URL**
                conn.instanceFollowRedirects = false
                conn.setRequestProperty("User-Agent", "PhotoBeam/$current (update-check)")
                val code = conn.responseCode
                if (code in 300..399) {
                    val loc = conn.getHeaderField("Location")
                    if (loc.isNullOrBlank()) return null
                    // Location 可能是相对路径（GitHub 就是），用当前 URL 解析
                    target = URL(URL(target), loc).toString()
                    hops++
                } else {
                    if (code != 200) return null
                    val finalUrl = target
                    val marker = "/releases/tag/"
                    val i = finalUrl.lastIndexOf(marker)
                    if (i < 0) return null
                    val tag = finalUrl.substring(i + marker.length).trim('/').removePrefix("v")
                    if (tag.isBlank()) return null
                    return UpdateResult(
                        latest = tag,
                        current = current,
                        hasNew = compareVersions(tag, current) > 0,
                        pageUrl = finalUrl
                    )
                }
            } catch (e: Exception) {
                return null
            } finally {
                conn.disconnect()
            }
        }
        return null
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
            // [修复] 预发布后缀不能按字典序比：`beta10 < beta2`（字典序），
            // 会导致 "1.2.0-beta10" 被判成比 "1.2.0-beta2" 旧 → 误报「已是最新」。
            // 按 `-`/`.` 拆段：能解析为数字的段做数值比较，其余按字符串（同 parseVersion 风格）。
            return comparePrerelease(an.pre, bn.pre)
        }
        return 0
    }

    /** [修复] 预发布后缀逐段比较（数字段按数值，其余按字典序） */
    private fun comparePrerelease(a: String, b: String): Int {
        val as1 = a.split('-', '.').filter { it.isNotEmpty() }
        val bs1 = b.split('-', '.').filter { it.isNotEmpty() }
        val n = maxOf(as1.size, bs1.size)
        for (i in 0 until n) {
            val av = as1.getOrNull(i) ?: return -1 // 段数少者更旧（1.2.0-beta < 1.2.0-beta.1）
            val bv = bs1.getOrNull(i) ?: return 1
            val an = av.toIntOrNull()
            val bn = bv.toIntOrNull()
            if (an != null && bn != null) {
                if (an != bn) return if (an > bn) 1 else -1
            } else if (av != bv) {
                return if (av > bv) 1 else -1
            }
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
