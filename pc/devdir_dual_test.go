package main

// [Stage 10] 双机模拟测试 —— 验证多台手机同步到同一台 PC 时的隔离与幂等语义
//
// 场景：两台手机（不同型号、不同指纹）都配对了这台 PC，各自传图。
// 期望：
//   1. 两台手机落到**不同的设备目录**（<设备名>-<fp8>）；
//   2. 两台手机传**同一张图**（内容相同 → file_id 相同）时，**各存一份**，
//      不像旧实现那样第二台被静默吞掉；
//   3. 同一台手机重传同一张图 → 仍然幂等跳过（不重复落盘）。
//
// 这些是**纯逻辑**测试（不碰网络与磁盘），保证核心语义不回归。
// 测试数据用中性虚构值（真实设备名/指纹不应进仓库）。

import (
	"path/filepath"
	"testing"
)

// TestDualPhoneDirsSeparate：两台手机 → 不同设备目录
func TestDualPhoneDirsSeparate(t *testing.T) {
	const saveDir = `C:\Users\<user>\AppData\Local\PhotoBeam\Received`

	phoneA := deviceDirName("PhoneA", tFP1)
	phoneB := deviceDirName("PhoneB", tFP2)

	dirA := resolveDeviceDir(saveDir, phoneA)
	dirB := resolveDeviceDir(saveDir, phoneB)

	if dirA == dirB {
		t.Fatalf("两台手机落到同一目录：%q", dirA)
	}
	if filepath.Base(dirA) != "PhoneA-01234567" {
		t.Errorf("手机 A 目录名 = %q，期望 PhoneA-01234567", filepath.Base(dirA))
	}
	if filepath.Base(dirB) != "PhoneB-fedcba98" {
		t.Errorf("手机 B 目录名 = %q，期望 PhoneB-fedcba98", filepath.Base(dirB))
	}
	// 都在接收目录之下
	base := filepath.Clean(saveDir)
	for _, d := range []string{dirA, dirB} {
		if !filepathHasPrefix(d, base) {
			t.Errorf("目录 %q 不在接收目录 %q 之下", d, base)
		}
	}
}

// TestDualPhoneSameImageBothStored：两台手机传同一张图 → 幂等键不同 → 都能存
func TestDualPhoneSameImageBothStored(t *testing.T) {
	const fileID = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

	// 旧实现：只用 file_id 做键 → 两台手机命中同一个键 → 第二台被吞
	oldKey := fileID
	// 新实现：设备指纹 + file_id
	newKeyA := seenKeyFor(tFP1, fileID)
	newKeyB := seenKeyFor(tFP2, fileID)

	if oldKey == newKeyA {
		t.Errorf("旧键与新键相同 —— 加设备维度没起作用")
	}
	if newKeyA == newKeyB {
		t.Errorf("两台手机的新键仍相同：%q —— 第二台会被吞", newKeyA)
	}
	// 模拟幂等集合：先记入 A，B 再来时不应命中
	seen := map[string]bool{}
	seen[newKeyA] = true
	if seen[newKeyB] {
		t.Errorf("手机 B 命中了手机 A 的幂等键 —— 会丢图")
	}
	// 同设备重传 → 命中（幂等仍有效）
	seen[newKeyA] = true
	if !seen[seenKeyFor(tFP1, fileID)] {
		t.Errorf("同设备同图应命中幂等")
	}
}

// filepathHasPrefix：判断 p 是否在 base 之下（跨平台安全的字符串比较）
func filepathHasPrefix(p, base string) bool {
	if p == base {
		return true
	}
	sep := string(filepath.Separator)
	return len(p) > len(base) && p[:len(base)] == base && p[len(base):len(base)+len(sep)] == sep
}
