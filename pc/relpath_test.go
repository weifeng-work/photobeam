package main

// [Stage 7] 相对路径消毒的单元测试
//
// 这是**安全关键**代码：rel_path 来自对端（手机），是不可信输入。
// 一旦清洗有疏漏，攻击者就能让文件写到接收目录之外。
// 因此测试必须覆盖：正常路径、路径穿越、绝对路径、盘符、UNC、保留设备名、
// 控制字符、超长路径、深度超限，以及最终的「拼出来一定在接收目录下」不变式。
//
// 运行：cd pc && go test -run TestSanitizeRelPath -v

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeRelPathNormal：正常路径应保留结构
func TestSanitizeRelPathNormal(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// 顶层媒体桶被剥掉（协议 §10 延后项 A：手机 Pictures/Screenshots/ → PC 的 Screenshots/）
		{"DCIM/Screenshots", "Screenshots"},
		{"Pictures/微信/图片", "微信\\图片"},
		{"DCIM/Camera/子目录", "Camera\\子目录"},
		// 非媒体顶层目录保留
		{"Camera", "Camera"},
		{"MyFolder/Sub", "MyFolder\\Sub"},
		{"DCIM/Camera/", "Camera"},
		{"A/B/C/D", "A\\B\\C\\D"},
		{"with space/dir", "with space\\dir"},
	}
	for _, c := range cases {
		got := sanitizeRelPath(c.in)
		if got != c.want {
			t.Errorf("sanitizeRelPath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeRelPathTopLevelStrip：顶层桶剥离的边界
func TestSanitizeRelPathTopLevelStrip(t *testing.T) {
	// 只有一层时（"DCIM"）不剥——剥掉就空了
	if got := sanitizeRelPath("DCIM"); got != "DCIM" {
		t.Errorf("sanitizeRelPath(\"DCIM\") = %q，单层不应剥离", got)
	}
	// 非媒体顶层不剥
	if got := sanitizeRelPath("DCIMX/a"); got != "DCIMX\\a" {
		t.Errorf("sanitizeRelPath(\"DCIMX/a\") = %q，非媒体顶层不应剥离", got)
	}
	// 小写也认
	if got := sanitizeRelPath("dcim/Screenshots"); got != "Screenshots" {
		t.Errorf("sanitizeRelPath(\"dcim/Screenshots\") = %q，应剥离小写桶名", got)
	}
}

// TestSanitizeRelPathTraversal：路径穿越必须被挡住
func TestSanitizeRelPathTraversal(t *testing.T) {
	cases := []struct {
		in  string
	}{
		{"../../etc"},
		{".."},
		{"../.."},
		{"DCIM/../../../Windows/System32"},
		{"..\\..\\Windows"},
		{"a/../../b"},
		{"./.././etc"},
	}
	for _, c := range cases {
		got := sanitizeRelPath(c.in)
		// 结果按 \ 切分后，任何一段都不能是 ".." 或 "."
		for _, seg := range strings.Split(got, string(filepath.Separator)) {
			if seg == ".." || seg == "." {
				t.Errorf("sanitizeRelPath(%q) = %q，含非法分量 %q", c.in, got, seg)
			}
		}
		if strings.Contains(got, "..") {
			t.Errorf("sanitizeRelPath(%q) = %q 含 '..'", c.in, got)
		}
	}
}

// TestSanitizeRelPathAbsolute：绝对路径与盘符必须被剥掉
func TestSanitizeRelPathAbsolute(t *testing.T) {
	cases := []string{
		"C:\\Windows\\System32",
		"C:/Windows/System32",
		"/etc/passwd",
		"//server/share/payload",
		"\\\\server\\share\\payload",
		"D:",
	}
	for _, in := range cases {
		got := sanitizeRelPath(in)
		if filepath.IsAbs(got) {
			t.Errorf("sanitizeRelPath(%q) = %q 是绝对路径", in, got)
		}
		// 不能残留盘符（形如 "C:"）
		if len(got) >= 2 && got[1] == ':' {
			t.Errorf("sanitizeRelPath(%q) = %q 残留盘符", in, got)
		}
	}
}

// TestSanitizeRelPathReserved：Windows 保留设备名必须被规避
func TestSanitizeRelPathReserved(t *testing.T) {
	for _, in := range []string{"CON", "con", "NUL", "COM1", "LPT9", "CON/photos"} {
		got := sanitizeRelPath(in)
		for _, seg := range strings.Split(got, string(filepath.Separator)) {
			base := seg
			if i := strings.IndexByte(base, '.'); i >= 0 {
				base = base[:i]
			}
			if reservedNames[strings.ToUpper(base)] {
				t.Errorf("sanitizeRelPath(%q) = %q 含保留设备名 %q", in, got, base)
			}
		}
	}
}

// TestSanitizeRelPathIllegalChars：控制字符与 Windows 非法字符必须被去掉
func TestSanitizeRelPathIllegalChars(t *testing.T) {
	got := sanitizeRelPath("bad\x00dir\x1fname:what*ever?")
	for _, bad := range []string{"\x00", "\x1f", ":", "*", "?"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitizeRelPath 结果 %q 含非法字符 %q", got, bad)
		}
	}
}

// TestSanitizeRelPathLimits：长度与深度上限
func TestSanitizeRelPathLimits(t *testing.T) {
	// 超长单级
	long := strings.Repeat("a", 500)
	got := sanitizeRelPath(long)
	if len(got) > maxPathSegment {
		t.Errorf("单级长度 %d 超过上限 %d", len(got), maxPathSegment)
	}
	// 超深
	deep := strings.Repeat("d/", 30)
	got2 := sanitizeRelPath(deep)
	depth := len(strings.Split(got2, string(filepath.Separator)))
	if depth > maxPathDepth {
		t.Errorf("深度 %d 超过上限 %d", depth, maxPathDepth)
	}
	// 超长整体
	got3 := sanitizeRelPath(strings.Repeat("x/", 500))
	if len(got3) > maxRelPathLen {
		t.Errorf("总长 %d 超过上限 %d", len(got3), maxRelPathLen)
	}
}

// TestResolveSaveSubDirEscape：不变式——拼出来一定在接收目录之下
func TestResolveSaveSubDirEscape(t *testing.T) {
	base := `C:\Users\<user>\AppData\Local\PhotoBeam\Received`
	hostile := []string{
		"../../../../Windows/System32",
		"..\\..\\..\\Windows",
		"C:\\Windows",
		"/etc",
		"DCIM/../../../../etc",
		"....//....//etc",
		"",           // 空 → 落平到根
		"   ",        // 空白 → 落平到根
		"...",       // 全是点 → 落平到根
		"CON",
	}
	for _, h := range hostile {
		full, rel := resolveSaveSubDir(base, h)
		cleanBase := filepath.Clean(base)
		if full != cleanBase && !strings.HasPrefix(full, cleanBase+string(filepath.Separator)) {
			t.Errorf("resolveSaveSubDir(%q) = %q，逃出了接收目录 %q", h, full, cleanBase)
		}
		if rel != "" && (strings.Contains(rel, "..") || filepath.IsAbs(rel)) {
			t.Errorf("resolveSaveSubDir(%q) 的相对路径 %q 不合法", h, rel)
		}
	}
}

// TestResolveSaveSubDirNormal：正常路径应生成子目录
func TestResolveSaveSubDirNormal(t *testing.T) {
	base := `C:\Users\<user>\AppData\Local\PhotoBeam\Received`
	// 顶层桶被剥掉 → Received\Screenshots
	full, rel := resolveSaveSubDir(base, "DCIM/Screenshots")
	want := filepath.Join(base, "Screenshots")
	if full != want {
		t.Errorf("resolveSaveSubDir = %q，期望 %q", full, want)
	}
	if rel != "Screenshots" {
		t.Errorf("rel = %q，期望 %q", rel, "Screenshots")
	}
	// 多层保留
	full2, rel2 := resolveSaveSubDir(base, "DCIM/Camera/Sub")
	want2 := filepath.Join(base, "Camera", "Sub")
	if full2 != want2 || rel2 != "Camera\\Sub" {
		t.Errorf("resolveSaveSubDir = (%q, %q)，期望 (%q, %q)", full2, rel2, want2, "Camera\\Sub")
	}
	// 空路径 → 根目录
	full3, rel3 := resolveSaveSubDir(base, "")
	if full3 != filepath.Clean(base) || rel3 != "" {
		t.Errorf("空 rel_path 应落平到根目录，得到 (%q, %q)", full3, rel3)
	}
}
