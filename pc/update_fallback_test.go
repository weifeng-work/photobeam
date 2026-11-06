package main

// [Stage 11] 「检查更新」限流降级路径的单元测试
//
// 背景：GitHub 匿名 API 只有 60 次/小时/IP（同一 WiFi 下的用户共享），
// 一旦被用光，api.github.com/repos/.../releases/latest 会返回 403/429，
// 检查更新就会对用户报"查询失败"。降级方案是访问 /releases/latest 页面并
// 跟随重定向，从**最终 URL** 里取 tag —— 走网页通道，不受 API 限额影响。
//
// 被测不变式：
//   · API 报 403/429 → 自动降级，最终**不报错**（Err == nil）
//   · API 直接连不上（离线/限流）→ 自动降级
//   · 两条路都失败 → 保留原始错误，不吞错
//
// 运行：cd pc && go test -run TestCheckForUpdate -v

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// withEndpoints：临时把 updateAPI / releaseURL 指到测试服务器，结束后还原。
func withEndpoints(t *testing.T, api, page string) {
	t.Helper()
	oldAPI, oldPage := updateAPI, releaseURL
	updateAPI, releaseURL = api, page
	t.Cleanup(func() { updateAPI, releaseURL = oldAPI, oldPage })
}

// newReleasePage：模拟 GitHub 的 /releases/latest —— 302 重定向到具体 tag。
// 只在 /releases/latest 上跳转（对齐真实 GitHub 行为，否则会无限重定向）。
func newReleasePage(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, "/owner/repo/releases/tag/"+tag, http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html>release page</html>"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCheckForUpdateFallbackOnRateLimit：API 返回 403（限流）→ 降级成功
func TestCheckForUpdateFallbackOnRateLimit(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer api.Close()

	page := newReleasePage(t, "v1.3.0")
	withEndpoints(t, api.URL, page.URL+"/releases/latest")

	info := checkForUpdate("1.2.0")
	if info.Err != nil {
		t.Fatalf("降级路径不应报错，却拿到: %v", info.Err)
	}
	if info.Latest != "1.3.0" {
		t.Errorf("降级取到的最新版本 = %q，期望 1.3.0", info.Latest)
	}
	if !info.HasNew {
		t.Error("1.3.0 应判定为有新版")
	}
}

// TestCheckForUpdateFallbackOnAPIUnreachable：API 完全连不上 → 降级成功
func TestCheckForUpdateFallbackOnAPIUnreachable(t *testing.T) {
	// 起一个服务器后立刻关掉 → 端口无人监听 → 连接被拒
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	page := newReleasePage(t, "v1.2.1")
	withEndpoints(t, deadURL, page.URL+"/releases/latest")

	info := checkForUpdate("1.2.0")
	if info.Err != nil {
		t.Fatalf("降级路径不应报错，却拿到: %v", info.Err)
	}
	if info.Latest != "1.2.1" {
		t.Errorf("降级取到的最新版本 = %q，期望 1.2.1", info.Latest)
	}
	if !info.HasNew {
		t.Error("1.2.1 应判定为有新版")
	}
}

// TestCheckForUpdateFallbackSaysUpToDate：降级路径拿到的版本与当前一致 → 无新版
func TestCheckForUpdateFallbackSaysUpToDate(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer api.Close()

	page := newReleasePage(t, "v1.2.0")
	withEndpoints(t, api.URL, page.URL+"/releases/latest")

	info := checkForUpdate("1.2.0")
	if info.Err != nil {
		t.Fatalf("降级路径不应报错，却拿到: %v", info.Err)
	}
	if info.HasNew {
		t.Error("版本相同不应判定为有新版")
	}
}

// TestCheckForUpdateBothFail：两条路都失败 → 保留原始错误，不静默吞掉
func TestCheckForUpdateBothFail(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer api.Close()

	// 页面也失败：直接返回 404，重定向后 URL 里没有 /releases/tag/ → 解析不出版本
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer page.Close()

	withEndpoints(t, api.URL, page.URL+"/releases/latest")

	info := checkForUpdate("1.2.0")
	if info.Err == nil {
		t.Fatal("两条路都失败时应报错")
	}
	if info.HasNew {
		t.Error("失败时不应判定为有新版")
	}
}

// TestCheckViaRedirectParsesTag：直接测降级解析：从最终 URL 里抠出 tag
func TestCheckViaRedirectParsesTag(t *testing.T) {
	cases := []struct {
		tag  string
		want string
	}{
		{"v1.3.0", "1.3.0"},
		{"v10.0.0", "10.0.0"},
		{"1.4.2", "1.4.2"}, // 没有 v 前缀也要认
	}
	for _, c := range cases {
		page := newReleasePage(t, c.tag)
		withEndpoints(t, page.URL+"/releases/latest", page.URL+"/releases/latest")
		client := &http.Client{Timeout: updateCheckTimeout}
		got, err := checkViaRedirect(client, "1.0.0")
		if err != nil {
			t.Errorf("checkViaRedirect(tag=%s) 报错: %v", c.tag, err)
			continue
		}
		if got != c.want {
			t.Errorf("checkViaRedirect(tag=%s) = %q，期望 %q", c.tag, got, c.want)
		}
	}
}

// TestCheckForUpdateRequiresVersion：版本号缺失时不该发网络请求
func TestCheckForUpdateRequiresVersion(t *testing.T) {
	called := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"tag_name":"v1.0.0"}`))
	}))
	defer api.Close()
	withEndpoints(t, api.URL, api.URL)

	info := checkForUpdate("   ")
	if info.Err == nil {
		t.Fatal("版本号为空时应报错")
	}
	if called {
		t.Error("版本号为空时不应发起网络请求")
	}
}

// TestLiveUpdateCheckAgainstRealGitHub：对**真实** GitHub 的端到端验证（默认跳过）。
//
// 为什么要单独一条：上面的用例都跑在 mock 上，能证明「降级逻辑写对了」，
// 但证明不了「真实 GitHub 当前的响应形态能被降级路径接住」。
// 2026-11-06 实测 GitHub 匿名 API 处于限流状态（core.remaining=0，
// releases/latest 返回 403），正是这条路径的真实触发条件。
//
// 运行：cd pc && PB_LIVE_NET=1 go test -run TestLiveUpdate -v
// CI 不跑（网络波动会让 CI 变红），本地手动开。
func TestLiveUpdateCheckAgainstRealGitHub(t *testing.T) {
	if os.Getenv("PB_LIVE_NET") != "1" {
		t.Skip("跳过真实网络测试（设 PB_LIVE_NET=1 开启）")
	}
	// 注意：这里**不覆盖** updateAPI / releaseURL —— 用生产常量，才是真验证。
	info := checkForUpdate("1.2.1")
	t.Logf("Err=%v Latest=%q HasNew=%v PageURL=%s", info.Err, info.Latest, info.HasNew, info.PageURL)
	if info.Err != nil {
		t.Fatalf("真实网络下检查更新失败（降级没兜住）: %v", info.Err)
	}
	if info.Latest == "" {
		t.Fatal("真实网络下没取到版本号")
	}
}
