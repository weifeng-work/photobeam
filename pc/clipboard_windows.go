package main

// PhotoBeam 剪贴板管线（Stage 2/3）— 协议 §5.2
// 收到图片 → 32bpp DIB + PNG 双格式写入剪贴板（附排除标记，不进 Win+V/云剪贴板）
// → 60 秒窗口（序号比对，用户中途复制则不还原）→ 自动还原原内容
// 快照/还原不碰 delayed rendering（读不到的格式跳过）[MiniMax 坑 3]
// 文案只承诺"不进本机剪贴板历史" [MiniMax 坑 4]
// 写入拆两个短 Open/Close 周期 [R-5]：全局剪贴板独占时间从 ~800ms 降到 <50ms；
// 周期之间的空档让系统剪贴板历史的延迟处理先行完成（否则实测 win32k 0xC0000005 杀进程 [C-04]）

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	// JPEG/PNG 解码注册（image.Decode 需要）
	_ "image/jpeg"
)

// ---------- Win32 ----------

var (
	user32CB   = windows.NewLazySystemDLL("user32.dll")
	kernel32CB = windows.NewLazySystemDLL("kernel32.dll")
	gdi32CB    = windows.NewLazySystemDLL("gdi32.dll")

	procOpenClipboard      = user32CB.NewProc("OpenClipboard")
	procCloseClipboard     = user32CB.NewProc("CloseClipboard")
	procEmptyClipboard     = user32CB.NewProc("EmptyClipboard")
	procSetClipboardData   = user32CB.NewProc("SetClipboardData")
	procGetClipboardData   = user32CB.NewProc("GetClipboardData")
	procGetClipboardOwner  = user32CB.NewProc("GetClipboardOwner")
	procEnumClipboardFmt   = user32CB.NewProc("EnumClipboardFormats")
	procGetSeqNumber       = user32CB.NewProc("GetClipboardSequenceNumber")
	procGetClipboardFmtNam = user32CB.NewProc("GetClipboardFormatNameW")
	procRegClipFmtW        = user32CB.NewProc("RegisterClipboardFormatW")
	procGlobalAlloc        = kernel32CB.NewProc("GlobalAlloc")
	procGlobalLock         = kernel32CB.NewProc("GlobalLock")
	procGlobalUnlock       = kernel32CB.NewProc("GlobalUnlock")
	procGlobalSize         = kernel32CB.NewProc("GlobalSize")
	procGlobalFree         = kernel32CB.NewProc("GlobalFree")
)

const (
	cfDIB        = 8
	gmemMoveable = 0x0002
)

// 非"裸字节可忠实还原"的标准格式 [R-1]：还原会把句柄/指针塞回剪贴板（Word/PPT 崩溃风险）
const (
	cfBitmap      = 2  // HBITMAP（GDI 句柄）
	cfMetafilePct = 3  // HMETAFILE
	cfPalette     = 9  // HPALETTE
	cfEnhMetafile = 14 // HENHMETAFILE
)

var (
	fmtPNG        uint16
	fmtCanHistory uint16
	fmtCanUpload  uint16
	fmtOnce       sync.Once
	fmtInitErr    string
)

func initClipboardFormats() {
	fmtOnce.Do(func() {
		fmtPNG = regFmt("PNG")
		fmtCanHistory = regFmt("CanIncludeInClipboardHistory")
		fmtCanUpload = regFmt("CanUploadToCloudClipboard")
		if fmtPNG == 0 || fmtCanHistory == 0 || fmtCanUpload == 0 {
			fmtInitErr = "RegisterClipboardFormat 失败"
			logf("剪贴板格式注册失败: PNG=%d hist=%d up=%d", fmtPNG, fmtCanHistory, fmtCanUpload)
		}
	})
}

func regFmt(name string) uint16 {
	p, _ := windows.UTF16PtrFromString(name)
	r, _, _ := procRegClipFmtW.Call(uintptr(unsafe.Pointer(p)))
	return uint16(r)
}

// ---------- 全局内存工具 ----------

func globalAllocCopy(data []byte) uintptr {
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(len(data)))
	if h == 0 {
		return 0
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return 0
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(p)), len(data)), data)
	procGlobalUnlock.Call(h)
	return h
}

func globalRead(h uintptr) []byte {
	sz, _, _ := procGlobalSize.Call(h)
	if sz == 0 || sz > 256<<20 { // 上限防 GlobalSize 异常值导致 OOM
		return nil
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return nil
	}
	out := make([]byte, sz)
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(p)), sz))
	procGlobalUnlock.Call(h)
	return out
}

// openClipboardRetry 有界重试 [F-02]：剪贴板是全局独占资源，截图时刻最易被占用
func openClipboardRetry() bool {
	for attempt := 1; attempt <= 3; attempt++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// ---------- 排除标记 [P0-02] ----------

func setExclusionMarkers() bool {
	// DWORD 0 = 排除（不进 Win+V 历史 / 不上传云剪贴板）
	ok := true
	for _, f := range []uint16{fmtCanHistory, fmtCanUpload} {
		h := globalAllocCopy(make([]byte, 4))
		if h == 0 {
			ok = false
			continue
		}
		if r, _, _ := procSetClipboardData.Call(uintptr(f), h); r == 0 {
			logf("排除标记写入失败 fmt=%d", f) // 降级不致命：用户会进 Win+V 历史但数据正确
			ok = false
		}
	}
	return ok
}

// ---------- 快照与还原（60 秒还原 [协议 §5.2]） ----------

func clipFmtName(fmt uint32) string {
	buf := make([]uint16, 128)
	n, _, _ := procGetClipboardFmtNam.Call(uintptr(fmt), uintptr(unsafe.Pointer(&buf[0])), 128)
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

type clipSnap struct {
	fmts  []uint32
	datas [][]byte
}

// 快照当前剪贴板可忠实还原的格式。
// 跳过：delayed rendering（GetClipboardData=0）、排除标记（DWORD 0 无还原语义）、
// OLE 内部格式（内容含跨进程指针，字节复制无法忠实还原）、
// GDI 句柄类标准格式（CF_BITMAP/CF_METAFILEPICT/CF_PALETTE/CF_ENHMETAFILE）[R-1]。
// 前提：调用方已 OpenClipboard 持有打开态——本函数绝不自行 Open/Close
//（内部 Close 会关掉调用方的打开态，实测 1418 [C-04]）。
// 无主剪贴板（owner=0：内容属已退出进程）→ 整体跳过：还原无意义且会触发 win32k AV [C-04]。
func snapshotClipboard() *clipSnap {
	if ow, _, _ := procGetClipboardOwner.Call(); ow == 0 {
		logf("无主剪贴板，跳过快照（不还原，规避 win32k AV）")
		return nil
	}
	snap := &clipSnap{}
	var fmts []uint32
	f := uint32(0)
	for {
		r, _, _ := procEnumClipboardFmt.Call(uintptr(f))
		f = uint32(r)
		if f == 0 {
			break
		}
		fmts = append(fmts, f)
	}
	for _, fmt := range fmts {
		if fmt == uint32(fmtCanHistory) || fmt == uint32(fmtCanUpload) {
			continue
		}
		switch fmt {
		case cfBitmap, cfMetafilePct, cfPalette, cfEnhMetafile:
			continue
		}
		if fmt > 0xC000 {
			name := clipFmtName(fmt)
			switch name {
			case "DataObject", "Ole Private Data", "Link Source", "Link Source Descriptor",
				"Object Descriptor", "Embed Source", "Embed Theme", "OwnerLink", "ObjectLink",
				"RenPrivateItem", "RenPrivateSource", "RenPrivateData":
				continue
			}
		}
		d, _, _ := procGetClipboardData.Call(uintptr(fmt))
		if d == 0 {
			continue // delayed rendering 或 NULL
		}
		data := globalRead(d)
		if len(data) == 0 {
			continue
		}
		snap.fmts = append(snap.fmts, fmt)
		snap.datas = append(snap.datas, data)
	}
	if len(snap.fmts) == 0 {
		return nil
	}
	return snap
}

func restoreClipboard(snap *clipSnap) {
	if snap == nil {
		return
	}
	if !openClipboardRetry() {
		logf("剪贴板还原失败: OpenClipboard 被占用") // [R-2] 不再静默放弃
		return
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	for i, fmt := range snap.fmts {
		h := globalAllocCopy(snap.datas[i])
		if h == 0 {
			logf("还原失败: GlobalAlloc fmt=%d", fmt)
			continue
		}
		if r, _, _ := procSetClipboardData.Call(uintptr(fmt), h); r == 0 {
			logf("还原失败: SetClipboardData fmt=%d", fmt)
		}
	}
}

// ---------- 图片 → DIB [MiniMax 坑 1：CF_DIB 非 CF_BITMAP] ----------

// 解码文件 → 32bpp BGRX bottom-up CF_DIB（BITMAPINFOHEADER + 像素）
// PNG 格式 = 原始文件字节（如果源是 PNG）或重新编码
func prepareImageData(path string) (dib []byte, pngData []byte, w, h int, err error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, nil, 0, 0, e
	}
	img, _, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		return nil, nil, 0, 0, derr
	}
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()
	rowSize := w * 4
	dataSize := rowSize * h
	dib = make([]byte, 40+dataSize)
	binary.LittleEndian.PutUint32(dib[0:4], 40)
	binary.LittleEndian.PutUint32(dib[4:8], uint32(w))
	binary.LittleEndian.PutUint32(dib[8:12], uint32(h)) // 正数 = bottom-up
	binary.LittleEndian.PutUint16(dib[12:14], 1)
	binary.LittleEndian.PutUint16(dib[14:16], 32)
	binary.LittleEndian.PutUint32(dib[20:24], uint32(dataSize))
	off := 40
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			dib[off] = byte(bl >> 8)
			dib[off+1] = byte(g >> 8)
			dib[off+2] = byte(r >> 8)
			dib[off+3] = 0xFF
			off += 4
		}
	}
	// PNG 格式：源是 PNG → 原始字节；否则重新编码（让偏好 PNG 的应用拿到无损版）
	if bytes.HasPrefix(raw, []byte("\x89PNG")) {
		pngData = raw
	} else {
		var buf bytes.Buffer
		if encErr := png.Encode(&buf, img); encErr == nil {
			pngData = buf.Bytes()
		}
	}
	return dib, pngData, w, h, nil
}

// ---------- 简单文本写入（诊断一键复制用；用户主动复制语义，不快照不还原） ----------

func setClipboardTextSimple(text string) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cbMu.Lock() // [S3-6] 与 60 秒还原互斥，防极端时序互相 EmptyClipboard
	defer cbMu.Unlock()
	if !openClipboardRetry() {
		return false
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	u16 := utf16Encode(text)
	h := globalAllocCopy(u16)
	if h == 0 {
		return false
	}
	r, _, _ := procSetClipboardData.Call(13, h) // CF_UNICODETEXT
	return r != 0
}

func utf16Encode(s string) []byte {
	u := make([]uint16, 0, len(s)*2+2)
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			u = append(u, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		} else {
			u = append(u, uint16(r))
		}
	}
	u = append(u, 0)
	out := make([]byte, len(u)*2)
	for i, v := range u {
		out[i*2] = byte(v)
		out[i*2+1] = byte(v >> 8)
	}
	return out
}

// ---------- 写入剪贴板 + 60 秒还原 ----------

var cbMu sync.Mutex

// 写入图片到剪贴板（附排除标记），快照原内容，60 秒后还原。
// 由 flushQueue 落盘成功后异步调用 [MiniMax 坑 6：不阻塞消息循环]。
// 剪贴板打开态绑定 OS 线程——goroutine 会被调度器迁移，
// 必须 LockOSThread 把整段 Win32 序列钉在同一线程（否则 1418 [C-04]）。
// [R-5] 两个短周期：周期1 = 空+标记+DIB（<50ms 独占）；空档 350ms；周期2 = PNG。
func writeClipboardWithRestore(dib, pngData []byte, seconds int, onRestore func()) (ok bool, snapNil bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cbMu.Lock()
	defer cbMu.Unlock()
	initClipboardFormats()
	if fmtInitErr != "" {
		logf("剪贴板写入失败: %s", fmtInitErr)
		return false, true
	}

	// ---- 周期 1：快照 + 空 + 排除标记 + CF_DIB ----
	if !openClipboardRetry() {
		logf("剪贴板写入失败: OpenClipboard 三次重试均被占用")
		return false, true
	}
	snap := snapshotClipboard() // 无主剪贴板 → nil（不还原，规避 AV [C-04]）
	if r, _, e := procEmptyClipboard.Call(); r == 0 {
		procCloseClipboard.Call()
		logf("剪贴板写入失败: EmptyClipboard err=%v", e)
		return false, true
	}
	markersOK := setExclusionMarkers()
	h := globalAllocCopy(dib)
	if h == 0 {
		procCloseClipboard.Call()
		logf("剪贴板写入失败: GlobalAlloc(len=%d)", len(dib))
		return false, true
	}
	r, _, werr := procSetClipboardData.Call(cfDIB, h)
	if r == 0 {
		procCloseClipboard.Call()
		logf("剪贴板写入失败: SetClipboardData(CF_DIB) len=%d winerr=%d", len(dib), werr)
		return false, true
	}
	procCloseClipboard.Call() // 周期 1 结束——剪贴板即刻让出

	// ---- 空档：让系统剪贴板历史的延迟处理完成（防 win32k AV [C-04]） ----
	time.Sleep(350 * time.Millisecond)

	// ---- 周期 2：PNG ----
	if len(pngData) > 0 && fmtPNG != 0 {
		if openClipboardRetry() {
			if hp := globalAllocCopy(pngData); hp != 0 {
				if r, _, _ := procSetClipboardData.Call(uintptr(fmtPNG), hp); r == 0 {
					logf("PNG 格式写入失败（DIB 已就位，粘贴不受影响）")
				}
			}
			procCloseClipboard.Call()
		} else {
			logf("PNG 写入跳过：剪贴板被占用（DIB 已就位，粘贴不受影响）") // [S3-9]
		}
	}
	time.Sleep(250 * time.Millisecond)

	// 写入后的序号（60 秒还原的比对基准 [MiniMax 坑 2]）
	r2, _, _ := procGetSeqNumber.Call()
	seqAfterWrite := uint32(r2)
	if markersOK {
		logf("剪贴板写入完成（不进 Win+V 历史）dib=%dB png=%dB", len(dib), len(pngData))
	} else {
		logf("剪贴板写入完成（排除标记失败：本次将进 Win+V 历史）dib=%dB png=%dB", len(dib), len(pngData)) // [S3-7]
	}

	// 60 秒后：序号未变（用户没复制过别的）→ 还原原内容
	time.AfterFunc(time.Duration(seconds)*time.Second, func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cbMu.Lock()
		defer cbMu.Unlock()
		r2, _, _ := procGetSeqNumber.Call()
		if uint32(r2) != seqAfterWrite {
			// 用户中途复制了别的内容 → 不还原（尊重用户操作）
			logf("剪贴板已被外部更新，跳过还原")
			return
		}
		if snap == nil {
			logf("无快照（无主剪贴板），无需还原")
		} else {
			restoreClipboard(snap)
			logf("剪贴板已还原原内容")
		}
		if onRestore != nil {
			onRestore()
		}
	})
	return true, snap == nil
}
