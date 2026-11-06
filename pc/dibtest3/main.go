// dibtest3：干净的 StretchDIBits 行为测定（目标 DIB 段足够大，绝不越界写）
// 目标：搞清 StretchDIBits 对「32bpp bottom-up 源 DIB」在什么条件下返回 0。
//
// [Stage 13] 这是 Stage 5 的一次性**调试残留**，不是产品代码 ——
// 当初为定位 StretchDIBits 返回 0 的条件而写的探针，结论已记进
// docs/stage5-evidence.md。放在仓库里只为留档（Initial commit 就在）。
//
// 但它是一个独立 main package：CI 里 `go test ./...` 会把它当第二个包编译
// （无害，但输出里多一行 "? photobeam/pc/dibtest3 [no test files]"）。
// 加 //go:build ignore → Go 工具链完全忽略它，不再参与任何 build/vet/test，
// 同时不必为「删掉它」去重写公开仓库的历史。
//
//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"unsafe"

	_ "image/jpeg"

	"golang.org/x/sys/windows"
)

var (
	gdi32                = windows.NewLazySystemDLL("gdi32.dll")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pStretchDIBits      = gdi32.NewProc("StretchDIBits")
	pStretchBlt         = gdi32.NewProc("StretchBlt")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
)

const (
	srcCopy = 0x00CC0020
	canvas  = 2048 // 目标画布边长（足够大，避免越界）
)

func setLe32(b []byte, v uint32) { b[0] = byte(v); b[1] = byte(v >> 8); b[2] = byte(v >> 16); b[3] = byte(v >> 24) }
func setLe16(b []byte, v uint16) { b[0] = byte(v); b[1] = byte(v >> 8) }
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func solidDIB(w, h int) []byte {
	d := make([]byte, 40+w*h*4)
	setLe32(d[0:4], 40)
	setLe32(d[4:8], uint32(w))
	setLe32(d[8:12], uint32(h))
	setLe16(d[12:14], 1)
	setLe16(d[14:16], 32)
	setLe32(d[20:24], uint32(w*h*4))
	for i := 0; i < w*h; i++ {
		d[40+i*4], d[40+i*4+1], d[40+i*4+2], d[40+i*4+3] = 0x40, 0x80, 0xC0, 0xFF
	}
	return d
}

func buildDIB(path string) ([]byte, int, int) {
	raw, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		panic(err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dib := make([]byte, 40+w*4*h)
	setLe32(dib[0:4], 40)
	setLe32(dib[4:8], uint32(w))
	setLe32(dib[8:12], uint32(h))
	setLe16(dib[12:14], 1)
	setLe16(dib[14:16], 32)
	setLe32(dib[20:24], uint32(w*4*h))
	off := 40
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			dib[off], dib[off+1], dib[off+2], dib[off+3] = byte(bl>>8), byte(g>>8), byte(r>>8), 0xFF
			off += 4
		}
	}
	return dib, w, h
}

type target struct {
	memDC uintptr
	bits  uintptr
	sec   uintptr
}

func newTarget() *target {
	memDC, _, _ := pCreateCompatibleDC.Call(0)
	var hdr [40]byte
	setLe32(hdr[0:4], 40)
	setLe32(hdr[4:8], canvas)
	setLe32(hdr[8:12], ^uint32(canvas-1)) // 负 = top-down
	setLe16(hdr[12:14], 1)
	setLe16(hdr[14:16], 32)
	t := &target{memDC: memDC}
	var bits uintptr
	sec, _, _ := pCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&hdr[0])), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	t.sec, t.bits = sec, bits
	pSelectObject.Call(memDC, sec)
	return t
}

func (t *target) clear() {
	for i := uintptr(0); i < uintptr(canvas*canvas*4); i++ {
		*(*byte)(unsafe.Pointer(t.bits + i)) = 0
	}
}

func (t *target) nonBlack() int {
	c := 0
	for i := 0; i < canvas*canvas; i++ {
		if *(*byte)(unsafe.Pointer(t.bits + uintptr(i*4))) != 0 {
			c++
		}
	}
	return c
}

func main() {
	path := os.Args[1]
	dib, w, h := buildDIB(path)
	fmt.Printf("真实图 %dx%d dib=%dB\n", w, h, len(dib))

	t := newTarget()

	// T1：真实 DIB → 各种目标尺寸
	for _, ds := range []int{45, 64, 200, 450} {
		t.clear()
		r, _, _ := pStretchDIBits.Call(t.memDC, 0, 0, uintptr(uint32(ds)), uintptr(uint32(ds)),
			0, 0, uintptr(uint32(w)), uintptr(uint32(h)),
			uintptr(unsafe.Pointer(&dib[40])), uintptr(unsafe.Pointer(&dib[0])), 0, srcCopy)
		fmt.Printf("T1 真实DIB dest=%3d  返回=%-6d 非黑=%d\n", ds, r, t.nonBlack())
	}

	// T2：合成纯色 DIB，同样尺寸 900x1600 → 验证是否与数据有关
	sd := solidDIB(w, h)
	for _, ds := range []int{45, 200} {
		t.clear()
		r, _, _ := pStretchDIBits.Call(t.memDC, 0, 0, uintptr(uint32(ds)), uintptr(uint32(ds)),
			0, 0, uintptr(uint32(w)), uintptr(uint32(h)),
			uintptr(unsafe.Pointer(&sd[40])), uintptr(unsafe.Pointer(&sd[0])), 0, srcCopy)
		fmt.Printf("T2 合成DIB dest=%3d  返回=%-6d 非黑=%d\n", ds, r, t.nonBlack())
	}

	// T3：合成方形 900x900（对照）
	sq := solidDIB(900, 900)
	t.clear()
	r3, _, _ := pStretchDIBits.Call(t.memDC, 0, 0, 900, 900, 0, 0, 900, 900,
		uintptr(unsafe.Pointer(&sq[40])), uintptr(unsafe.Pointer(&sq[0])), 0, srcCopy)
	fmt.Printf("T3 合成900x900 dest=900 返回=%-6d 非黑=%d\n", r3, t.nonBlack())

	// T4：CreateDIBSection + StretchBlt（备选路径），源=真实 DIB
	t.clear()
	var sh [40]byte
	setLe32(sh[0:4], 40)
	setLe32(sh[4:8], uint32(w))
	setLe32(sh[8:12], uint32(h)) // bottom-up
	setLe16(sh[12:14], 1)
	setLe16(sh[14:16], 32)
	setLe32(sh[20:24], uint32(w*h*4))
	var srcBits uintptr
	srcSec, _, _ := pCreateDIBSection.Call(t.memDC, uintptr(unsafe.Pointer(&sh[0])), 0, uintptr(unsafe.Pointer(&srcBits)), 0, 0)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(srcBits)), w*h*4), dib[40:])
	srcDC, _, _ := pCreateCompatibleDC.Call(0)
	pSelectObject.Call(srcDC, srcSec)
	r4, _, _ := pStretchBlt.Call(t.memDC, 0, 0, 200, 200, srcDC, 0, 0, uintptr(uint32(w)), uintptr(uint32(h)), srcCopy)
	fmt.Printf("T4 DIB段+StretchBlt dest=200 返回=%-6d 非黑=%d\n", r4, t.nonBlack())

	// 导出 T4 结果，确认方向
	out := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			idx := y*canvas + x
			b0 := *(*byte)(unsafe.Pointer(t.bits + uintptr(idx*4)))
			b1 := *(*byte)(unsafe.Pointer(t.bits + uintptr(idx*4+1)))
			b2 := *(*byte)(unsafe.Pointer(t.bits + uintptr(idx*4+2)))
			out.Set(x, y, color.RGBA{b2, b1, b0, 255})
		}
	}
	f, _ := os.Create("dibtest3_stretchblt.png")
	png.Encode(f, out)
	f.Close()
	fmt.Println("T4 结果已导出 dibtest3_stretchblt.png")

	// T5/T6/T7/T8：头 vs 数据 二分
	realHdr := make([]byte, 40)
	copy(realHdr, dib[:40])
	synHdr := make([]byte, 40)
	copy(synHdr, sd[:40])

	try := func(name string, hdr, data []byte, dw, dh, sw, sh int) {
		t.clear()
		r, _, _ := pStretchDIBits.Call(t.memDC, 0, 0, uintptr(uint32(dw)), uintptr(uint32(dh)),
			0, 0, uintptr(uint32(sw)), uintptr(uint32(sh)),
			uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&hdr[0])), 0, srcCopy)
		fmt.Printf("%-34s 返回=%-6d 非黑=%d\n", name, r, t.nonBlack())
	}
	try("T5 真头+合成数据", realHdr, sd[40:], 200, 200, w, h)
	try("T6 合成头+真数据", synHdr, dib[40:], 200, 200, w, h)
	try("T7 真头+真数据(重跑)", realHdr, dib[40:], 200, 200, w, h)
	try("T8 合成头+合成数据(重跑)", synHdr, sd[40:], 200, 200, w, h)

	// T9：真数据但 alpha 全置 0
	noAlpha := make([]byte, len(dib)-40)
	copy(noAlpha, dib[40:])
	for i := 3; i < len(noAlpha); i += 4 {
		noAlpha[i] = 0
	}
	try("T9 真头+真数据(alpha=0)", realHdr, noAlpha, 200, 200, w, h)

	// T10：真数据但整体填成常量（保持长度）
	constData := make([]byte, len(dib)-40)
	for i := range constData {
		constData[i] = 0x7F
	}
	try("T10 真头+常量数据(同长度)", realHdr, constData, 200, 200, w, h)

	// T11：真数据的前 100 行（缩小源高度）
	shortData := make([]byte, 40+w*100*4)
	copy(shortData[:40], realHdr)
	setLe32(shortData[8:12], 100)
	setLe32(shortData[20:24], uint32(w*100*4))
	copy(shortData[40:], dib[40:w*100*4])
	try("T11 真头+真数据(仅100行)", shortData[:40], shortData[40:], 200, 200, w, 100)
}
