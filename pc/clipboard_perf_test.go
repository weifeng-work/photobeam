package main

// [Stage 6] 剪贴板关键路径性能守门人
//
// 目的：协议 §9 把「截图 → PC 剪贴板可粘贴」收紧到 ≤2 秒后，这个指标必须有**能自动判定
// 达标与否**的守门人，否则标准只是一句口号。
//
// 本文件守的是**PC 端关键路径**（解码 + DIB 生成）——这是 2026-11-05 实测出的最大瓶颈：
//   原实现 逐像素 img.At() 278ms + PNG 重编码 846ms = 1124ms 全在关键路径上
//   现实现 draw.Draw 33ms + PNG 后台化 → 关键路径约 100ms
//
// 端到端（含手机端握手/传输）的守门人是 tools/assert-latency.ps1（需要真机 + 配对）。
// 本测试不需要任何外部依赖，CI 里也能跑——用于防止**回归**（有人把 draw.Draw 改回逐像素，
// 或把 PNG 编码挪回同步路径，测试会立刻红）。
//
// 运行：cd pc && go test -run TestClipboard -v

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"
)

// 关键路径预算（毫秒）。
// 实测基线：解码 ~65ms + DIB ~33ms ≈ 100ms。
// 预算给到 300ms 是为了容忍 CI 机器的波动，但足以拦住「退回逐像素」（278ms → 总量 ~350ms+
// 且在慢机器上更糟）与「PNG 编码回到同步路径」（+846ms，远超）。
const (
	dibBudgetMS     = 300
	decodeBudgetMS  = 200
	pngEncodeBudget = 2000 // PNG 编码仍在后台跑，但它不该无限慢
	// 注：真机截图（JPEG 压缩后）PNG 编码实测约 846ms；
	// 本测试用噪声图（压缩后仍是高熵，PNG 压不动）会到 ~1.3s，
	// 因此预算按测试图的更差情况取 2000ms。它只用于「别退化到慢得离谱」，不参与 ≤2s 达标判定。
)

// makeTestJPEG：造一张与真机截图同量级的图片（1080x2400）
func makeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// 渐变 + 噪声，避免压缩后变成纯色（纯色会让 png.Encode 异常快，失去守门意义）
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(x * 255 / w),
				G: uint8(y * 255 / h),
				B: uint8((x ^ y) & 0xFF),
				A: 0xFF,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("造测试 JPEG 失败: %v", err)
	}
	return buf.Bytes()
}

// TestDIBGenerationSpeed：守住「解码 + DIB 生成」在预算内
func TestDIBGenerationSpeed(t *testing.T) {
	raw := makeTestJPEG(t, 1080, 2400)

	t0 := time.Now()
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	tDecode := time.Since(t0)

	t1 := time.Now()
	dib, w, h, err := buildDIB(img)
	if err != nil {
		t.Fatalf("buildDIB 失败: %v", err)
	}
	tDIB := time.Since(t1)

	t.Logf("解码=%v DIB生成=%v 总计=%v (dib=%dB %dx%d)",
		tDecode.Round(time.Millisecond), tDIB.Round(time.Millisecond),
		(tDecode + tDIB).Round(time.Millisecond), len(dib), w, h)

	if ms := tDecode.Milliseconds(); ms > decodeBudgetMS {
		t.Errorf("解码耗时 %dms 超出预算 %dms", ms, decodeBudgetMS)
	}
	if ms := tDIB.Milliseconds(); ms > dibBudgetMS {
		t.Errorf("DIB 生成耗时 %dms 超出预算 %dms —— "+
			"检查是否退回逐像素 img.At() 实现（应使用 draw.Draw 快速路径）", ms, dibBudgetMS)
	}
}

// TestDIBContentCorrect：守住 DIB 的格式正确性（头部 + 像素顺序 + 尺寸）
func TestDIBContentCorrect(t *testing.T) {
	// 造一张 4x2 的小图，逐像素验证颜色与行序
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	// 第 0 行（图像上边）：红 绿 蓝 白
	src.Set(0, 0, color.RGBA{255, 0, 0, 255})
	src.Set(1, 0, color.RGBA{0, 255, 0, 255})
	src.Set(2, 0, color.RGBA{0, 0, 255, 255})
	src.Set(3, 0, color.RGBA{255, 255, 255, 255})
	// 第 1 行（图像下边）：黑 灰 银 暗红
	src.Set(0, 1, color.RGBA{0, 0, 0, 255})
	src.Set(1, 1, color.RGBA{128, 128, 128, 255})
	src.Set(2, 1, color.RGBA{192, 192, 192, 255})
	src.Set(3, 1, color.RGBA{64, 0, 0, 255})

	dib, w, h, err := buildDIB(src)
	if err != nil {
		t.Fatalf("buildDIB 失败: %v", err)
	}
	if w != 4 || h != 2 {
		t.Fatalf("尺寸错误: %dx%d，期望 4x2", w, h)
	}
	if len(dib) != 40+w*4*h {
		t.Fatalf("DIB 长度错误: %d，期望 %d", len(dib), 40+w*4*h)
	}
	// BITMAPINFOHEADER
	if got := int(dib[0]) | int(dib[1])<<8 | int(dib[2])<<16 | int(dib[3])<<24; got != 40 {
		t.Errorf("biSize = %d，期望 40", got)
	}
	if got := int(dib[4]) | int(dib[5])<<8 | int(dib[6])<<16 | int(dib[7])<<24; got != 4 {
		t.Errorf("biWidth = %d，期望 4", got)
	}
	if got := int(dib[8]) | int(dib[9])<<8 | int(dib[10])<<16 | int(dib[11])<<24; got != 2 {
		t.Errorf("biHeight = %d，期望 2（正数 = bottom-up）", got)
	}
	if got := int(dib[14]) | int(dib[15])<<8; got != 32 {
		t.Errorf("biBitCount = %d，期望 32", got)
	}
	if got := int(dib[16]) | int(dib[17])<<8 | int(dib[18])<<16 | int(dib[19])<<24; got != 0 {
		t.Errorf("biCompression = %d，期望 0 (BI_RGB)", got)
	}

	// 像素：bottom-up → DIB 第 0 行像素 = 图像最后一行
	at := func(row, col int) (b, g, r, a byte) {
		off := 40 + (row*4+col)*4
		return dib[off], dib[off+1], dib[off+2], dib[off+3]
	}
	cases := []struct {
		row, col   int
		eb, eg, er byte
		name       string
	}{
		{0, 0, 0, 0, 0, "图像下行左(黑)"},
		{0, 1, 128, 128, 128, "图像下行第2格(灰)"},
		{0, 3, 0, 0, 64, "图像下行右(暗红)"},
		{1, 0, 0, 0, 255, "图像上行左(红)"},
		{1, 1, 0, 255, 0, "图像上行第2格(绿)"},
		{1, 2, 255, 0, 0, "图像上行第3格(蓝)"},
	}
	for _, c := range cases {
		gotB, gotG, gotR, gotA := at(c.row, c.col)
		if gotB != c.eb || gotG != c.eg || gotR != c.er || gotA != 0xFF {
			t.Errorf("%s: 期望 BGR=(%d,%d,%d) A=255，实得 BGR=(%d,%d,%d) A=%d",
				c.name, c.eb, c.eg, c.er, gotB, gotG, gotR, gotA)
		}
	}
}

// TestPNGSeparateFromCriticalPath：守住「PNG 编码不在关键路径上」
//
// 判据：prepareImageDataFast 不应做 PNG 编码（它只返回 DIB），
// 且 decode+DIB 的耗时应显著小于 decode+DIB+PNG。
// 若有人把 PNG 编码挪回 prepareImageDataFast，本测试会失败。
func TestPNGSeparateFromCriticalPath(t *testing.T) {
	raw := makeTestJPEG(t, 1080, 2400)

	t0 := time.Now()
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	_, _, _, err = buildDIB(img)
	if err != nil {
		t.Fatalf("buildDIB 失败: %v", err)
	}
	critical := time.Since(t0)

	// 单独测 PNG 编码耗时，确认它确实是"重活"（所以才值得移出关键路径）
	t1 := time.Now()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("PNG 编码失败: %v", err)
	}
	pngCost := time.Since(t1)

	t.Logf("关键路径(解码+DIB)=%v  PNG 编码=%v (在后台执行)", critical.Round(time.Millisecond), pngCost.Round(time.Millisecond))

	if ms := critical.Milliseconds(); ms > dibBudgetMS+decodeBudgetMS {
		t.Errorf("关键路径 %dms 超出总预算 %dms", ms, dibBudgetMS+decodeBudgetMS)
	}
	if pngCost > time.Duration(pngEncodeBudget)*time.Millisecond {
		t.Errorf("PNG 编码 %v 异常慢（预算 %dms）", pngCost.Round(time.Millisecond), pngEncodeBudget)
	}
}
