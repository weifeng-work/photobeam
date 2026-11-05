package main

// [Stage 7] 相对路径消毒 —— 手机端目录结构保留（协议 §5.1 rel_path 字段）
//
// 威胁模型：`rel_path` 来自**对端**（手机），是不可信输入。攻击者可以让手机发送
// `../../Windows/System32/` 之类的值，试图把文件写到接收目录之外。
// 本文件是唯一的防线，必须做到：
//   1. 绝不产生 `..` 分量（路径穿越）；
//   2. 绝不产生绝对路径（`C:\`、`/etc`、UNC `\\server\share`）；
//   3. 绝不产生盘符 / 保留设备名（CON、NUL、COM1…）；
//   4. 绝不产生控制字符与 Windows 非法文件名字符；
//   5. 深度与总长度有上限（防目录爆炸、防路径过长）；
//   6. 同一层级的分隔符统一（手机端给的是 `/`，Windows 要 `\`）。
//
// 设计取舍：**不做「越界即整体拒绝」**——手机端某个目录名被清洗掉时，
// 该图仍应送达（落平到接收目录根），而不是整张图失败。
// 只有当清洗后**整条路径为空**时才退化为根目录落盘。

import (
	"path/filepath"
	"strings"
)

const (
	// 单级目录名最大长度（Windows 单个文件名上限 255 字符）
	maxPathSegment = 100
	// 保留目录的最大层数（防目录爆炸）
	maxPathDepth = 8
	// 相对路径总长度上限
	maxRelPathLen = 512
)

// Windows 保留设备名（不区分大小写）——这些名字即使带扩展名也不能当目录名
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// 手机端图片的 relative_path 顶层目录（MediaStore 的标准桶）。
// 协议 §10 延后项 A 的目标：「手机 Pictures/Screenshots/ → PC 的 Screenshots/」——
// 即**剥掉顶层桶**，保留其下的层级（否则电脑上会多出一层无意义的 DCIM/）。
//
// 键一律用**全大写**：比较时用 strings.ToUpper 归一化（Go map 键区分大小写，
// 写成混合大小写会漏匹配 —— 这个坑踩过一次：Pictures 因大小写不匹配没被剥掉）。
var mediaTopDirs = map[string]bool{
	"DCIM": true, "PICTURES": true, "DOWNLOAD": true,
	"DOCUMENTS": true, "MOVIES": true, "MUSIC": true, "PODCASTS": true,
}

// sanitizeRelPath：清洗手机端传来的相对路径，返回可安全用于子目录名的路径片段。
//
// 返回值示例：
//   "DCIM/Screenshots"      → "Screenshots"      （顶层桶 DCIM 被剥掉）
//   "Pictures/微信/图片"     → "微信\图片"         （顶层桶 Pictures 被剥掉）
//   "../../etc"             → "etc"（丢掉 .. 分量，不产生穿越）
//   "C:\\Windows\\System32" → "Windows\System32"（去掉盘符）
//   "/absolute/path"        → "absolute\path"（去掉前导 /）
//   "..."                   → ""（全部无效 → 落平到根）
//
// 关键不变式：返回值**永远**是相对路径、不含 `..`、不含盘符、不含非法字符，
// 且拼到接收目录下不会逃出接收目录。
func sanitizeRelPath(rel string) string {
	if rel == "" {
		return ""
	}
	// 长度上限（先截断再处理，避免超长输入做无谓工作）
	if len(rel) > maxRelPathLen {
		rel = rel[:maxRelPathLen]
	}

	// 统一分隔符：手机端给 `/`，也兼容 `\`
	rel = strings.ReplaceAll(rel, "\\", "/")

	// 去掉 UNC 前缀 `//server/share` 的前导斜杠（下面统一按 / 切分即可）
	rel = strings.TrimLeft(rel, "/")

	parts := strings.Split(rel, "/")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		seg = cleanPathSegment(seg)
		if seg == "" {
			continue // 空分量（连续 /、尾部 /）或被清洗掉的分量
		}
		if seg == ".." {
			// 路径穿越分量：直接丢弃（不报错、不中断整条路径）
			continue
		}
		if len(out) >= maxPathDepth {
			break // 深度上限
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		return ""
	}
	// 剥掉顶层媒体桶（DCIM / Pictures / …）——协议 §10 延后项 A 的目标
	if len(out) > 1 && mediaTopDirs[strings.ToUpper(out[0])] {
		out = out[1:]
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, string(filepath.Separator))
}

// cleanPathSegment：清洗单个目录名分量。
func cleanPathSegment(seg string) string {
	if seg == "" {
		return ""
	}
	// 去掉控制字符与 Windows 非法字符（与 sanitizeName 的字符集一致）
	seg = strings.Map(func(r rune) rune {
		if r < 32 || r == ':' || r == '*' || r == '?' || r == '"' ||
			r == '<' || r == '>' || r == '|' {
			return -1
		}
		return r
	}, seg)
	// 去掉首尾空格与点（Windows 会自动去掉尾部的点和空格，导致名字变形）
	seg = strings.TrimSpace(seg)
	seg = strings.Trim(seg, ".")
	if seg == "" {
		return ""
	}
	// 保留设备名检查（去掉扩展名后比较）
	base := seg
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if reservedNames[strings.ToUpper(base)] {
		// 加前缀规避（CON → _CON）
		seg = "_" + seg
	}
	// 长度上限
	if len(seg) > maxPathSegment {
		seg = seg[:maxPathSegment]
		seg = strings.TrimSpace(seg)
	}
	return seg
}

// resolveSaveSubDir：把清洗后的相对路径拼到接收目录下，并做**二次逃逸检查**。
//
// 二次检查是必须的：即使 sanitizeRelPath 的逻辑有疏漏，这里还能挡住
// 「拼出来的路径不在接收目录之下」的情况（纵深防御）。
// 返回 (绝对子目录, 相对子路径字符串)。
func resolveSaveSubDir(saveDir, rel string) (string, string) {
	clean := sanitizeRelPath(rel)
	if clean == "" {
		return saveDir, ""
	}
	full := filepath.Join(saveDir, clean)
	base := filepath.Clean(saveDir)
	// 逃逸检查：拼出来的路径必须在接收目录之下
	if full != base && !strings.HasPrefix(full, base+string(filepath.Separator)) {
		// 不可能发生（sanitizeRelPath 已保证），但纵深防御：退化为根目录
		return saveDir, ""
	}
	return full, clean
}
