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
	"fmt"
	"image"
	"image/draw"
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
	// fmtPhotoBeamMarker：本程序专属的归属标记格式。
	// 作用 = 识别「剪贴板当前内容是我们自己刚写进去的图片」——
	// 否则连续写两张图时，第二张的快照会把**第一张 PhotoBeam 图**当成「用户原内容」，
	// 60 秒后还原回去的是上一张图，而不是用户原本复制的东西（B8 的真因）。
	fmtPhotoBeamMarker uint16
	fmtOnce            sync.Once
	fmtInitErr         string
)

func initClipboardFormats() {
	fmtOnce.Do(func() {
		fmtPNG = regFmt("PNG")
		fmtCanHistory = regFmt("CanIncludeInClipboardHistory")
		fmtCanUpload = regFmt("CanUploadToCloudClipboard")
		fmtPhotoBeamMarker = regFmt("PhotoBeam.Image.Marker")
		if fmtPNG == 0 || fmtCanHistory == 0 || fmtCanUpload == 0 || fmtPhotoBeamMarker == 0 {
			fmtInitErr = "RegisterClipboardFormat 失败"
			logf("剪贴板格式注册失败: PNG=%d hist=%d up=%d marker=%d",
				fmtPNG, fmtCanHistory, fmtCanUpload, fmtPhotoBeamMarker)
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
			// [修复] 写入失败时句柄仍归我们所有 → 必须 GlobalFree，否则每次失败泄漏一份全局内存
			procGlobalFree.Call(h)
			logf("排除标记写入失败 fmt=%d", f) // 降级不致命：用户会进 Win+V 历史但数据正确
			ok = false
		}
	}
	return ok
}

// 写入我们的归属标记（PhotoBeam.Image.Marker）：
// 剪贴板里带上这个专属格式，之后就能认出「当前内容是我们自己写的图」。
func setOwnerMarker(gen uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], gen)
	h := globalAllocCopy(b[:])
	if h == 0 {
		return
	}
	if r, _, _ := procSetClipboardData.Call(uintptr(fmtPhotoBeamMarker), h); r == 0 {
		// [修复] 归属标记写入失败 → 句柄仍归我们，必须 GlobalFree 防泄漏
		procGlobalFree.Call(h)
		logf("归属标记写入失败 fmt=%d", fmtPhotoBeamMarker)
	}
}

// clipboardHasOurMarker：剪贴板当前内容是否是 PhotoBeam 自己写的图。
// 前提：调用方已 OpenClipboard 持有打开态。
func clipboardHasOurMarker() bool {
	if fmtPhotoBeamMarker == 0 {
		return false
	}
	h, _, _ := procGetClipboardData.Call(uintptr(fmtPhotoBeamMarker))
	return h != 0
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
		// 归属标记是我们自己的元数据，**不能进快照**：
		// 否则还原之后剪贴板还带着「这是 PhotoBeam 写的」的标记，
		// 下一次写入会误判成「当前是我们自己的图」而复用旧快照。
		if fmt == uint32(fmtPhotoBeamMarker) {
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
//
// [Stage 6 · 延迟优化] 关键路径拆分：
// 原实现 prepareImageData 一次做完「解码 + 逐像素转 DIB + PNG 重编码」，
// 其中 PNG 重编码（实测 846ms @1080x2400）完全不在关键路径上——
// CF_DIB 在周期 1 就已就位，用户那时就能 Ctrl+V。PNG 只是给偏好 PNG 的应用的无损备选。
//
// 新管线：
//   prepareImageDataFast → 只做「解码 + DIB 转换」（实测约 100ms）→ 立刻写剪贴板（用户可粘贴）
//   preparePNGAsync     → 后台线程做 PNG 重编码 → 完成后补写 PNG 格式（周期 2）
//
// 收益：可粘贴时刻提前约 850~1100ms（实测 3.4s → 约 2.0s，见 docs/stage5-evidence.md §5.12）。

// 解码文件 → 32bpp BGRX bottom-up CF_DIB（BITMAPINFOHEADER + 像素）
// PNG 格式 = 原始文件字节（如果源是 PNG）或重新编码（让偏好 PNG 的应用拿到无损版）
func prepareImageData(path string) (dib []byte, pngData []byte, w, h int, err error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, nil, 0, 0, e
	}
	img, _, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		return nil, nil, 0, 0, derr
	}
	dib, w, h, err = buildDIB(img)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	pngData = encodePNGIfNeeded(raw, img)
	return dib, pngData, w, h, nil
}

// buildDIB：图像 → 32bpp BGRX bottom-up 的 CF_DIB 负载（40 字节 BITMAPINFOHEADER + 像素）
//
// [Stage 6 · 延迟优化] 原实现用 img.At(x,y) 逐像素取色（1080x2400 = 259 万次接口调用 + 边界检查，
// 实测 278ms）。改为：draw.Draw 一次性把源图转成 *image.RGBA（标准库对 YCbCr 有专门快路径），
// 再按行做 BGR 交换拷贝 —— 实测 33ms，提速约 8 倍，且像素值与原实现一致
// （色彩转换交给标准库，不手写 YCbCr→RGB 公式，避免精度漂移）。
func buildDIB(img image.Image) (dib []byte, w, h int, err error) {
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, 0, 0, fmt.Errorf("图片尺寸非法 %dx%d", w, h)
	}
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)

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
		src := rgba.Pix[y*rgba.Stride : y*rgba.Stride+rowSize]
		for x := 0; x < w; x++ {
			p := src[x*4 : x*4+4]
			dib[off] = p[2]   // B
			dib[off+1] = p[1] // G
			dib[off+2] = p[0] // R
			dib[off+3] = 0xFF
			off += 4
		}
	}
	return dib, w, h, nil
}

// encodePNGIfNeeded：源是 PNG → 原始字节；否则重新编码成 PNG。
func encodePNGIfNeeded(raw []byte, img image.Image) []byte {
	if bytes.HasPrefix(raw, []byte("\x89PNG")) {
		return raw
	}
	var buf bytes.Buffer
	if encErr := png.Encode(&buf, img); encErr == nil {
		return buf.Bytes()
	}
	return nil
}

// prepareImageDataFast：只做关键路径（解码 + DIB），不做 PNG 重编码。
// 用于「先让用户能粘贴，PNG 后台补写」的新管线。
func prepareImageDataFast(path string) (dib []byte, w, h int, err error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, 0, 0, e
	}
	img, _, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		return nil, 0, 0, derr
	}
	return buildDIB(img)
}

// preparePNGAsync：在后台把落盘文件重编码成 PNG（源已是 PNG 则直接用原字节）。
// 供 writeClipboardDIBThenPNG 的周期 2 调用。
func preparePNGAsync(path string) []byte {
	raw, e := os.ReadFile(path)
	if e != nil {
		logf("PNG 后台编码：读取失败 %v", e)
		return nil
	}
	if bytes.HasPrefix(raw, []byte("\x89PNG")) {
		return raw
	}
	img, _, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		logf("PNG 后台编码：解码失败 %v", derr)
		return nil
	}
	var buf bytes.Buffer
	if encErr := png.Encode(&buf, img); encErr != nil {
		logf("PNG 后台编码：编码失败 %v", encErr)
		return nil
	}
	return buf.Bytes()
}

// ---------- 写入剪贴板 + 60 秒还原 ----------

var cbMu sync.Mutex

// [Stage 5 收尾 · B8] 剪贴板写入的「代际号」与「用户原内容快照」
//
// B8 的真因不是「旧定时器把新图还原掉了」（序号比对已经挡住了），
// 而是：**第二张图的快照抓到的是第一张 PhotoBeam 图** ——
// 它被当成了「用户原本复制的内容」，60 秒后还原回去的是上一张图，而不是用户的原内容。
//
// 修法：
//   · 每次写入带上归属标记（PhotoBeam.Image.Marker）；
//   · 写入前先看剪贴板是不是「我们自己的图」：
//       - 是 → 复用第一次写入前捕获的 clipOrigSnap（用户的真正原内容），不覆盖它；
//       - 否 → 重新捕获并存进 clipOrigSnap（用户在这期间换过内容，以新的为准）。
//   · clipGen 代际号：旧写入的还原定时器发现自己已被更新的一代取代 → 跳过（由新写入负责还原）。
var (
	clipGen     uint64     // 每次写入 +1
	clipOrigSnap *clipSnap // 用户真正原内容（跨连续多次写入保留）
)

// writeClipboardWithRestore：写入图片到剪贴板（附排除标记），快照原内容，N 秒后还原。
// 由 flushQueue 落盘成功后异步调用 [MiniMax 坑 6：不阻塞消息循环]。
// 剪贴板打开态绑定 OS 线程——goroutine 会被调度器迁移，
// 必须 LockOSThread 把整段 Win32 序列钉在同一线程（否则 1418 [C-04]）。
//
// [Stage 6 · 延迟优化] 拆成两个阶段，**函数返回即代表用户可以 Ctrl+V**：
//   阶段 1（同步、关键路径）：快照 + 空 + 排除标记 + CF_DIB → 剪贴板立刻可粘贴
//   阶段 2（后台 goroutine）：PNG 重编码（846ms）→ 补写 PNG 格式 + 启动还原定时器
//
// 原实现在 CF_DIB 写入后固定 sleep 350+250=600ms 才返回，且 PNG 编码在调用前同步做完
// （846ms）——两者都不影响粘贴却把「可粘贴时刻」推后了约 1.4s。
// 现在：CF_DIB 写完立即返回，PNG 在后台补齐，还原定时器改由阶段 2 负责。
//
// [R-5] 两个短周期：周期1 = 空+标记+DIB（<50ms 独占）；空档 350ms；周期2 = PNG。
func writeClipboardWithRestore(dib []byte, pngProvider func() []byte, seconds int, onRestore func()) (ok bool, snapNil bool) {
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
	var snap *clipSnap
	ours := clipboardHasOurMarker()
	if ours {
		// 是 → 复用「用户真正的原内容」，**不要**把上一张 PhotoBeam 图当成原内容
		// （注意：这里根本不去 snapshotClipboard —— 对自己的图做快照没有意义，
		//  还会打出一条「无主剪贴板，跳过快照」的误导日志）
		if clipOrigSnap != nil {
			snap = clipOrigSnap
			logf("剪贴板快照：当前是本程序上一张图 → 复用用户原内容快照（不把上一张图当原内容）")
		} else {
			snap = nil // 没有原内容快照 → 本轮不还原
			logf("剪贴板快照：当前是本程序上一张图，但首次写入时没有可还原的用户原内容 → 本轮不还原")
		}
	} else {
		// 否 → 用户当前剪贴板就是原内容，记下来（后续连续写入都还原回它）
		snap = snapshotClipboard()
		clipOrigSnap = snap
		if snap != nil {
			logf("剪贴板快照：已捕获用户原内容（%d 个格式）", len(snap.fmts))
		} else {
			logf("剪贴板快照：无用户原内容可捕获（无主剪贴板）")
		}
	}
	clipGen++
	myGen := clipGen
	if r, _, e := procEmptyClipboard.Call(); r == 0 {
		procCloseClipboard.Call()
		logf("剪贴板写入失败: EmptyClipboard err=%v", e)
		return false, true
	}
	markersOK := setExclusionMarkers()
	setOwnerMarker(myGen) // [B8] 归属标记：让下一次写入能认出「这是我们写的」
	h := globalAllocCopy(dib)
	if h == 0 {
		procCloseClipboard.Call()
		logf("剪贴板写入失败: GlobalAlloc(len=%d)", len(dib))
		return false, true
	}
	r, _, werr := procSetClipboardData.Call(cfDIB, h)
	if r == 0 {
		procCloseClipboard.Call()
		// [修复] SetClipboardData 失败 → h 仍归我们所有，必须 GlobalFree（否则每次失败泄漏 len(dib) 字节全局内存）
		procGlobalFree.Call(h)
		logf("剪贴板写入失败: SetClipboardData(CF_DIB) len=%d winerr=%d", len(dib), werr)
		return false, true
	}
	procCloseClipboard.Call() // 周期 1 结束——**剪贴板即刻让出，用户此时已可 Ctrl+V**

	// [修复·关键时序] 还原的基准序号必须**此刻**取：DIB 已写入、剪贴板刚让出。
	// 旧实现在后台 goroutine 里 sleep(350ms)+PNG 编码（合计约 1.2s）之后才取 —
	// 用户在这 ~1.2s 内复制的内容会被算进「序号未变」，还原定时器据此把用户
	// 刚复制的内容覆盖掉，违反 user-journey「窗口期内用户复制了别的内容则不还原」。
	r0, _, _ := procGetSeqNumber.Call()
	seqAtWrite := uint32(r0)

	// ---- 阶段 2（后台）：PNG 补写 + 还原定时器 ----
	// [Stage 6] 原来这里固定 sleep 350+250=600ms，现在全部搬到后台。
	// 代际号 myGen 传进去：若期间有更新的图写入，PNG 与还原定时器都要让位。
	schedulePNGBackfill(pngProvider, myGen, snap, seconds, onRestore, markersOK, seqAtWrite)

	return true, snap == nil
}

// schedulePNGBackfill：后台补写 PNG 格式 + 安排还原定时器。
//
// [Stage 6 · 延迟优化] 从关键路径剥离：调用方（writeClipboardWithRestore）在 CF_DIB
// 写入后立刻返回，用户马上就能粘贴；PNG 是给偏好 PNG 的应用的无损备选，晚几百毫秒无妨。
//
// 保留 [R-5] 的 350ms 空档（让系统剪贴板历史的延迟处理完成，防 win32k AV [C-04]）——
// 它现在只延后 PNG 的补写，不再延后用户的粘贴时刻。
func schedulePNGBackfill(pngProvider func() []byte, myGen uint64, snap *clipSnap, seconds int, onRestore func(), markersOK bool, seqAtWrite uint32) {
	go func() {
		// 空档：让系统剪贴板历史的延迟处理完成（防 win32k AV [C-04]）
		time.Sleep(350 * time.Millisecond)

		// 期间若已有更新的图写入 → 本轮 PNG 作废（否则会覆盖新图的剪贴板内容）
		// [修复] clipGen 的读必须在 cbMu 内：它是被 :482 持锁写的共享变量，
		// 之前这里裸读，-race 会报（多张连发时后台 goroutine 与写入路径并发）。
		cbMu.Lock()
		genNow := clipGen
		cbMu.Unlock()
		if genNow != myGen {
			logf("PNG 补写跳过：已被更新的图取代（代际 %d → %d）", myGen, genNow)
			return
		}

		// ---- 周期 2：PNG（后台编码，不在关键路径上）----
		var pngData []byte
		if pngProvider != nil {
			pngData = pngProvider()
		}
		if len(pngData) > 0 && fmtPNG != 0 {
			runtime.LockOSThread()
			cbMu.Lock()
			if clipGen == myGen {
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
			} else {
				logf("PNG 补写跳过：写入期间出现更新的图（代际 %d → %d）", myGen, clipGen)
			}
			cbMu.Unlock()
			runtime.UnlockOSThread()
		}

		if markersOK {
			logf("剪贴板写入完成（不进 Win+V 历史）png=%dB", len(pngData))
		} else {
			logf("剪贴板写入完成（排除标记失败：本次将进 Win+V 历史）png=%dB", len(pngData)) // [S3-7]
		}

		// N 秒后：序号未变（用户没复制过别的）→ 还原**用户原本的内容**
		time.AfterFunc(time.Duration(seconds)*time.Second, func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			cbMu.Lock()
			defer cbMu.Unlock()
			// [B8] 代际号检查：本次写入之后又写入了更新的图 → 本次还原作废
			// （由最新那次写入的定时器负责还原，避免旧定时器动到新图）。
			if clipGen != myGen {
				logf("剪贴板还原跳过：本次写入已被更新的一张图取代（代际 %d → %d），由最新写入负责还原", myGen, clipGen)
				return
			}
			r2, _, _ := procGetSeqNumber.Call()
			if uint32(r2) != seqAtWrite {
				// [B7] 两种「跳过」必须能区分开：
				//   · 用户自己复制了别的内容 → 尊重用户，不还原
				//   · （被本程序后续写入取代的情况上面已单独处理）
				logf("剪贴板还原跳过：剪贴板已被外部更新（用户复制了其他内容，序号 %d → %d），不还原", seqAtWrite, uint32(r2))
				return
			}
			if snap == nil {
				logf("无快照（无主剪贴板），无需还原")
			} else {
				restoreClipboard(snap)
				clipOrigSnap = nil // 原内容已还给用户 → 下一轮重新捕获
				logf("剪贴板已还原原内容")
			}
			if onRestore != nil {
				onRestore()
			}
		})
	}()
}
