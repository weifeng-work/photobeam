package main

// [Stage 11] PC 端「检查更新」——只查版本、只给下载链接，不自动安装。
//
// 设计原则（与协议 §10 的取舍一致）：
//   · **不做自动下载/自动替换**：运行中的 exe 被占用，且静默替换是安全反模式。
//     用户点了「检查更新」→ 告知有新版本 → 给下载链接，由用户自己决定。
//   · **不引入新依赖**：用标准库 net/http（项目原本零第三方 HTTP 依赖）。
//   · **离线可用**：查不到更新**不是错误**，只是"没新版本/查不到"，不能打扰用户。
//
// 版本号来源：构建时由 -ldflags 注入（build-pc.sh 读仓库根的 VERSION 文件），
// 与 GitHub Release 的 tag（v<VERSION>）保持一致。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// appVersion：当前版本号（构建时注入，见 build-pc.sh 的 -X main.appVersion）。
// 未注入时为空字符串（本地 `go build` 调试版）→ 检查更新会提示"未标注版本"。
var appVersion = ""

// updateRepo：GitHub 仓库（owner/repo）。公开仓库的 releases/latest 是公开 API。
const updateRepo = "weifeng-work/photobeam"

// updateAPI：GitHub 最新 Release 的 API。匿名访问有速率限制（60 次/小时/IP），
// 但这里是**用户手动点击**才触发，速率完全够用。
//
// 用 var 而非 const：测试里要指向 httptest 服务器（见 update_test.go）。
var updateAPI = "https://api.github.com/repos/" + updateRepo + "/releases/latest"

// releaseURL：人看的 Release 页面（比 API 更适合给用户点开）
var releaseURL = "https://github.com/" + updateRepo + "/releases/latest"

// updateCheckTimeout：整体超时。查更新是**附加功能**，绝不能拖住 UI 或托盘线程。
const updateCheckTimeout = 8 * time.Second

// updateInfo：检查结果
type updateInfo struct {
	Latest   string // 最新版本号（不含 v 前缀）
	Current  string // 当前版本号
	HasNew   bool   // 是否有新版
	PageURL  string // Release 页面
	Notes    string // 发布说明（首行，够用即可）
	Err      error  // 非 nil = 检查失败（网络/限流等）
}

// checkForUpdate：查 GitHub 最新 Release，比对版本号。
//
// 网络失败**不视为错误**给用户看（离线环境很常见）——只在 Err 里带回，
// 由调用方决定怎么提示。
func checkForUpdate(current string) updateInfo {
	info := updateInfo{Current: current, PageURL: releaseURL}

	if strings.TrimSpace(current) == "" {
		info.Err = fmt.Errorf("当前版本未标注（本地调试构建）")
		return info
	}

	client := &http.Client{Timeout: updateCheckTimeout}
	req, err := http.NewRequest("GET", updateAPI, nil)
	if err != nil {
		info.Err = err
		return info
	}
	// GitHub API 要求 User-Agent，且建议 Accept: application/vnd.github+json
	req.Header.Set("User-Agent", "PhotoBeam/"+current+" (update-check)")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		// [Stage 12] API 不可用（离线 / 触发 GitHub 匿名 API 速率限制 60 次/小时/IP）
		// → 降级到「跟随重定向取最终 URL 的 tag」：不走 API，**无限流**。
		if alt, altErr := checkViaRedirect(client, current); altErr == nil {
			info.Latest = alt
			info.HasNew = compareVersions(alt, current) > 0
			return info
		}
		info.Err = err
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 速率限制（403/429）最常见 → 同样降级
		if alt, altErr := checkViaRedirect(client, current); altErr == nil {
			info.Latest = alt
			info.HasNew = compareVersions(alt, current) > 0
			return info
		}
		info.Err = fmt.Errorf("GitHub API 返回 %d", resp.StatusCode)
		return info
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		info.Err = err
		return info
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		info.Err = err
		return info
	}
	info.Latest = strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	if rel.HTMLURL != "" {
		info.PageURL = rel.HTMLURL
	}
	info.Notes = firstLine(rel.Body)
	info.HasNew = compareVersions(info.Latest, current) > 0
	return info
}

// checkViaRedirect：降级方案 —— 访问 /releases/latest 并跟随重定向，
// 从**最终 URL** 里取 tag（形如 .../releases/tag/v1.2.0）。
//
// 为什么需要它：GitHub 匿名 API 限额只有 60 次/小时/IP（同一 WiFi 下的用户共享），
// 很容易被用光 → 检查更新直接失败。这个方案**不走 API**，页面不限流。
// 实测（2026-11-06）：跟随重定向后最终 URL = https://github.com/<repo>/releases/tag/v1.2.0
func checkViaRedirect(client *http.Client, current string) (string, error) {
	req, err := http.NewRequest("GET", releaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "PhotoBeam/"+current+" (update-check)")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	final := resp.Request.URL.String()
	i := strings.LastIndex(final, "/releases/tag/")
	if i < 0 {
		return "", fmt.Errorf("无法从 %s 解析版本号", final)
	}
	tag := strings.Trim(strings.TrimSpace(final[i+len("/releases/tag/"):]), "/")
	tag = strings.TrimPrefix(tag, "v")
	if tag == "" {
		return "", fmt.Errorf("版本号为空")
	}
	return tag, nil
}

// firstLine：取发布说明首行（去掉 markdown 标题符号），够用即可。
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		l = strings.TrimLeft(l, "#")
		l = strings.TrimSpace(l)
		if l != "" {
			return l
		}
	}
	return ""
}

// compareVersions：比较两个点分版本号（"1.2.3"）。
// 返回 >0 表示 a 新于 b，<0 表示 a 旧于 b，=0 表示相同。
//
// 规则：
//   · 逐段按数字比较（"1.10.0" > "1.9.0"，不是字符串比较）；
//   · 段数不同 → 缺失段按 0 处理（"1.2" == "1.2.0"）；
//   · 带预发布后缀（"1.2.0-beta"）→ **比同数字的正式版旧**（语义化版本惯例）。
func compareVersions(a, b string) int {
	an, aPre := parseVersion(a)
	bn, bPre := parseVersion(b)
	n := len(an)
	if len(bn) > n {
		n = len(bn)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(an) {
			av = an[i]
		}
		if i < len(bn) {
			bv = bn[i]
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	// 数字段全同 → 比较预发布标记：有预发布后缀的更旧
	if (aPre != "") != (bPre != "") {
		if aPre != "" {
			return -1
		}
		return 1
	}
	// 都有预发布后缀 → 逐段比较（不能直接比字符串）
	//
	// [Stage 17 · 修复 M-5] 原实现是 `if aPre > bPre` —— **字典序**比较，
	// 于是 "beta10" < "beta2"（'1' < '2'）→ 1.2.0-beta10 被判成比 1.2.0-beta2 旧。
	// Android 端有同一个 bug（Update.kt 的 comparePrerelease），两边一起修，
	// 否则两端的「检查更新」会给出不一致的结论。
	if aPre != "" && bPre != "" {
		return comparePrerelease(aPre, bPre)
	}
	return 0
}

// comparePrerelease：逐段比较预发布后缀。
//
// "beta.2" vs "beta.10"  → 段 ["beta","2"] vs ["beta","10"] → 2 < 10 → 更旧
// "beta2"  vs "beta10"   → 拆成 ("beta",2) vs ("beta",10) → 2 < 10 → 更旧
// "alpha"  vs "beta"     → "alpha" < "beta"
//
// 规则：每段先试纯数字（数值比），否则拆「字母前缀 + 数字后缀」（前缀字典序、后缀数值）。
func comparePrerelease(a, b string) int {
	as := splitPreSegments(a)
	bs := splitPreSegments(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		if i >= len(as) {
			return -1 // 段数少者更旧（1.2.0-beta < 1.2.0-beta.1）
		}
		if i >= len(bs) {
			return 1
		}
		if c := comparePreSegment(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return 0
}

// splitPreSegments：按 '-' 和 '.' 拆段（与 Android 端 comparePrerelease 一致）
func splitPreSegments(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '-' || r == '.' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// comparePreSegment：比较单个预发布段
func comparePreSegment(a, b string) int {
	an, aErr := strconv.ParseInt(a, 10, 64)
	bn, bErr := strconv.ParseInt(b, 10, 64)
	if aErr == nil && bErr == nil {
		if an == bn {
			return 0
		}
		if an > bn {
			return 1
		}
		return -1
	}
	aPrefix, aNum := splitAlphaNum(a)
	bPrefix, bNum := splitAlphaNum(b)
	if aPrefix != bPrefix {
		if aPrefix > bPrefix {
			return 1
		}
		return -1
	}
	if aNum == bNum {
		return 0
	}
	if aNum > bNum {
		return 1
	}
	return -1
}

// splitAlphaNum：把 "beta10" 拆成 ("beta", 10)；没有数字后缀时数字为 0
func splitAlphaNum(s string) (string, int64) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	prefix := s[:i]
	var num int64
	if i < len(s) {
		if v, err := strconv.ParseInt(s[i:], 10, 64); err == nil {
			num = v
		}
	}
	return prefix, num
}

// parseVersion：解析版本号 → (数字段列表, 预发布后缀)。
//
// "1.2.0-beta.1" → ([1,2,0], "beta.1")
// "v1.2"        → ([1,2], "")
func parseVersion(v string) ([]int, string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return nil, ""
	}
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			// 非数字段 → 当作 0（容忍异常输入，不让比较崩掉）
			n = 0
		}
		nums = append(nums, n)
	}
	return nums, pre
}

// splitVersion：拆版本号段（保留原样，测试与调试用）
func splitVersion(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return nil
	}
	return strings.Split(v, ".")
}
