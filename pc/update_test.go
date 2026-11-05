package main

// [Stage 11] 版本号比对的单元测试
//
// 关键不变式：
//   · **按数字段比较**，不是字符串比较 —— "1.10.0" > "1.9.0"（字符串比会判反）
//   · 段数不同 → 缺失段按 0（"1.2" == "1.2.0"）
//   · v 前缀可选（tag 是 "v1.2.0"，App 里是 "1.2.0"）
//
// 运行：cd pc && go test -run TestCompareVersions -v

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.0", "1.1.0", 1},
		{"1.1.0", "1.2.0", -1},
		{"1.2.0", "1.2.0", 0},
		// 经典陷阱：字符串比较会把 "1.10.0" 判成比 "1.9.0" 小
		{"1.10.0", "1.9.0", 1},
		{"1.9.0", "1.10.0", -1},
		// v 前缀
		{"v1.3.0", "1.2.0", 1},
		{"1.3.0", "v1.2.0", 1},
		// 段数不同
		{"1.2", "1.2.0", 0},
		{"1.2.1", "1.2", 1},
		{"1.2", "1.2.1", -1},
		// 多位主版本
		{"2.0.0", "1.99.99", 1},
		// 预发布后缀
		{"1.2.0-beta", "1.2.0", -1},   // beta 比正式版旧
		{"1.2.0", "1.2.0-beta", 1},
		// 空
		{"", "1.0.0", -1},
		{"1.0.0", "", 1},
		{"", "", 0},
	}
	for _, c := range cases {
		got := compareVersions(c.a, c.b)
		if got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

// TestCompareVersionsSymmetry：比较必须对称（a>b ⟺ b<a）
func TestCompareVersionsSymmetry(t *testing.T) {
	pairs := [][2]string{
		{"1.10.0", "1.9.0"},
		{"2.0.0", "1.0.0"},
		{"1.2.3", "1.2.3"},
		{"1.2", "1.2.1"},
	}
	for _, p := range pairs {
		ab := compareVersions(p[0], p[1])
		ba := compareVersions(p[1], p[0])
		if ab != -ba {
			t.Errorf("compareVersions 不对称：(%q,%q)=%d 而 (%q,%q)=%d", p[0], p[1], ab, p[1], p[0], ba)
		}
	}
}

// TestSplitVersion：拆段
func TestSplitVersion(t *testing.T) {
	if got := splitVersion("v1.2.3"); len(got) != 3 || got[0] != "1" || got[2] != "3" {
		t.Errorf("splitVersion(\"v1.2.3\") = %v", got)
	}
	if got := splitVersion(""); len(got) != 0 {
		t.Errorf("splitVersion(\"\") = %v，期望空", got)
	}
}
