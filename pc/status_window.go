package main

// 状态面板（原生 Win32 独立窗口）
//
// 为什么用「一个只读多行 EDIT + 三个按钮」而不是逐控件排版：
//   · 面板内容是**多段表格**（设备 / 目录 / 最近活动），逐行 STATIC 要几十个控件，
//     设备增减还得动态增删；一个 EDIT 天然支持滚动、选中复制，代码量小一个数量级。
//   · 字体用「新宋体」（NSimSun）—— 中英文都是等宽，ASCII 半角、汉字全角，
//     表格列能对齐（Segoe UI 下面这种混排对不齐）。
//   · 不碰剪贴板：本程序自己的逻辑不写剪贴板；用户手动选中复制是用户行为。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	statusClass  = "PhotoBeamStatus"
	statusTitle  = "PhotoBeam 状态"
	statusWinW   = 880 // DIP，按 DPI 缩放
	statusWinH   = 680
	statusTimer  = 1
	statusMargin = 14
	statusBtnH   = 30
	statusBtnW   = 130

	// panelCells：正文表格的「格数」上限（1 格 = 半个汉字宽）。
	// 字体大小按「EDIT 客户区宽度 / panelCells」反算 —— 保证任何 DPI 下都不换行，
	// 这样列才能对齐（不需要凑固定的字号）。
	panelCells = 116

	idStatusText    = 3001
	idStatusOpen    = 3002
	idStatusDiag    = 3003
	idStatusRefresh = 3004

	// EDIT / 窗口样式
	esMultiline   = 0x0004
	esReadOnly    = 0x0800
	esAutoVScroll = 0x0040
	wsVScroll     = 0x00200000
	wsexClientEdge = 0x00000200

	// 消息
	wmCreate  = 0x0001
	wmCommand = 0x0111

	// EDIT 消息
	emGetFirstVisibleLine = 0x00CE
	emLineScroll          = 0x00B6
	emSetSel              = 0x00B1

	colorWindow = 5 // COLOR_WINDOW
	colorBtnText = 18 // COLOR_BTNTEXT
)

var (
	procKillTimerP   = user32CB.NewProc("KillTimer")
	procGetTextFaceW = gdi32CB.NewProc("GetTextFaceW")

	statusMu        sync.Mutex
	statusHwnd      uintptr
	statusCreating  bool
	statusLastText  string
	statusEditHwnd  uintptr
	statusFontBody  uintptr
	statusFontUI    uintptr
	statusFontOnce  sync.Once
	statusClassOnce sync.Once

	// standalonePanel：通过 --status-panel 独立开窗时，本进程不接连接、也不写统计，
	// 数据只能从 device_stats.json 读 → 每次刷新重新读盘，避免显示一份冻结的快照。
	standalonePanel bool

	// 防火墙状态缓存：底层要 exec netsh（几百毫秒 + 子进程开销），
	// 绝不能跟着 1 秒刷新跑 —— 30 秒一次足够（规则只在用户点「修复防火墙」时变）
	fwCacheMu  sync.Mutex
	fwCacheAt  time.Time
	fwCacheVal string

	// 目录扫描缓存（磁盘扫描 5 秒一次，够用且不拖累 1 秒刷新）
	dirCacheMu sync.Mutex
	dirCache   = map[string]dirCacheEntry{}
)

type dirCacheEntry struct {
	At   time.Time
	Rows []dirRow
}

type dirRow struct {
	Dir     string
	Count   int
	LastMod time.Time
}

// ---------- 对外入口 ----------

// showStatusWindow：托盘「状态面板…」调用。已开则前置，不重复开窗。
func showStatusWindow() {
	statusMu.Lock()
	h := statusHwnd
	busy := statusCreating
	statusMu.Unlock()
	if h != 0 {
		procShowWindowP.Call(h, 9) // SW_RESTORE
		procSetForegroundWindow.Call(h)
		return
	}
	if busy {
		return
	}
	statusMu.Lock()
	statusCreating = true
	statusMu.Unlock()

	go goroutineSafe("status-window", func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hwnd := createStatusWindow()
		statusMu.Lock()
		statusCreating = false
		statusMu.Unlock()
		if hwnd == 0 {
			messageBox("打开状态面板失败：窗口创建失败", "PhotoBeam")
			return
		}
		statusMu.Lock()
		statusHwnd = hwnd
		statusLastText = ""
		statusMu.Unlock()

		procShowWindowP.Call(hwnd, 5) // SW_SHOW
		procSetForegroundWindow.Call(hwnd)
		pumpMessages()

		statusMu.Lock()
		statusHwnd = 0
		statusEditHwnd = 0
		statusMu.Unlock()
	})
}

// ---------- 窗口类 / 字体 ----------

func registerStatusClassOnce() {
	statusClassOnce.Do(func() {
		clsName, _ := windows.UTF16PtrFromString(statusClass)
		hInst, _, _ := procGetModuleHandleWP.Call(0)
		wc := wndClassEx{
			CbSize:     uint32(unsafe.Sizeof(wndClassEx{})),
			Background: windows.Handle(sysColorBrush(colorBtnFace)),
			ClassName:  clsName,
		}
		wc.WndProc = windows.NewCallback(statusWndProc)
		wc.Instance = windows.Handle(hInst)
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	})
}

func getStatusFonts() {
	statusFontOnce.Do(func() {
		dpi := systemDPI()
		mk := func(face string, pt int32, weight int32) uintptr {
			fp, _ := windows.UTF16PtrFromString(face)
			h := -mulDiv(pt, dpi, 72)
			f, _, _ := procCreateFontWP.Call(
				uintptr(uint32(h)),
				0, 0, 0, uintptr(weight), 0, 0, 0,
				1,    // DEFAULT_CHARSET
				4,    // OUT_TT_PRECIS
				0,    // CLIP_DEFAULT_PRECIS
				4,    // CLEARTYPE_QUALITY
				0x22, // DEFAULT_PITCH|FF_DONTCARE
				uintptr(unsafe.Pointer(fp)))
			return f
		}
		// 正文用**等宽**字体：ASCII 半宽、汉字全宽（比例正好 1:2），表格才能对齐。
		// 为什么不是新宋体（NSimSun）：它在 13px 下的 TM 高度 = 字号，行距贴死，
		// 渲染出来上下行会视觉重叠；Cascadia Mono 12px 的行高是 16px，留白正常，
		// 且缺字时由系统回退到雅黑/Songti，CJK 宽度仍是 ASCII 的 2 倍。
		// 缺字体检测见 createBodyFont：选完用 GetTextFaceW 核对，被替换就降级。
		statusFontBody = createBodyFont(mulDiv(12, dpi, 96))
		statusFontUI = mk("Segoe UI", 9, 400)
	})
}

// scale：DIP → 物理像素
func scale(v int32) int32 {
	dpi := systemDPI()
	if dpi <= 0 {
		dpi = 96
	}
	return mulDiv(v, dpi, 96)
}

// ---------- 控件 ----------

func createStatusEdit(parent uintptr, x, y, w, h int32) uintptr {
	cls, _ := windows.UTF16PtrFromString("EDIT")
	hInst, _, _ := procGetModuleHandleWP.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		uintptr(wsexClientEdge),
		uintptr(unsafe.Pointer(cls)),
		0,
		uintptr(wsChildVis|wsVisible|wsVScroll|wsBorder|esMultiline|esReadOnly|esAutoVScroll),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(idStatusText), hInst, 0)
	if hwnd != 0 && statusFontBody != 0 {
		procSendMessageW.Call(hwnd, wmSetFont, statusFontBody, 1)
	}
	return hwnd
}

// createBodyFont：按优先级挑一个**真的存在**的等宽字体。
// CreateFontW 找不到字体时会静默替换成别的（不报错），所以选完必须用
// GetTextFaceW 回读实际字族名核对，不一致就降级到下一个候选。
func createBodyFont(px int32) uintptr {
	for _, face := range []string{"Cascadia Mono", "Consolas", "NSimSun"} {
		fp, _ := windows.UTF16PtrFromString(face)
		f, _, _ := procCreateFontWP.Call(
			uintptr(uint32(int32(-px))),
			0, 0, 0, 400, 0, 0, 0,
			1,    // DEFAULT_CHARSET
			4,    // OUT_TT_PRECIS
			0,    // CLIP_DEFAULT_PRECIS
			5,    // CLEARTYPE_QUALITY
			0x31, // FIXED_PITCH|FF_MODERN
			uintptr(unsafe.Pointer(fp)))
		if f == 0 {
			continue
		}
		if fontFaceMatches(f, face) {
			return f
		}
		procDeleteObject.Call(f)
	}
	return 0
}

func fontFaceMatches(f uintptr, want string) bool {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return false
	}
	defer procReleaseDC.Call(0, hdc)
	old, _, _ := procSelectObjectP.Call(hdc, f)
	defer procSelectObjectP.Call(hdc, old)
	var buf [64]uint16
	n, _, _ := procGetTextFaceW.Call(hdc, 64, uintptr(unsafe.Pointer(&buf[0])))
	if n == 0 {
		return false
	}
	got := windows.UTF16ToString(buf[:n-1])
	return strings.EqualFold(got, want)
}

// ---------- 窗口创建 ----------

func createStatusButton(parent uintptr, text string, id int, x, y, w, h int32) uintptr {
	t, _ := windows.UTF16PtrFromString(text)
	cls, _ := windows.UTF16PtrFromString("BUTTON")
	hInst, _, _ := procGetModuleHandleWP.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(t)),
		uintptr(wsChildVis|wsVisible|wsTabStop),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(id), hInst, 0)
	if hwnd != 0 && statusFontUI != 0 {
		procSendMessageW.Call(hwnd, wmSetFont, statusFontUI, 1)
	}
	return hwnd
}

func createStatusWindow() uintptr {
	registerStatusClassOnce()
	getStatusFonts()

	style := wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox
	rc := struct{ L, T, R, B int32 }{0, 0, scale(statusWinW), scale(statusWinH)}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&rc)), uintptr(style), 0, 0)
	ow, oh := rc.R-rc.L, rc.B-rc.T

	var wa struct{ L, T, R, B int32 }
	procSysParamsInfoW.Call(0x0030, 0, uintptr(unsafe.Pointer(&wa)), 0) // SPI_GETWORKAREA
	x := wa.L + (wa.R-wa.L-ow)/2
	y := wa.T + (wa.B-wa.T-oh)/2

	clsName, _ := windows.UTF16PtrFromString(statusClass)
	title, _ := windows.UTF16PtrFromString(statusTitle)
	hInst, _, _ := procGetModuleHandleWP.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(title)),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(ow), uintptr(oh),
		0, 0, hInst, 0)
	return hwnd
}

// ---------- 窗口过程 ----------

func statusWndProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmCreate:
		buildStatusControls(uintptr(hwnd))
		procSetTimerP.Call(uintptr(hwnd), statusTimer, 1000, 0)
		refreshStatusText(uintptr(hwnd), true)
		return 0
	case wmTimer:
		refreshStatusText(uintptr(hwnd), false)
		return 0
	case wmCommand:
		switch uint16(wParam & 0xFFFF) {
		case idStatusOpen:
			openFolder(getSaveDir())
		case idStatusDiag:
			go goroutineSafe("status-export-diag", func() {
				path, err := exportDiagnostics()
				if err != nil {
					messageBox("导出诊断信息失败：\n"+err.Error(), "PhotoBeam")
					return
				}
				messageBox("诊断信息已写入：\n"+path+"\n\n（未写入剪贴板）", "PhotoBeam 诊断信息")
			})
		case idStatusRefresh:
			refreshStatusText(uintptr(hwnd), true)
		}
		return 0
	case wmClose:
		procKillTimerP.Call(uintptr(hwnd), statusTimer)
		procDestroyWindowP.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		procKillTimerP.Call(uintptr(hwnd), statusTimer)
		procPostQuitMessageP.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

func buildStatusControls(hwnd uintptr) {
	m := scale(statusMargin)
	btnH := scale(statusBtnH)
	btnW := scale(statusBtnW)
	gap := scale(8)
	cw := scale(statusWinW)
	ch := scale(statusWinH)

	// 底部按钮行
	by := ch - m - btnH
	// 三个按钮右对齐
	x3 := cw - m - btnW
	x2 := x3 - gap - btnW
	x1 := x2 - gap - btnW
	createStatusButton(hwnd, "打开接收文件夹", idStatusOpen, x1, by, btnW, btnH)
	createStatusButton(hwnd, "导出诊断信息", idStatusDiag, x2, by, btnW, btnH)
	createStatusButton(hwnd, "刷新", idStatusRefresh, x3, by, btnW, btnH)

	// 正文
	statusMu.Lock()
	statusEditHwnd = createStatusEdit(hwnd, m, m, cw-2*m, by-m-gap)
	statusMu.Unlock()
}

// ---------- 文本 ----------

func refreshStatusText(hwnd uintptr, force bool) {
	if standalonePanel {
		loadDeviceStats() // 只看盘：独立窗口没有自己的连接与统计
	}
	statusMu.Lock()
	edit := statusEditHwnd
	last := statusLastText
	statusMu.Unlock()
	if edit == 0 {
		return
	}
	txt := buildStatusText()
	if txt == last && !force {
		return
	}
	// 保留滚动位置：先记住首个可见行，写完再滚回去
	first, _, _ := procSendMessageW.Call(edit, emGetFirstVisibleLine, 0, 0)

	t, _ := windows.UTF16PtrFromString(txt)
	procSetWindowTextW.Call(edit, uintptr(unsafe.Pointer(t)))
	procSendMessageW.Call(edit, emSetSel, 0, 0) // 不选中任何文本
	if first > 0 {
		procSendMessageW.Call(edit, emLineScroll, 0, first)
	}

	statusMu.Lock()
	statusLastText = txt
	statusMu.Unlock()
}

func buildStatusText() string {
	now := time.Now()
	devs := devicesSnapshot()
	stats, sess := statsSnapshot()

	var b strings.Builder
	line := func(s string) { b.WriteString(s); b.WriteString("\r\n") }
	// 分隔线用 ASCII "-"：CJK 制表符在回退字体里宽度不是 2 倍，"─" 会明显短一截
	rule := func() { line(strings.Repeat("-", panelCells)) }

	// ---- 标题 ----
	head := "PhotoBeam 状态"
	ver := appVersion
	if ver == "" {
		ver = "dev"
	}
	line(pad(head, panelCells-dispWidth("v"+ver)) + "v" + ver)
	rule()

	// ---- 服务 ----
	line("运行 " + humanDuration(now.Sub(procStart)))
	line(fmt.Sprintf("进程 %d · 监护 %s", os.Getpid(), supPidText()))
	line(fmt.Sprintf("监听 TCP :%d / UDP :%d · 指纹 %s", tcpPort(), udpPort(), identity.FP()))
	line(trimTo("保存目录 "+getSaveDir(), panelCells))
	line(trimTo("防火墙 "+firewallRuleStateCached(), panelCells))
	line(trimTo("开机自启 "+autostartRealState(), panelCells))
	line("崩溃自动重启 " + onOffText(supervisorEnabled()))
	if clipboardEnabled() {
		line(fmt.Sprintf("剪贴板 开 · %d 秒后还原", clipboardSeconds()))
	} else {
		line("剪贴板 关（图片只落盘，不写剪贴板）")
	}

	// ---- 设备 ----
	online := 0
	for _, d := range devs {
		if st, ok := stats[d.FP]; ok && now.Unix()-st.lastActive() <= onlineWindowSecs {
			online++
		}
	}
	b.WriteString("\r\n")
	line(pad(fmt.Sprintf("设备（%d 台已配对，%d 台在线）", len(devs), online), 42) +
		fmt.Sprintf("在线判据：%d 秒内连过（含 30 秒探测）", onlineWindowSecs))
	rule()
	if len(devs) == 0 {
		line("尚未配对任何手机。在托盘点「配对新设备」，然后在手机上点「② 配对电脑」。")
	} else {
		line(pad("设备", 30) + pad("状态", 18) + pad("本次", 6) + pad("累计", 6) + "上次连接")
		sorted := append([]Device(nil), devs...)
		sort.Slice(sorted, func(i, j int) bool {
			return stats[sorted[i].FP].lastActive() > stats[sorted[j].FP].lastActive()
		})
		for _, d := range sorted {
			st := stats[d.FP]
			label := fmt.Sprintf("%s (%s)", d.DevName, shortFp(d.FP))
			active := st.lastActive()
			state := "离线 · 从未连过"
			if active > 0 {
				if now.Unix()-active <= onlineWindowSecs {
					if st.LastFile > 0 && now.Unix()-st.LastFile <= onlineWindowSecs {
						state = "在线 · 正在同步"
					} else {
						state = "在线"
					}
				} else {
					state = "离线"
				}
			}
			last := "—"
			if active > 0 {
				last = time.Unix(active, 0).Format("01-02 15:04:05")
			}
			line(pad(trimTo(label, 29), 30) +
				pad(state, 18) +
				pad(fmt.Sprintf("%d", sess[d.FP]), 6) +
				pad(fmt.Sprintf("%d", st.Total), 6) +
				last)
		}
	}

	// ---- 目录（已收到过）----
	b.WriteString("\r\n")
	line("同步目录（只列「已经收到过图片」的目录 · 磁盘扫描 5 秒一次）")
	rule()
	if len(devs) == 0 {
		line("—")
	}
	for _, d := range devs {
		rows := scanDeviceDirs(filepath.Join(getSaveDir(), deviceDirName(d.DevName, d.FP)))
		nameLine := fmt.Sprintf("%s (%s)", d.DevName, shortFp(d.FP))
		if len(rows) == 0 {
			line(nameLine + "：还没有收到过图片")
			continue
		}
		line(nameLine)
		for _, r := range rows {
			line("  " + pad(trimTo(r.Dir, 40), 41) +
				pad(fmt.Sprintf("%d 个", r.Count), 9) +
				"最近 " + r.LastMod.Format("01-02 15:04"))
		}
	}

	// ---- 最近活动 ----
	b.WriteString("\r\n")
	line("最近活动（本次运行）")
	rule()
	acts := recentActivities()
	if len(acts) == 0 {
		line("（本次运行还没有收到图片）")
	} else {
		for i := len(acts) - 1; i >= 0; i-- {
			a := acts[i]
			line(a.At.Format("15:04:05") + "  " + pad(trimTo(a.Dev, 14), 15) + trimTo(a.File, 52))
		}
	}

	b.WriteString("\r\n")
	line("说明：手机上勾选了、但一张都还没传过的目录不会出现在上面的列表里 ——" +
		"协议目前不上报手机端的目录选择（留待 v1.3）。")
	return b.String()
}

// ---------- 数据辅助 ----------

// firewallRuleStateCached：30 秒 TTL 的防火墙状态（底层 exec netsh，不能每秒跑）
func firewallRuleStateCached() string {
	fwCacheMu.Lock()
	defer fwCacheMu.Unlock()
	if fwCacheVal != "" && time.Since(fwCacheAt) < 30*time.Second {
		return fwCacheVal
	}
	v := firewallRuleState()
	fwCacheVal, fwCacheAt = v, time.Now()
	return v
}

func supPidText() string {
	if p := readSupervisorPID(); p > 0 {
		return fmt.Sprintf("%d", p)
	}
	return "未运行"
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

// autostartRealState：读注册表的**实际**状态，而不是 config.json 里的意愿值
func autostartRealState() string {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return "未知（打不开注册表）"
	}
	defer k.Close()
	v, _, err := k.GetStringValue("PhotoBeam")
	want := autostartFlag()
	if err != nil || v == "" {
		if want {
			return "⚠ 已勾选但注册表无此项（下次登录不会自启，重新勾选一次即可修复）"
		}
		return "已关闭"
	}
	exe, _ := os.Executable()
	same := strings.EqualFold(v, exe)
	switch {
	case same && want:
		return "已启用（注册表一致）"
	case same && !want:
		return "已启用（⚠ 界面显示为关，建议重新勾选一次以对齐）"
	default:
		return "⚠ 注册表指向别的路径：" + v
	}
}

// scanDeviceDirs：扫一个设备目录下的一级子目录，统计文件数与最近修改时间（5 秒缓存）
func scanDeviceDirs(devDir string) []dirRow {
	dirCacheMu.Lock()
	if e, ok := dirCache[devDir]; ok && time.Since(e.At) < 5*time.Second {
		dirCacheMu.Unlock()
		return e.Rows
	}
	dirCacheMu.Unlock()

	rows := scanDeviceDirsUncached(devDir)

	dirCacheMu.Lock()
	dirCache[devDir] = dirCacheEntry{At: time.Now(), Rows: rows}
	dirCacheMu.Unlock()
	return rows
}

func scanDeviceDirsUncached(devDir string) []dirRow {
	entries, err := os.ReadDir(devDir)
	if err != nil {
		return nil
	}
	agg := map[string]*dirRow{}
	add := func(dir string, mod time.Time) {
		r := agg[dir]
		if r == nil {
			r = &dirRow{Dir: dir}
			agg[dir] = r
		}
		r.Count++
		if mod.After(r.LastMod) {
			r.LastMod = mod
		}
	}
	for _, e := range entries {
		full := filepath.Join(devDir, e.Name())
		if e.IsDir() {
			// 一级子目录：再往下简化合并（协议只在 PC 侧建一层子目录，但纵深防御多扫一层无妨）
			_ = filepath.WalkDir(full, func(p string, d os.DirEntry, err error) error {
				if err != nil || d == nil || d.IsDir() {
					return nil
				}
				info, ierr := d.Info()
				if ierr != nil {
					return nil
				}
				add(e.Name(), info.ModTime())
				return nil
			})
			continue
		}
		if info, ierr := e.Info(); ierr == nil {
			add("(根目录)", info.ModTime())
		}
	}
	out := make([]dirRow, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

// ---------- 文本宽度（中文=2 格） ----------

func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		if r < 128 {
			w++
		} else {
			w += 2
		}
	}
	return w
}

func pad(s string, width int) string {
	w := dispWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func trimTo(s string, width int) string {
	if dispWidth(s) <= width {
		return s
	}
	out := make([]rune, 0, len(s))
	w := 0
	for _, r := range s {
		rw := 1
		if r >= 128 {
			rw = 2
		}
		if w+rw > width-1 {
			break
		}
		out = append(out, r)
		w += rw
	}
	return string(out) + "…"
}
