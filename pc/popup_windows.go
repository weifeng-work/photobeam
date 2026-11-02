package main

// 右下角自绘弹窗 [MiniMax 坑 5：自绘无边框置顶窗，不用 WinRT toast / 模态 MessageBox]
// Win32 裸实现（x/sys/windows，零新依赖）
// WS_EX_NOACTIVATE 不抢焦点 + WS_EX_TOOLWINDOW 不进任务栏 + TOPMOST 置顶
// STATIC 子控件显示文本；5 秒自动关闭（WM_TIMER），点击立即关闭

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wsPopup         = 0x80000000
	wsVisible       = 0x10000000
	wsChild         = 0x40000000
	wsChildVisible  = 0x40000000 | 0x10000000
	wsexTopmost     = 0x00000008
	wsexToolWindow  = 0x00000080
	wsexNoActivate  = 0x08000000
	smCxScreen      = 0
	smCyScreen      = 1
	wmClose         = 0x0010
	wmDestroy       = 0x0002
	wmTimer         = 0x0113
	wmLButtonDown   = 0x0201
	wmCtlColorStatic = 0x0138
)

var (
	procRegisterClassExW  = user32CB.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32CB.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32CB.NewProc("DefWindowProcW")
	procDestroyWindowP    = user32CB.NewProc("DestroyWindow")
	procSetTimerP         = user32CB.NewProc("SetTimer")
	procGetMessageWP      = user32CB.NewProc("GetMessageW")
	procTranslateMessageP = user32CB.NewProc("TranslateMessage")
	procDispatchMessageWP = user32CB.NewProc("DispatchMessageW")
	procPostQuitMessageP  = user32CB.NewProc("PostQuitMessage")
	procGetSysMetricsP    = user32CB.NewProc("GetSystemMetrics")
	procSysParamsInfoW    = user32CB.NewProc("SystemParametersInfoW")
	procPostMessageW      = user32CB.NewProc("PostMessageW")
	procGetModuleHandleWP = kernel32CB.NewProc("GetModuleHandleW")
	procCreateSolidBrushP = gdi32CB.NewProc("CreateSolidBrush")
	procSetTextColorP     = gdi32CB.NewProc("SetTextColor")
	procSetBkModeP        = gdi32CB.NewProc("SetBkMode")
	procBeginPaintP       = user32CB.NewProc("BeginPaint")
	procEndPaintP         = user32CB.NewProc("EndPaint")
	procDrawTextWP        = user32CB.NewProc("DrawTextW")
	procSelectObjectP     = gdi32CB.NewProc("SelectObject")
	procCreateFontWP      = gdi32CB.NewProc("CreateFontW")
	procCreateCompatibleDC = gdi32CB.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32CB.NewProc("CreateDIBSection")
	procStretchBlt         = gdi32CB.NewProc("StretchBlt")
	procDeleteDC           = gdi32CB.NewProc("DeleteDC")
	procDeleteObject       = gdi32CB.NewProc("DeleteObject")
)

// 弹窗文字（自绘 [WM_PAINT]，不用 STATIC 子控件——主题化 STATIC 会无视
// WM_CTLCOLORSTATIC 的深底白字，实测渲染成白底白字不可读）
var popupText struct{ title, l1, l2 *uint16 }

type wndClassEx struct {
	CbSize     uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

var (
	popupClassOnce sync.Once
	popupBrushOnce sync.Once // 与 popupClassOnce 分离：Once 不可重入，嵌套 Do 同一 Once 会自锁死
	popupBrush     uintptr
)

func getPopupBrush() uintptr {
	popupBrushOnce.Do(func() {
		r, _, _ := procCreateSolidBrushP.Call(0x001F1F1F)
		popupBrush = r
	})
	return popupBrush
}

// 显示右下角弹窗（连接状态指示灯的可见形态）
// [R-3] popupMu 把弹窗全程串行化：popupText 是包级全局，连拍两图时若并行弹窗，
// 先弹的窗口会画出后一张的文件名。持锁覆盖"设置文字→创建窗口→消息循环"全程。
var activePopupHwnd uintptr // 原子访问 [S3-8]

// 缩略图（DIB 32bpp bottom-up，来自 prepareImageData，绘制时直接 StretchDIBits 零转码）
var popupThumb struct {
	dib  []byte
	w, h int
	has  bool
}

// 带缩略图的同步弹窗（onFile 专用）
func showPopupThumb(title, line1, line2 string, durationMs int, dib []byte, w, h int) {
	showPopupEx(title, line1, line2, durationMs, dib, w, h)
}

func showPopup(title, line1, line2 string, durationMs int) {
	showPopupEx(title, line1, line2, durationMs, nil, 0, 0)
}

// 唯一实现：popupMu 全程串行化（同一时刻只有一个弹窗，文字不会串台 [R-3]）。
// [修复 2026-11-02] 原先 showPopupThumb 先置 popupThumb.has=true，紧接着调用的
// showPopup 又把 has 置回 false —— 缩略图状态在窗口建好之前就被清掉了，
// 所以 WM_PAINT 永远走不到画缩略图的分支（v_popup2.png 只有文字没有图）。
// 现改为：文字与缩略图状态都由 createPopupWindow 在 popupMu 内一次性设置。
func showPopupEx(title, line1, line2 string, durationMs int, dib []byte, w, h int) {
	// [S3-8] 顶掉还在显示的旧弹窗（跨线程只能 PostMessage，不能直接 DestroyWindow）
	if old := atomic.LoadUintptr(&activePopupHwnd); old != 0 {
		procPostMessageW.Call(old, wmClose, 0, 0)
	}
	go func() {
		popupMu.Lock()
		defer popupMu.Unlock()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		registerPopupClassOnce()
		hwnd := createPopupWindow(title, line1, line2, dib, w, h)
		if hwnd == 0 {
			return
		}
		atomic.StoreUintptr(&activePopupHwnd, uintptr(hwnd))
		procSetTimerP.Call(uintptr(hwnd), 1, uintptr(durationMs), 0)
		pumpMessages()
		atomic.StoreUintptr(&activePopupHwnd, 0)
	}()
}

var popupMu sync.Mutex

func registerPopupClassOnce() {
	popupClassOnce.Do(func() {
		clsName, _ := windows.UTF16PtrFromString("PhotoBeamPopup")
		hInst, _, _ := procGetModuleHandleWP.Call(0)
		wc := wndClassEx{
			CbSize:     uint32(unsafe.Sizeof(wndClassEx{})),
			Background: windows.Handle(getPopupBrush()),
			ClassName:  clsName,
		}
		wc.WndProc = windows.NewCallback(popupWndProc)
		wc.Instance = windows.Handle(hInst)
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	})
}

const wmPaint = 0x000F

// 单行文本绘制（DrawTextW：CENTER|VCENTER|SINGLELINE，透明底）
// cchText 必须 -1（表示以 \0 结尾）——传 0 = 零字符，什么都不画（实测返回 1）
func drawTextLine(hdc uintptr, s *uint16, x, y, wd, ht int32, color uint32) {
	if s == nil {
		return
	}
	rc := struct{ L, T, R, B int32 }{x, y, x + wd, y + ht}
	procSetTextColorP.Call(hdc, uintptr(color))
	procDrawTextWP.Call(hdc, uintptr(unsafe.Pointer(s)), 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&rc)),
		0x00008024) // DT_SINGLELINE|DT_VCENTER|DT_END_ELLIPSIS [S3-15]
	//   修正：原 0x5024 = DT_PATH_ELLIPSIS(0x4000)|DT_RTLREADING(0x1000)|SINGLELINE|VCENTER
	//   —— 0x1000(DT_RTLREADING) 是笔误，0x4000(DT_PATH_ELLIPSIS) 只对"路径"生效，
	//   文件名这种非路径文本不会出省略号。改用 DT_END_ELLIPSIS(0x8000) 才是通用的尾省略号。
}

var (
	popupFontOnce sync.Once
	popupFont     uintptr
)

func getPopupFont() uintptr {
	popupFontOnce.Do(func() {
		face := windows.StringToUTF16Ptr("Segoe UI")
		// DEFAULT_GUI_FONT 在部分机器上损坏（实测文本高度=1 不可见）→ 显式建 Segoe UI 18px
		f, _, _ := procCreateFontWP.Call(
			uintptr(0xFFFFffee), // 高度 -18（有符号）
			0, 0, 0, 400, 0, 0, 0,
			1,   // DEFAULT_CHARSET
			4,   // OUT_TT_PRECIS
			0,   // CLIP_DEFAULT_PRECIS
			4,   // CLEARTYPE_QUALITY
			0x22, // DEFAULT_PITCH|FF_DONTCARE
			uintptr(unsafe.Pointer(face)))
		popupFont = f
	})
	return popupFont
}

func popupWndProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTimer, wmClose, wmLButtonDown:
		// 定时器到期 / 用户点击 / 关闭 → 销毁弹窗 + 退出消息循环
		procDestroyWindowP.Call(uintptr(hwnd))
		procPostQuitMessageP.Call(0)
		return 0
	case wmPaint: // 自绘三行文字（深底：标题白、正文灰）
		// PAINTSTRUCT 在 x64 是 80 字节（原写 72 → BeginPaint 越界写 8 字节，踩栈）
		var ps [80]byte
		hdc, _, _ := procBeginPaintP.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps[0])))
		if hdc != 0 {
			procSetBkModeP.Call(hdc, 1) // TRANSPARENT（1=TRANSPARENT，2=OPAQUE 白底）
			procSelectObjectP.Call(hdc, getPopupFont())
			w := int32(440)
			tx := int32(12)
			if popupThumb.has {
				// 左侧缩略图：高 76 适配，保持宽高比，超宽截到 76
				th := int32(80)
				tw := th * int32(popupThumb.w) / int32(popupThumb.h)
				if tw > 76 {
					tw = 76
				}
				StretchDIBitsTopDown(hdc, 6, 9, tw, th, popupThumb.dib, popupThumb.w, popupThumb.h)
				tx = 88
			}
			// 文字：左对齐 + 省略号（DT_CENTER 与省略号组合不可靠 [实测]）
			textW := w - tx - 10
			drawTextLine(hdc, popupText.title, tx, 9, textW, 26, 0xFFFFFF)
			drawTextLine(hdc, popupText.l1, tx, 40, textW, 22, 0xD0D0D0)
			drawTextLine(hdc, popupText.l2, tx, 66, textW, 22, 0xB8B8B8)
			procEndPaintP.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps[0])))
		}
		return 0
	case 0x0002: // WM_DESTROY
		procPostQuitMessageP.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

func pumpMessages() {
	var msg [48]byte
	for {
		r, _, _ := procGetMessageWP.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessageP.Call(uintptr(unsafe.Pointer(&msg[0])))
		procDispatchMessageWP.Call(uintptr(unsafe.Pointer(&msg[0])))
	}
}

func createPopupWindow(title, line1, line2 string, dib []byte, imgW, imgH int) uintptr {
	w := int32(440)
	h := int32(96)
	// 工作区（扣除任务栏）右下角 [F-07]：SystemParametersInfo(SPI_GETWORKAREA)
	// 而非整屏尺寸——任务栏在左/顶部时也贴对角，且 DPI 感知后坐标不再被虚拟化
	var wa struct{ L, T, R, B int32 }
	procSysParamsInfoW.Call(0x0030, 0, uintptr(unsafe.Pointer(&wa)), 0) // SPI_GETWORKAREA
	x := wa.R - w - 16
	y := wa.B - h - 12

	clsName, _ := windows.UTF16PtrFromString("PhotoBeamPopup")
	tw, _ := windows.UTF16PtrFromString(title)
	w1, _ := windows.UTF16PtrFromString(line1)
	w2, _ := windows.UTF16PtrFromString(line2)
	// 自绘文字由 WM_PAINT 使用（同一时刻最多一个弹窗在显示）
	popupText.title, popupText.l1, popupText.l2 = tw, w1, w2
	// 缩略图状态与文字同在 popupMu 内设置 → WM_PAINT 读到的一定是本弹窗自己的一份
	if len(dib) > 40 && imgW > 0 && imgH > 0 {
		popupThumb.dib = dib
		popupThumb.w, popupThumb.h = imgW, imgH
		popupThumb.has = true
		logf("弹窗: 缩略图已挂载 %dx%d dib=%dB", imgW, imgH, len(dib))
	} else {
		popupThumb.dib = nil
		popupThumb.w, popupThumb.h = 0, 0
		popupThumb.has = false
		logf("弹窗: 无缩略图 len(dib)=%d %dx%d", len(dib), imgW, imgH)
	}
	// WS_POPUP | WS_VISIBLE；EX: NOACTIVATE|TOPMOST|TOOLWINDOW
	const style = wsPopup | wsVisible
	const exStyle = wsexNoActivate | wsexTopmost | wsexToolWindow

	hInst, _, _ := procGetModuleHandleWP.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(clsName)),
		0,
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, hInst, 0)
	if hwnd == 0 {
		return 0
	}
	return hwnd
}


// StretchDIBitsTopDown：把 32bpp bottom-up DIB 缩放到目标矩形
//
// [修复 2026-11-02] 原实现用 StretchDIBits，实测**一个像素都画不出来**（返回 0 扫描行），
// 缩略图区域始终是纯背景色。独立对照实验（pc/dibtest3/main.go，结论可复现）：
//   - StretchDIBits 对本项目这种"大尺寸 bottom-up 32bpp DIB"在 dest=45/64/200/450 下
//     全部返回 0；而同样尺寸的合成 DIB 有时又能成功 —— 该 API 在此场景下行为不可靠。
//   - 改走 GDI 稳妥路径后稳定成功：CreateDIBSection 建源 DIB 段 → 内存 DC → StretchBlt，
//     实测输出方向与内容均正确（参考 pc/dibtest3_stretchblt.png）。
//
// 另：头信息用**本地 40 字节副本**，绝不碰调用方的 DIB（剪贴板写入路径与弹窗共用同一份 DIB，
// 原地改 biHeight 会造成数据竞争并把源数据改坏）。
func StretchDIBitsTopDown(hdc uintptr, x, y, dw, dh int32, dib []byte, srcW, srcH int) {
	if len(dib) < 40 || srcW <= 0 || srcH <= 0 {
		logf("弹窗缩略图: 参数无效 len(dib)=%d src=%dx%d", len(dib), srcW, srcH)
		return
	}
	var hdr [40]byte // 本地 BITMAPINFOHEADER 副本（biHeight>0 = bottom-up，与像素布局一致）
	copy(hdr[:], dib[:40])

	memDC, _, _ := procCreateCompatibleDC.Call(0)
	if memDC == 0 {
		logf("弹窗缩略图: CreateCompatibleDC 失败")
		return
	}
	defer procDeleteDC.Call(memDC)
	var bits uintptr
	sec, _, _ := procCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&hdr[0])),
		0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if sec == 0 {
		logf("弹窗缩略图: CreateDIBSection 失败")
		return
	}
	defer procDeleteObject.Call(sec)
	// 把像素搬进 DIB 段（bottom-up 逐字节直拷）
	copy(unsafe.Slice((*byte)(unsafe.Pointer(bits)), len(dib)-40), dib[40:])
	old, _, _ := procSelectObjectP.Call(memDC, sec)
	defer procSelectObjectP.Call(memDC, old)

	r, _, _ := procStretchBlt.Call(hdc,
		uintptr(uint32(x)), uintptr(uint32(y)), uintptr(uint32(dw)), uintptr(uint32(dh)),
		memDC, 0, 0, uintptr(uint32(srcW)), uintptr(uint32(srcH)),
		0x00CC0020) // SRCCOPY
	logf("弹窗缩略图: StretchBlt dst=(%d,%d,%dx%d) src=(0,0,%dx%d) 返回=%d",
		x, y, dw, dh, srcW, srcH, r)
}
