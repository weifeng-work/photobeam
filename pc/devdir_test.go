package main

// [Stage 10] 设备目录名的单元测试
//
// 关键不变式：
//   1. 目录名永远**唯一**（同名设备靠指纹后缀区分）；
//   2. 目录名永远**不含路径分隔符/盘符/..**（设备名是不可信输入）；
//   3. 目录名**可读**（保留设备名主体，用户一眼认得出）。
//
// 测试数据用**中性虚构值**（不是真实设备名/指纹，也不带尖括号占位符——
// 占位符里的 < > 会被清洗逻辑去掉，反而测不出真实行为）。
//
// 运行：cd pc && go test -run TestDeviceDir -v

import (
	"path/filepath"
	"strings"
	"testing"
)

// 测试用的中性虚构值
const (
	tFP1 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tFP2 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// TestDeviceDirNameBasic：基本形态
func TestDeviceDirNameBasic(t *testing.T) {
	cases := []struct {
		devName, fp, want string
	}{
		{"TestPhoneA", tFP1, "TestPhoneA-01234567"},
		{"Test Phone B", tFP2, "Test Phone B-fedcba98"},
		{"", tFP1, "dev-01234567"},
		{"   ", tFP1, "dev-01234567"},
		{"SomePhone", "", "SomePhone-unknown"},
	}
	for _, c := range cases {
		got := deviceDirName(c.devName, c.fp)
		if got != c.want {
			t.Errorf("deviceDirName(%q, %q) = %q，期望 %q", c.devName, c.fp, got, c.want)
		}
	}
}

// TestDeviceDirNameUnique：同型号两台手机 → 目录名不同
func TestDeviceDirNameUnique(t *testing.T) {
	a := deviceDirName("SameModel", tFP1)
	b := deviceDirName("SameModel", tFP2)
	if a == b {
		t.Errorf("两台同型号手机的目录名相同：%q —— 指纹后缀没起作用", a)
	}
	if !strings.HasPrefix(a, "SameModel-") || !strings.HasPrefix(b, "SameModel-") {
		t.Errorf("目录名应保留设备名前缀，实际：%q / %q", a, b)
	}
}

// TestDeviceDirNameSanitized：恶意/非法设备名必须被清洗
func TestDeviceDirNameSanitized(t *testing.T) {
	hostile := []string{
		"..\\..\\Windows",
		"../../etc",
		"C:\\Windows\\System32",
		"CON",
		"NUL",
		"bad\x00name",
		"a/b",
		"  spaced  ",
		"trailing.",
	}
	for _, h := range hostile {
		got := deviceDirName(h, tFP1)
		// 不能含路径分隔符
		if strings.ContainsAny(got, `\/`) {
			t.Errorf("deviceDirName(%q) = %q 含路径分隔符", h, got)
		}
		// 不能含 ..
		if strings.Contains(got, "..") {
			t.Errorf("deviceDirName(%q) = %q 含 '..'", h, got)
		}
		// 不能含盘符
		if len(got) >= 2 && got[1] == ':' {
			t.Errorf("deviceDirName(%q) = %q 含盘符", h, got)
		}
		// 不能是保留设备名（_CON 形式可以）
		base := strings.Split(got, "-")[0]
		if reservedNames[strings.ToUpper(base)] {
			t.Errorf("deviceDirName(%q) = %q 是保留设备名", h, got)
		}
		// 必须仍然唯一（带指纹后缀）
		if !strings.Contains(got, "01234567") {
			t.Errorf("deviceDirName(%q) = %q 丢了指纹后缀", h, got)
		}
	}
}

// TestDeviceDirNameEscapes：拼到接收目录下不能逃出去
func TestDeviceDirNameEscapes(t *testing.T) {
	base := `C:\Users\<user>\AppData\Local\PhotoBeam\Received`
	for _, h := range []string{"..\\..\\Windows", "../../etc", "C:\\Windows"} {
		dir := deviceDirName(h, tFP1)
		full := filepath.Join(base, dir)
		if !strings.HasPrefix(full, base+string(filepath.Separator)) {
			t.Errorf("deviceDirName(%q) 拼出的路径 %q 逃出了接收目录", h, full)
		}
		// resolveDeviceDir 的二次检查也要挡住
		if got := resolveDeviceDir(base, dir); !strings.HasPrefix(got, base+string(filepath.Separator)) {
			t.Errorf("resolveDeviceDir 放行了逃逸路径：%q", got)
		}
	}
}

// TestSeenKeyDeviceScoped：幂等键必须含设备维度
//
// 这是 Stage 10 的核心安全/正确性断言：
// 两台手机传**同一张图**（内容相同 → file_id 相同）时，
// 幂等键必须能区分它们 → 两台各落一份，不能互相吞。
func TestSeenKeyDeviceScoped(t *testing.T) {
	const fileID = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	a := seenKeyFor(tFP1, fileID)
	b := seenKeyFor(tFP2, fileID)
	if a == b {
		t.Errorf("两台设备的幂等键相同：%q —— 第二台会被判 duplicate 吞掉", a)
	}
	if !strings.Contains(a, fileID) || !strings.Contains(a, "01234567") {
		t.Errorf("幂等键 %q 未同时包含设备与文件标识", a)
	}
	// 同设备同文件 → 必须相同（幂等仍然有效）
	if seenKeyFor(tFP1, fileID) != a {
		t.Errorf("同设备同文件的幂等键应相同")
	}
}
