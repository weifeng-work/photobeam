package main

// [Stage 10] 设备目录名 —— 多台手机同步到同一台 PC 时的物理隔离
//
// 背景：v1.7 引入 `rel_path` 后，PC 端按手机端相册目录建子目录
// （如 `Received\Screenshots\`）。但**目录名只来自手机端的相册目录，
// 不含设备维度** —— 两台手机都传 `DCIM/Screenshots/` 时会落到同一个目录，
// 用户分不清哪张图来自哪台手机。
//
// 本文件定义**设备目录名**的算法：`<设备名>-<指纹前8位>`，例如
//
//	<PHONE-MODEL>-<fp8>
//
// 设计要点：
//   · **指纹后缀防重名**：两台同型号手机（如家里两台小米）设备名相同，
//     指纹后缀保证目录唯一，同时保留可读性。
//   · **设备名同样按不可信输入处理**：设备名来自对端 HELLO 的 `dev_name`，
//     可能含路径分隔符 / 盘符 / 保留设备名 → 必须清洗（复用 relpath 的规则）。
//   · **PC 端是唯一真相源**：手机端不自己算，而是由 PC 在握手应答里回传
//     （`pair_ok` / `auth_ok` 的 `dev_dir` 字段，**不参与 transcript**），
//     避免两端各算一套规则导致显示与实际目录不一致。

import (
	"path/filepath"
	"strings"
)

// devDirFpLen：指纹取前几位做后缀。8 位 = 32 bit，足够区分家庭/办公室场景下的
// 同型号设备（实测配置里设备指纹是 16 位 hex）。
const devDirFpLen = 8

// deviceDirName：算出某台设备在 PC 端的目录名。
//
// 参数：
//   - devName：对端上报的设备名（不可信输入，可能含任意字符）
//   - fp：设备指纹 hex（PC 端自己算出来的，可信）
//
// 返回：形如 `<PHONE-MODEL>-<fp8>` 的目录名；devName 清洗后为空时退化为
// `dev-<fp8>`（保证一定有可读且唯一的目录名）。
//
// 返回值**保证**：不含路径分隔符、不含 `..`、不含盘符、不含保留设备名，
// 拼到接收目录下不会逃出去（纵深防御见 resolveDeviceDir）。
func deviceDirName(devName, fp string) string {
	clean := cleanPathSegment(devName) // 复用 relpath 的清洗：去非法字符/分隔符/保留名/首尾点
	if clean == "" {
		clean = "dev"
	}
	return clean + "-" + shortFp(fp)
}

// resolveDeviceDir：把设备目录名拼到接收目录下，并做**二次逃逸检查**。
//
// 纵深防御：即使 deviceDirName 的清洗逻辑将来有疏漏，这里还能挡住
// 「拼出来的路径不在接收目录之下」的情况。
func resolveDeviceDir(saveDir, devDir string) string {
	if devDir == "" {
		return saveDir
	}
	full := filepath.Join(saveDir, devDir)
	base := filepath.Clean(saveDir)
	if full != base && !strings.HasPrefix(full, base+string(filepath.Separator)) {
		// 不该发生（deviceDirName 已保证），但纵深防御：退化为根目录
		logf("设备目录 %q 逃出接收目录，退回根目录", devDir)
		return saveDir
	}
	return full
}

// shortFp：指纹取前 N 位（小写 hex）。指纹为空时返回 "unknown"。
func shortFp(fp string) string {
	fp = strings.ToLower(strings.TrimSpace(fp))
	if len(fp) > devDirFpLen {
		fp = fp[:devDirFpLen]
	}
	if fp == "" {
		return "unknown"
	}
	return fp
}
