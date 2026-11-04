package main

// 配对向导窗（PC-1 / PC-2）—— 协议 §3.2、§8、验收 §9.1
//
// 背景：协议要求「两端同时展示 SAS」，此前 PC 端只写一行日志（server.go 的
// "配对模式 SAS=…"），用户看不到那 6 位数字 → 手机端弹的「请在电脑上核对数字」
// 对话框无法完成。本文件实现 Receiver 端向导窗。
//
// 外观原则（Stage 5 用户反馈「不是一个正常样式的窗口 / 为什么不使用 windows
// 系统提供的窗口控件」）：
//
//	**整窗就是一个普通的 Windows 对话框**——系统标题栏、系统对话框底色
//	(COLOR_BTNFACE)、系统字体(Segoe UI 9pt)、以及系统控件
//	(STATIC / BUTTON / EDIT)。
//	所有文字都放在 STATIC 控件里，父窗口**不画任何东西**，
//	所以不会出现「深色自绘背景 + 浅色系统控件」的割裂感
//	（第一版就是这样：系统按钮是浅灰的，底下是深灰自绘背景，看着像坏掉了）。
//
//	两个曾经的弯路，结论记录在此避免重犯：
//	① **WM_CTLCOLORBTN 对按钮无效**：Windows 10/11 的 BUTTON 是**主题渲染**的
//	   （uxtheme），父窗口给的画刷和文字色会被忽略——实测消息确实到了、
//	   颜色也确实设了，渲染出来仍是系统默认灰。所以「给按钮改色」这条路走不通，
//	   只能接受系统默认外观，或者整体改用自绘。考虑到用户明确要求用系统控件，
//	   这里选择**接受系统默认外观**。
//	② 本程序没有嵌入 Windows 应用清单（manifest）→ ComCtl32 v6 未激活。
//	   这台机器上没有 Windows SDK / MinGW，无法编译 .syso 资源，
//	   所以「嵌 manifest 换 v6 控件」这条路暂时走不通（见 docs/stage5-evidence.md 遗留）。
//	   折中做法：显式用 WM_SETFONT 给所有控件设 Segoe UI，
//	   避免没有 manifest 时默认的位图字体（MS Shell Dlg）在 125%/150% 缩放下发虚。
//
// 关键约束：向导期间必须可交互 → **不能加 WS_EX_NOACTIVATE**（那样按钮点不了）；
// 用 WS_EX_TOOLWINDOW（不进任务栏）+ TOPMOST（置顶）。
// 零 CGO：只用 syscall / x/sys/windows，不引入新依赖。

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------- Win32 入口 ----------

var (
	procSendMessageW        = user32CB.NewProc("SendMessageW")
	procSetWindowTextW      = user32CB.NewProc("SetWindowTextW")
	procGetWindowTextW      = user32CB.NewProc("GetWindowTextW")
	procGetDlgItemW         = user32CB.NewProc("GetDlgItem")
	procGetDlgCtrlID        = user32CB.NewProc("GetDlgCtrlID")
	procSetForegroundWindow = user32CB.NewProc("SetForegroundWindow")
	procShowWindowP         = user32CB.NewProc("ShowWindow")
	procAdjustWindowRectEx  = user32CB.NewProc("AdjustWindowRectEx")
	procGetSysColorBrush    = user32CB.NewProc("GetSysColorBrush")
	procGetSysColor         = user32CB.NewProc("GetSysColor")
	procGetDC               = user32CB.NewProc("GetDC")
	procReleaseDC           = user32CB.NewProc("ReleaseDC")
	procGetDeviceCaps       = gdi32CB.NewProc("GetDeviceCaps")

	procSetBkColorP = gdi32CB.NewProc("SetBkColor")

	ole32                   = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
)

// 向导窗客户区尺寸与子控件 ID
const (
	pairWinW = 520 // 客户区宽
	pairWinH = 368 // 客户区高

	IDC_PAIR_MATCH    = 1001
	IDC_PAIR_MISMATCH = 1002
	IDC_PAIR_DIREDIT  = 1003
	IDC_PAIR_BROWSE   = 1004
	IDC_PAIR_USEDIR   = 1005

	// STATIC 文本控件（用 ID 区分配色）
	IDC_PAIR_T_FPLBL  = 1010
	IDC_PAIR_T_FPVAL  = 1011
	IDC_PAIR_T_DEVLBL = 1012
	IDC_PAIR_T_DEVVAL = 1013
	IDC_PAIR_T_ASK    = 1014
	IDC_PAIR_T_SAS    = 1015
	IDC_PAIR_T_ACT    = 1016
	IDC_PAIR_T_DIR    = 1017
	IDC_PAIR_T_TIP    = 1018
)

const (
	// pairTimeout：配对向导窗的等待上限。
	// [Stage 5 修订] 原为 15 秒——人眼核对 6 位数字再点按钮，15 秒根本不够
	//（实测首次验收时手机端因等待超时直接发出了 pair_reject）。
	// 现统一为 60 秒，**两端必须一致**：手机端 SAS 对话框的 latch 等待同样是 60 秒。
	pairTimeout   = 60 * time.Second
	pairTimeoutMs = int(pairTimeout / time.Millisecond)

	// pairReplaceWait：新配对向导窗开出来之前，等旧窗口真正销毁的上限。
	// 正常情况几毫秒就结束；超时的唯一可能是旧窗口正卡在模态框里（文件夹选择器），
	// 那时也不能无限等——最多让屏幕上短暂并存两个窗，也不能卡住新会话。
	pairReplaceWait = 2 * time.Second

	wsChildVis    = 0x40000000 | 0x10000000
	wsTabStop     = 0x00010000
	wsBorder      = 0x00800000
	esAutoHScroll = 0x0080

	// COM 初始化
	coinitApartmentThreaded = 0x2 // COINIT_APARTMENTTHREADED
	coinitSFalse            = 1   // S_FALSE：线程已初始化过，仍需配一次 CoUninitialize

	// 普通窗口样式（带标题栏）
	wsOverlapped  = 0x00000000
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000

	// STATIC 样式
	ssLeft       = 0x0000
	ssCenter     = 0x0001
	ssCenterImg  = 0x0200
	ssEndEllipsis = 0xC000

	// 消息
	wmSetFont = 0x0030

	// 系统色 / 系统指标
	colorBtnFace   = 15
	logPixelsY     = 88
	spiGetFontSmoothed = 0x0AFA
)

// ---------- 会话状态 ----------

type pairEvent int

const (
	pairEvMatch pairEvent = iota // 用户点『一致』
	pairEvMismatch               // 用户点『不一致』→ 发 pair_reject
	pairEvTimeout                // 超时
)

// pairingSession：一次配对向导窗的生命周期
type pairingSession struct {
	mu      sync.Mutex
	hwnd    uintptr
	sas     string
	fp      string
	devName string
	events  chan pairEvent
	closed  bool
	// closeReq：已请求关闭。**用于修「关闭请求早于窗口创建」的竞态**：
	// 旧实现里 close() 读到的 p.hwnd 可能还是 0（窗口已可见但句柄还没写回），
	// 于是 WM_CLOSE 根本没发出去 → 旧窗口永久留在屏幕上（实测一次弹两个配对窗）。
	closeReq bool
	// gone：窗口**彻底销毁**后关闭（单实例等待用）。
	// 保证新向导窗开出来之前，旧窗口已经从屏幕上消失。
	gone chan struct{}
}

var (
	pairWinMu  sync.Mutex
	pairWinMap = map[uintptr]*pairingSession{} // hwnd → 会话（WndProc 反查用）
	activePair *pairingSession
)

// post：投递用户动作（满了就丢——用户点两次也只算一次）
func (p *pairingSession) post(ev pairEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	select {
	case p.events <- ev:
	default:
	}
}

// close：关闭向导窗（跨线程安全：只能 PostMessage）
// 同时关闭 events —— 等待用户动作的 goroutine 会立刻退出（不泄漏）
func (p *pairingSession) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	// [Stage 5 修复] 置 closeReq：即使此刻 p.hwnd 还是 0（窗口已建但句柄还没写回），
	// 创建 goroutine 也会在写回句柄后立刻把窗口销毁掉 —— 杜绝「关了没关掉」的残窗。
	p.closeReq = true
	h := p.hwnd
	close(p.events)
	p.mu.Unlock()
	if h != 0 {
		procPostMessageW.Call(h, wmClose, 0, 0)
	}
}

func pairSessionOf(hwnd uintptr) *pairingSession {
	pairWinMu.Lock()
	defer pairWinMu.Unlock()
	return pairWinMap[hwnd]
}

func setActivePair(p *pairingSession) *pairingSession {
	pairWinMu.Lock()
	defer pairWinMu.Unlock()
	old := activePair
	activePair = p
	return old
}

func clearActivePair(p *pairingSession) {
	pairWinMu.Lock()
	defer pairWinMu.Unlock()
	if activePair == p {
		activePair = nil
	}
}

// showPairingWizard：弹出配对向导窗
//
// [Stage 5 修复 · 真正的单实例]
//   旧实现的「同一时刻只允许一个」是假的：它只把旧会话标记为关闭（PostMessage WM_CLOSE），
//   **不保证旧窗口真的消失**，于是屏幕上会同时挂着两个配对确认窗（实测复现）。
//   现在：① 关闭请求先于窗口创建到达时，创建端立即销毁（closeReq）；
//         ② 新窗口开出来之前**等旧窗口确实销毁**（gone 通道），保证任意时刻至多一个可见向导窗。
//
// 返回会话对象：调用方用 close() 主动关窗，用 events 接收用户动作
func showPairingWizard(sas, fp, devName string) *pairingSession {
	p := &pairingSession{
		sas:     sas,
		fp:      fp,
		devName: devName,
		events:  make(chan pairEvent, 4),
		gone:    make(chan struct{}),
	}
	if old := setActivePair(p); old != nil {
		old.close() // 旧向导窗先关（PC-1：同一时刻只允许一个向导窗）
		// 等旧窗口彻底销毁再开新的 —— 否则屏幕上会同时出现两个配对确认窗。
		select {
		case <-old.gone:
		case <-time.After(pairReplaceWait):
			logWarn("旧配对向导窗未在 %v 内销毁，仍继续开新窗", pairReplaceWait)
		}
	}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		// 无论走哪条退出路径，都要告诉「等着开新窗的那一方」：这个窗口已经没了。
		defer close(p.gone)
		registerPairingClassOnce()
		hwnd := createPairingWindow(p)
		if hwnd == 0 {
			logErr("配对向导窗创建失败")
			clearActivePair(p)
			p.close()
			return
		}
		p.mu.Lock()
		p.hwnd = hwnd
		needClose := p.closeReq
		p.mu.Unlock()
		pairWinMu.Lock()
		pairWinMap[hwnd] = p
		pairWinMu.Unlock()
		if needClose {
			// 关闭请求先于窗口创建到达 → 立即销毁，绝不留下残窗
			procDestroyWindowP.Call(hwnd)
			procPostQuitMessageP.Call(0)
			return
		}
		// 超时定时器（WM_TIMER → pairEvTimeout）
		procSetTimerP.Call(hwnd, 1, uintptr(pairTimeoutMs), 0)
		procShowWindowP.Call(hwnd, 5) // SW_SHOW
		procSetForegroundWindow.Call(hwnd)
		pumpMessages()
		// 窗口已销毁：摘掉映射，避免泄漏
		pairWinMu.Lock()
		delete(pairWinMap, hwnd)
		pairWinMu.Unlock()
		p.mu.Lock()
		p.hwnd = 0
		p.mu.Unlock()
		clearActivePair(p)
	}()
	return p
}

// ---------- 窗口类与字体 ----------

var (
	pairClassOnce sync.Once
	pairFontOnce  sync.Once
	pairFontNorm  uintptr
	pairFontMono  uintptr
	pairFontBig   uintptr
)

func registerPairingClassOnce() {
	pairClassOnce.Do(func() {
		clsName, _ := windows.UTF16PtrFromString("PhotoBeamPairing")
		hInst, _, _ := procGetModuleHandleWP.Call(0)
		wc := wndClassEx{
			CbSize: uint32(unsafe.Sizeof(wndClassEx{})),
			// 系统对话框底色（COLOR_BTNFACE）——和系统控件是同一套配色
			Background: windows.Handle(sysColorBrush(colorBtnFace)),
			ClassName:  clsName,
		}
		wc.WndProc = windows.NewCallback(pairingWndProc)
		wc.Instance = windows.Handle(hInst)
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	})
}

// mulDiv：Windows 的 MulDiv（四舍五入）
func mulDiv(a, b, c int32) int32 {
	if c == 0 {
		return 0
	}
	return (a*b + c/2) / c
}

// colorRef：RGB → COLORREF（Win32 是 0x00BBGGRR，顺序与直觉相反）
func colorRef(r, g, b uint32) uint32 { return r | g<<8 | b<<16 }

// sysColorBrush：取系统颜色画刷（系统对象，不能 DeleteObject）
func sysColorBrush(color int32) uintptr {
	b, _, _ := procGetSysColorBrush.Call(uintptr(color))
	return b
}

// sysColor：取系统颜色的 COLORREF 值（SetBkColor/SetTextColor 要的是这个，不是画刷句柄）
func sysColor(color int32) uint32 {
	c, _, _ := procGetSysColor.Call(uintptr(color))
	return uint32(c)
}

// systemDPI：当前系统 DPI（用于按缩放算字体像素高度）
func systemDPI() int32 {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 96
	}
	defer procReleaseDC.Call(0, hdc)
	dpi, _, _ := procGetDeviceCaps.Call(hdc, logPixelsY)
	if dpi <= 0 {
		return 96
	}
	return int32(dpi)
}

// getPairFonts：系统字体 Segoe UI（普通 9pt / 等宽 9pt / 大号粗体 30pt）
//
// 没有嵌入 manifest 时，默认 UI 字体是位图的 "MS Shell Dlg"，在 125%/150% 缩放下
// 会明显发虚 → 这里显式用 WM_SETFONT 给每个控件设 Segoe UI。
func getPairFonts() {
	pairFontOnce.Do(func() {
		dpi := systemDPI()
		mk := func(face string, pt int32, weight int32) uintptr {
			fp, _ := windows.UTF16PtrFromString(face)
			h := -mulDiv(pt, dpi, 72) // 负数 = 像素高度
			f, _, _ := procCreateFontWP.Call(
				uintptr(uint32(h)),
				0, 0, 0, uintptr(weight), 0, 0, 0,
				1,   // DEFAULT_CHARSET
				4,   // OUT_TT_PRECIS
				0,   // CLIP_DEFAULT_PRECIS
				4,   // CLEARTYPE_QUALITY
				0x22, // DEFAULT_PITCH|FF_DONTCARE
				uintptr(unsafe.Pointer(fp)))
			return f
		}
		pairFontNorm = mk("Segoe UI", 9, 400)
		pairFontMono = mk("Consolas", 9, 400)
		pairFontBig = mk("Segoe UI", 30, 700)
	})
}

// ---------- 窗口过程 ----------

func pairingWndProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTimer:
		// 超时：通知调用方 + 关窗
		if p := pairSessionOf(uintptr(hwnd)); p != nil {
			p.post(pairEvTimeout)
		}
		procDestroyWindowP.Call(uintptr(hwnd))
		procPostQuitMessageP.Call(0)
		return 0
	case wmClose:
		procDestroyWindowP.Call(uintptr(hwnd))
		procPostQuitMessageP.Call(0)
		return 0
	case 0x0002: // WM_DESTROY
		procPostQuitMessageP.Call(0)
		return 0
	case 0x0111: // WM_COMMAND
		switch wParam & 0xFFFF {
		case IDC_PAIR_MATCH:
			if p := pairSessionOf(uintptr(hwnd)); p != nil {
				p.post(pairEvMatch)
			}
			procDestroyWindowP.Call(uintptr(hwnd))
			procPostQuitMessageP.Call(0)
			return 0
		case IDC_PAIR_MISMATCH:
			if p := pairSessionOf(uintptr(hwnd)); p != nil {
				p.post(pairEvMismatch)
			}
			procDestroyWindowP.Call(uintptr(hwnd))
			procPostQuitMessageP.Call(0)
			return 0
		case IDC_PAIR_BROWSE:
			onBrowseDir(uintptr(hwnd))
			return 0
		case IDC_PAIR_USEDIR:
			onUseDir(uintptr(hwnd))
			return 0
		}
	case wmCtlColorStatic:
		// STATIC 配色：次要文字灰、SAS 深蓝强调，其余系统默认黑
		id, _, _ := procGetDlgCtrlID.Call(lParam)
		txt := uint32(0x000000)
		switch uint32(id) {
		case IDC_PAIR_T_FPLBL, IDC_PAIR_T_DEVLBL, IDC_PAIR_T_ACT, IDC_PAIR_T_TIP:
			txt = 0x606060
		case IDC_PAIR_T_SAS:
			txt = colorRef(0x0A, 0x5F, 0xC0)
		}
		procSetTextColorP.Call(wParam, uintptr(txt))
		procSetBkColorP.Call(wParam, uintptr(sysColor(colorBtnFace)))
		return sysColorBrush(colorBtnFace)
	case wmPaint:
		// 父窗口不画任何内容——所有可见文字都在 STATIC 控件里。
		// 这里只让系统用类背景刷刷一遍即可。
		r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
		return r
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

// ---------- 窗口创建 ----------

func createPairingWindow(p *pairingSession) uintptr {
	const style = wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox
	const exStyle = wsexTopmost | wsexToolWindow

	rc := struct{ L, T, R, B int32 }{0, 0, pairWinW, pairWinH}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&rc)), uintptr(style), 0, uintptr(exStyle))
	ow := rc.R - rc.L
	oh := rc.B - rc.T

	// 居中（工作区中心，避开任务栏）
	var wa struct{ L, T, R, B int32 }
	procSysParamsInfoW.Call(0x0030, 0, uintptr(unsafe.Pointer(&wa)), 0) // SPI_GETWORKAREA
	x := wa.L + (wa.R-wa.L-ow)/2
	y := wa.T + (wa.B-wa.T-oh)/2

	clsName, _ := windows.UTF16PtrFromString("PhotoBeamPairing")
	title, _ := windows.UTF16PtrFromString("PhotoBeam 配对确认")
	// 注意：**不加 WS_EX_NOACTIVATE** —— 向导期间要能点按钮/输路径
	hInst, _, _ := procGetModuleHandleWP.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(title)),
		// [Stage 5 修复] **不带 WS_VISIBLE**：窗口先以隐藏状态创建，
		// 等句柄写回会话、且确认没有「提前到达的关闭请求」之后才 ShowWindow。
		// 否则会出现「窗口已经可见、但会话还不知道它的句柄」的窗口期——
		// 那个窗口期里来的 close() 读到的 hwnd 是 0，旧窗口就永远关不掉了。
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(ow), uintptr(oh),
		0, 0, hInst, 0)
	if hwnd == 0 {
		return 0
	}

	getPairFonts()
	dev := p.devName
	if dev == "" {
		dev = "等待手机…"
	}

	// ---- 上半部分：设备信息 + 6 位码 ----
	createPairStatic(hwnd, "本机指纹", IDC_PAIR_T_FPLBL, 20, 12, 80, 20, pairFontNorm, ssLeft|ssCenterImg)
	createPairStatic(hwnd, p.fp, IDC_PAIR_T_FPVAL, 104, 12, 396, 20, pairFontMono, ssLeft|ssCenterImg)
	createPairStatic(hwnd, "手机设备", IDC_PAIR_T_DEVLBL, 20, 34, 80, 20, pairFontNorm, ssLeft|ssCenterImg)
	createPairStatic(hwnd, dev, IDC_PAIR_T_DEVVAL, 104, 34, 396, 20, pairFontNorm, ssLeft|ssCenterImg)

	createPairStatic(hwnd, "请核对手机屏幕上显示的数字是否与下面一致",
		IDC_PAIR_T_ASK, 20, 62, 480, 20, pairFontNorm, ssCenter|ssCenterImg)
	createPairStatic(hwnd, p.sas,
		IDC_PAIR_T_SAS, 20, 84, 480, 62, pairFontBig, ssCenter|ssCenterImg)
	createPairStatic(hwnd, "数字一致 → 点「一致」；数字不一致 → 点「不一致」",
		IDC_PAIR_T_ACT, 20, 150, 480, 20, pairFontNorm, ssCenter|ssCenterImg)

	// ---- 两个决策按钮（系统按钮，不指定默认按钮：避免误按回车就"一致"）----
	createPairButton(hwnd, "一 致", IDC_PAIR_MATCH, 104, 180, 150, 32, pairFontNorm)
	createPairButton(hwnd, "不 一 致", IDC_PAIR_MISMATCH, 266, 180, 150, 32, pairFontNorm)

	// ---- 下半部分：接收文件夹（PC-2）----
	createPairStatic(hwnd, "接收文件夹（改后立即生效）",
		IDC_PAIR_T_DIR, 20, 228, 480, 20, pairFontNorm, ssLeft|ssCenterImg)
	createPairEdit(hwnd, getSaveDir(), IDC_PAIR_DIREDIT, 20, 252, 370, 26, pairFontNorm)
	createPairButton(hwnd, "浏览…", IDC_PAIR_BROWSE, 398, 251, 98, 28, pairFontNorm)
	createPairButton(hwnd, "使用此路径", IDC_PAIR_USEDIR, 20, 286, 110, 26, pairFontNorm)

	// ---- 底部提示 ----
	createPairStatic(hwnd, fmt.Sprintf("%d 秒内未收到手机确认将自动断开", pairTimeoutMs/1000),
		IDC_PAIR_T_TIP, 20, 330, 480, 20, pairFontNorm, ssCenter|ssCenterImg)
	return hwnd
}

// createPairStatic：建一个系统 STATIC 文本控件并设字体
func createPairStatic(parent uintptr, text string, id int, x, y, w, h int, font uintptr, style uint32) uintptr {
	t, _ := windows.UTF16PtrFromString(text)
	cls, _ := windows.UTF16PtrFromString("STATIC")
	hInst, _, _ := procGetModuleHandleWP.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(t)),
		uintptr(wsChildVis|style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(id), hInst, 0)
	if hwnd != 0 && font != 0 {
		procSendMessageW.Call(hwnd, wmSetFont, font, 1)
	}
	return hwnd
}

// createPairButton：建一个系统 BUTTON 控件并设字体
func createPairButton(parent uintptr, text string, id int, x, y, w, h int, font uintptr) uintptr {
	t, _ := windows.UTF16PtrFromString(text)
	cls, _ := windows.UTF16PtrFromString("BUTTON")
	hInst, _, _ := procGetModuleHandleWP.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(t)),
		uintptr(wsChildVis|wsTabStop),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(id), hInst, 0)
	if hwnd != 0 && font != 0 {
		procSendMessageW.Call(hwnd, wmSetFont, font, 1)
	}
	return hwnd
}

// createPairEdit：建一个系统 EDIT 控件并设字体
func createPairEdit(parent uintptr, text string, id int, x, y, w, h int, font uintptr) uintptr {
	t, _ := windows.UTF16PtrFromString(text)
	cls, _ := windows.UTF16PtrFromString("EDIT")
	hInst, _, _ := procGetModuleHandleWP.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(t)),
		uintptr(wsChildVis|wsTabStop|wsBorder|esAutoHScroll),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(id), hInst, 0)
	if hwnd != 0 && font != 0 {
		procSendMessageW.Call(hwnd, wmSetFont, font, 1)
	}
	return hwnd
}

// ---------- 保存目录控件（PC-2） ----------

func getWindowText(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	buf := make([]uint16, 1024)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

func setWindowText(hwnd uintptr, s string) {
	if hwnd == 0 {
		return
	}
	t, _ := windows.UTF16PtrFromString(s)
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(t)))
}

// onBrowseDir：『浏览…』→ SHBrowseForFolderW → 立即写回配置
func onBrowseDir(parent uintptr) {
	dir := browseForFolder(parent, getSaveDir(), "选择图片接收文件夹")
	if dir == "" {
		return // 用户取消
	}
	if err := applySaveDir(dir); err != nil {
		messageBox("更改接收文件夹失败：\n"+err.Error(), "PhotoBeam")
		return
	}
	edit, _, _ := procGetDlgItemW.Call(parent, IDC_PAIR_DIREDIT)
	setWindowText(edit, getSaveDir())
	messageBox("接收文件夹已更改：\n"+getSaveDir(), "PhotoBeam")
}

// onUseDir：『使用此路径』→ 用文本框里的路径（SHBrowseForFolderW 不可用时的兜底）
func onUseDir(parent uintptr) {
	edit, _, _ := procGetDlgItemW.Call(parent, IDC_PAIR_DIREDIT)
	txt := strings.TrimSpace(getWindowText(edit))
	if txt == "" {
		messageBox("请先填写接收文件夹路径。", "PhotoBeam")
		return
	}
	if err := applySaveDir(txt); err != nil {
		messageBox("使用该路径失败：\n"+err.Error(), "PhotoBeam")
		return
	}
	setWindowText(edit, getSaveDir())
	messageBox("接收文件夹已更改：\n"+getSaveDir(), "PhotoBeam")
}

// ---------- 现代文件夹选择器（零 CGO） ----------

// [Stage 5 第五轮 · I] 换掉 SHBrowseForFolderW（Vista 之前的经典对话框）：
//
//	经典版的问题（实测）：目录树以「桌面」为根、**不预选**当前接收目录；
//	BIF_EDITBOX 在新样式下不渲染 → **没有路径输入框**；换到别的位置只能逐层展开目录树。
//
//	现在改用 GetOpenFileNameW + OFN_PICKFOLDERS：Vista+ 上它就是现代的「选择文件夹」对话框
//	（左侧命名空间树 + 右上搜索框 + 底部「文件夹:」路径输入框），lpstrInitialDir 会**预选**当前目录。
//	仍然是纯 Win32 API，零 CGO、零新增依赖。

var browseMu sync.Mutex

const (
	ofnExplore     = 0x00080000
	ofnPathMustExi = 0x00000800
	ofnFileMustExi = 0x00001000
	ofnNoChangeDir = 0x00000008
	ofnPickFolders = 0x00000020
)

// openFileNameW：OPENFILENAMEW 结构（GetOpenFileNameW 用）
type openFileNameW struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       uintptr
	lpstrCustomFilter uintptr
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         uintptr
	nMaxFile          uint32
	lpstrFileTitle    uintptr
	nMaxFileTitle     uint32
	lpstrInitialDir   uintptr
	lpstrTitle        uintptr
	Flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       uintptr
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    uintptr
	pvReserved        uintptr
	dwReserved        uint32
	FlagsEx           uint32
}

var procGetOpenFileNameW = windows.NewLazySystemDLL("comdlg32.dll").NewProc("GetOpenFileNameW")

func browseForFolder(owner uintptr, initial, title string) string {
	// 模态对话框 + COM（现代对话框内部是 IFileDialog，STA）→ 打开态必须绑在同一 OS 线程
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	browseMu.Lock()
	defer browseMu.Unlock()

	hr, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded)
	// S_OK(0) / S_FALSE(1) 都表示「本线程已被初始化」→ 必须配一次 CoUninitialize；
	// 负值（如 RPC_E_CHANGED_MODE）表示本线程已用别的模型初始化过 → 不能反初始化。
	needUninit := hr == 0 || hr == coinitSFalse
	if needUninit {
		defer procCoUninitialize.Call()
	} else {
		logWarn("CoInitializeEx 返回 0x%X（本线程 COM 模型已存在），继续", uint32(hr))
	}

	fileBuf := make([]uint16, 1024)
	titleBuf := make([]uint16, 260)
	filter, _ := windows.UTF16PtrFromString("文件夹")
	titlePtr, _ := windows.UTF16PtrFromString(title)
	var initPtr uintptr
	if initial != "" {
		if p, err := windows.UTF16PtrFromString(initial); err == nil {
			initPtr = uintptr(unsafe.Pointer(p))
		}
	}
	ofn := openFileNameW{
		lStructSize: uint32(unsafe.Sizeof(openFileNameW{})),
		// owner：向导窗是 WS_EX_TOPMOST；不设 owner 的话对话框会被压在向导窗**下面**
		// （用户完全看不见）。设了 owner → 对话框是向导窗的 owned 窗口 → 永远在主人之上。
		hwndOwner:       owner,
		lpstrFilter:     uintptr(unsafe.Pointer(filter)),
		lpstrFile:       uintptr(unsafe.Pointer(&fileBuf[0])),
		nMaxFile:        1024,
		lpstrFileTitle:  uintptr(unsafe.Pointer(&titleBuf[0])),
		nMaxFileTitle:   260,
		lpstrInitialDir: initPtr, // **预选**当前接收目录（经典版做不到）
		lpstrTitle:      uintptr(unsafe.Pointer(titlePtr)),
		// OFN_PICKFOLDERS：把「打开文件」变成「选择文件夹」；OFN_PATHMUSTEXIST：必须是已存在的目录
		Flags: ofnExplore | ofnPickFolders | ofnPathMustExi | ofnNoChangeDir,
	}
	ok, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if ok == 0 {
		// 返回 0 = 用户取消（或对话框没能打开）；该 API 不区分，措辞不武断
		logf("文件夹选择器未返回结果（用户取消，或对话框未能打开）")
		return ""
	}
	dir := windows.UTF16ToString(fileBuf)
	logf("文件夹选择器返回: %s", dir)
	return dir
}
