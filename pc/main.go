package main

// PhotoBeam Receiver 入口：配置、身份、UDP 发现、TCP 监听、托盘

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows/registry"
)

func main() {
	defer func() {
		// 崩溃取证 [R-20]：panic 写退出码与最后 200 条日志
		if r := recover(); r != nil {
			logf("PANIC: %v", r)
			writeCrashDump(r)
			os.Exit(3)
		}
	}()

	// 隐藏自检入口：PhotoBeam.exe --test-clipboard <图片路径>
	// 直接跑 剪贴板管线（解码→DIB→写入→10 秒还原），结果打印到 stdout
	if len(os.Args) == 3 && os.Args[1] == "--test-clipboard" {
		cfg = loadConfig()
		testClipboard(os.Args[2])
		return
	}

	// DPI 感知 [F-07]：不声明则 GetSystemMetrics/弹窗坐标被虚拟化，缩放屏上模糊且右下角跑偏
	// PER_MONITOR_AWARE_V2 = -4；失败忽略（Win10 1703 前没有该 API）
	procDpiAware := user32.NewProc("SetProcessDpiAwarenessContext")
	procDpiAware.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4

	cfg = loadConfig()
	identity = loadOrCreateIdentity()
	loadSeenIDs()
	logf("PhotoBeam 启动 · 指纹 %s · 保存目录 %s", identity.FP(), getSaveDir())

	// TCP 监听（5416 起探测空闲端口）
	if err := startTCPServer(); err != nil {
		// [PC-3] 原来是 go messageBox(...) 紧接 os.Exit(1) —— 进程先退出，
		// 消息框大概率来不及显示，用户体感是「双击闪退/毫无反应」。
		// 现在：**同步**弹框（等用户点掉再退出）+ 补日志（此前该分支无日志）。
		logErr("TCP 端口 5416-5436 全部被占，无法启动: %v", err)
		messageBox("TCP 端口 5416-5436 全部被占用，无法启动。\n"+
			"请关闭占用这些端口的程序后重新启动 PhotoBeam。\n\n"+err.Error(),
			"PhotoBeam 无法启动")
		os.Exit(1)
	}
	// UDP 发现（5416 固定，被占告警拒绝启动 — 协议 §0 [P0-11]）
	if err := startDiscoveryResponder(); err != nil {
		// [PC-3] 同上：同步弹框 + 补日志（此前该分支连日志都没有）
		logErr("UDP 发现端口 5416 被占用，无法启动发现服务: %v", err)
		messageBox("UDP 发现端口 5416 被占用，无法启动发现服务。\n"+
			"手机将无法通过广播找到本机。\n请释放 5416/udp 后重新启动 PhotoBeam。\n\n"+err.Error(),
			"PhotoBeam 无法启动")
		os.Exit(1)
	}

	// 防火墙规则：首运行尝试以管理员权限预置两条规则（UAC 一次）[P1-11]
	// 注意：ShellExecute(runas) 会阻塞等待 UAC 应答 → 放到独立 goroutine，绝不阻塞监听启动
	go ensureFirewall()

	// 看门狗 [S3-12]：goroutine 数异常（>16，正常基线 5）才落栈取证——平时零输出
	go func() {
		for {
			time.Sleep(60 * time.Second)
			n := runtime.NumGoroutine()
			if n > 16 {
				buf := make([]byte, 1<<20)
				nw := runtime.Stack(buf, true)
				logf("goroutine 异常 %d：STACK DUMP 已写 stack.log", n)
				os.WriteFile(filepath.Join(appDir(), "logs", "stack.log"), buf[:nw], 0o644)
			}
		}
	}()

	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(trayIconIco) // [修复] 此前漏设图标 → 托盘显示空白
	systray.SetTooltip("PhotoBeam 接收器 · TCP " + fmt.Sprint(cfg.TCPPort) + " · 指纹 " + identity.FP())

	mOpen := systray.AddMenuItem("打开接收文件夹", "打开 "+getSaveDir())
	mSetDir := systray.AddMenuItem("接收文件夹…", "更改图片保存目录（立即生效）")
	mRePair := systray.AddMenuItem("重新配对", "清除已配对设备，下次连接重新走配对流程")
	mDiag := systray.AddMenuItem("导出诊断信息", "生成诊断文本并写入日志目录（不写入剪贴板）")
	mFW := systray.AddMenuItem("修复防火墙规则", "以管理员权限重建防火墙入站规则")
	mAuto := systray.AddMenuItemCheckbox("开机自启（登录时启动）", "写入当前用户的启动项", cfg.Autostart)
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出 PhotoBeam")

	syncAutostartCheck(mAuto)

	for {
		select {
		case <-mOpen.ClickedCh:
			openFolder(getSaveDir())
		case <-mSetDir.ClickedCh:
			// [PC-2] 托盘里也能改接收目录（不配对时也能改）
			go goroutineSafe("set-savedir", func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				// owner=0：托盘路径没有向导窗这种 TOPMOST 窗口压着，无主对话框照样能置前
				dir := browseForFolder(0, getSaveDir(), "选择图片接收文件夹")
				if dir == "" {
					return // 用户取消
				}
				if err := applySaveDir(dir); err != nil {
					messageBox("更改接收文件夹失败：\n"+err.Error(), "PhotoBeam")
					return
				}
				mOpen.SetTooltip("打开 " + getSaveDir()) // 菜单提示同步更新
				messageBox("接收文件夹已更改：\n"+getSaveDir(), "PhotoBeam")
			})
		case <-mRePair.ClickedCh:
			clearDevices()
			messageBox("已清除全部配对记录。\n手机下次连接时将重新进入配对流程。", "重新配对")
		case <-mDiag.ClickedCh:
			// [PC-5] 诊断信息**不再写入剪贴板**（用户已决定），改为写文件
			go goroutineSafe("export-diagnostics", func() {
				path, err := exportDiagnostics()
				if err != nil {
					messageBox("导出诊断信息失败：\n"+err.Error(), "PhotoBeam")
					return
				}
				// 路径可能很长 → 用会自动换行的消息框（弹窗宽度固定，容易被截断）
				messageBox("诊断信息已写入：\n"+path+"\n\n（未写入剪贴板）", "PhotoBeam 诊断信息")
			})
		case <-mFW.ClickedCh:
			ok := addFirewallRulesElevated()
			if ok > 32 {
				messageBox("防火墙规则已重建（按当前程序路径绑定，全部 profile 放行）。", "修复防火墙")
			} else {
				exe, _ := os.Executable()
				messageBox("重建失败或被取消。可手动以管理员身份运行：\n"+
					"netsh advfirewall firewall add rule name=\"PhotoBeam-TCP\" dir=in action=allow protocol=TCP localport=5416-5436 profile=any program=\""+exe+"\"\n"+
					"netsh advfirewall firewall add rule name=\"PhotoBeam-UDP\" dir=in action=allow protocol=UDP localport=5416 profile=any program=\""+exe+"\"", "修复防火墙")
			}
		case <-mAuto.ClickedCh:
			cfg.Autostart = !cfg.Autostart
			saveConfig(cfg)
			setAutostart(cfg.Autostart)
			syncAutostartCheck(mAuto)
		case <-mQuit.ClickedCh:
			systray.Quit()
		}
	}
}

func onExit() {}

func openFolder(dir string) {
	go func() {
		explorer := filepath.Join(os.Getenv("WINDIR"), "explorer.exe")
		_ = execOpen(explorer, dir)
	}()
}

// buildDiagnostics：诊断文本（协议 §8 字段 + 网络自检四件套 [PC-7]）
func buildDiagnostics() string {
	var b strings.Builder
	fmt.Fprintf(&b, "PhotoBeam 诊断信息\r\n")
	fmt.Fprintf(&b, "生成时间: %s\r\n", timeNow().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "协议版本: %d\r\n", ProtoVer)
	fmt.Fprintf(&b, "指纹: %s\r\n", identity.FP())
	fmt.Fprintf(&b, "TCP 监听: %d\r\nUDP 发现: %d\r\n", cfg.TCPPort, cfg.UDPPort)
	fmt.Fprintf(&b, "保存目录: %s\r\n", getSaveDir())
	fmt.Fprintf(&b, "防火墙规则: %s\r\n", firewallRuleState())
	fmt.Fprintf(&b, "已配对设备: %d\r\n", len(cfg.Devices))
	for _, d := range cfg.Devices {
		fmt.Fprintf(&b, "  - %s (%s) 最近 %s:%d\r\n", d.DevName, d.FP, d.LastIP, d.LastPort)
	}

	// ---- 网络自检四件套（协议 §8:289-292） ----
	fmt.Fprintf(&b, "\r\n---- 网络自检 ----\r\n")
	ips := netAdvertisedIPs()
	if len(ips) == 0 {
		fmt.Fprintf(&b, "对外通告 IP: 未找到（没有带 IPv4 的以太网/无线网卡）\r\n")
	} else {
		fmt.Fprintf(&b, "对外通告 IP（手机可达候选，含网卡名）:\r\n")
		for _, s := range ips {
			fmt.Fprintf(&b, "  - %s\r\n", s)
		}
	}
	gw := netDefaultGateway()
	if gw == "" {
		gw = "未知"
	}
	fmt.Fprintf(&b, "默认网关: %s\r\n", gw)
	fmt.Fprintf(&b, "网络 profile: %s\r\n", netProfile())
	fmt.Fprintf(&b, "防火墙规则路径比对: %s\r\n", firewallPathVerdict())
	fmt.Fprintf(&b, "认证失败统计: %s\r\n", authStatText())

	fmt.Fprintf(&b, "\r\n日志尾部:\r\n")
	if lb, err := os.ReadFile(filepath.Join(appDir(), "logs", "pc.log")); err == nil {
		lines := strings.Split(strings.TrimRight(string(lb), "\r\n"), "\r\n")
		start := len(lines) - 20
		if start < 0 {
			start = 0
		}
		b.WriteString(strings.Join(lines[start:], "\r\n"))
	} else {
		// 文件读不到（首次运行/权限）→ 用内存环形缓冲兜底
		for _, l := range logTail(20) {
			b.WriteString(l)
			b.WriteString("\r\n")
		}
	}
	return b.String()
}

// exportDiagnostics：诊断文本写入 logs\diagnostics-<时间戳>.txt（PC-5）
// **绝不写剪贴板**（用户已明确：诊断信息不再进剪贴板）
func exportDiagnostics() (string, error) {
	dir := filepath.Join(appDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "diagnostics-"+timeNow().Format("20060102-150405")+".txt")
	if err := os.WriteFile(path, []byte(buildDiagnostics()), 0o644); err != nil {
		return "", err
	}
	logf("诊断信息已导出: %s", path)
	return path, nil
}

// writeCrashDump：崩溃取证落盘（协议 §8:299）
// 内容 = 退出码 + panic 堆栈 + 最后 200 条日志
func writeCrashDump(r any) {
	dir := filepath.Join(appDir(), "logs")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "crash-"+timeNow().Format("20060102-150405")+".log")
	exe, _ := os.Executable()
	tail := logTail(logRingMax)
	var b strings.Builder
	fmt.Fprintf(&b, "PhotoBeam 崩溃报告\r\n")
	fmt.Fprintf(&b, "时间: %s\r\n", timeNow().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "退出码: 3\r\n")
	fmt.Fprintf(&b, "panic: %v\r\n", r)
	fmt.Fprintf(&b, "可执行文件: %s\r\n", exe)
	fmt.Fprintf(&b, "\r\n---- 堆栈 ----\r\n")
	b.Write(debug.Stack())
	fmt.Fprintf(&b, "\r\n---- 最后 %d 条日志 ----\r\n", len(tail))
	for _, l := range tail {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		logf("崩溃报告写入失败: %v", err)
		return
	}
	logf("崩溃报告已写入: %s", path)
}

func firewallRuleState() string {
	for _, rule := range []string{"PhotoBeam-TCP", "PhotoBeam-UDP"} {
		p, found := firewallRulePrograms(rule)
		if !found {
			return "未找到（入站可能被拦，请用托盘『修复防火墙规则』）"
		}
		if p == "" {
			return "存在（" + rule + "，但未按程序路径绑定，只按端口放行）"
		}
	}
	return "存在（PhotoBeam-TCP / UDP）"
}

func ensureFirewall() {
	if cfg.FWDone {
		return
	}
	if strings.Contains(firewallRuleState(), "未找到") {
		if r := addFirewallRulesElevated(); r > 32 {
			cfg.FWDone = true
			saveConfig(cfg)
			logf("防火墙规则已预置")
		} else {
			logf("防火墙规则预置被取消（UAC 拒绝），可稍后从托盘修复")
			trayWarn("防火墙规则未创建（UAC 被取消）。\n手机可能连不上本机。\n可稍后从托盘菜单『修复防火墙规则』重试。")
		}
	} else {
		cfg.FWDone = true
		saveConfig(cfg)
	}
}

// addFirewallRulesElevated：以管理员权限重建两条入站规则
// 规则按**程序路径**绑定（协议 §8：按程序路径绑定 + 绑定全部 profile）——
// 否则诊断里的「规则路径 vs 当前 exe」永远对不上（端口规则没有 Program 行）
func addFirewallRulesElevated() int {
	prog := ""
	if exe, err := os.Executable(); err == nil && exe != "" {
		prog = ` program="` + exe + `"`
	}
	r1 := shellExecuteRunAs("netsh", `advfirewall firewall add rule name="PhotoBeam-TCP" dir=in action=allow protocol=TCP localport=5416-5436 profile=any`+prog)
	if r1 <= 32 {
		return r1
	}
	r2 := shellExecuteRunAs("netsh", `advfirewall firewall add rule name="PhotoBeam-UDP" dir=in action=allow protocol=UDP localport=5416 profile=any`+prog)
	return r2
}

func setAutostart(on bool) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	if on {
		k.SetStringValue("PhotoBeam", exe)
	} else {
		k.DeleteValue("PhotoBeam")
	}
}

func syncAutostartCheck(m *systray.MenuItem) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	v, _, err := k.GetStringValue("PhotoBeam")
	if err == nil && strings.EqualFold(v, exe) {
		m.Check()
	} else {
		m.Uncheck()
	}
}

// ---------- 剪贴板管线自检（--test-clipboard） ----------

func testClipboard(path string) {
	dib, pngData, w, h, err := prepareImageData(path)
	if err != nil {
		logf("FAIL prepareImageData: %v", err)
		os.Exit(1)
	}
	logf("解码 OK: %dx%d dib=%dB png=%dB", w, h, len(dib), len(pngData))

	ok, snapNil := writeClipboardWithRestore(dib, pngData, 10, nil)
	logf("写入返回: %v（无主剪贴板不还原=%v）", ok, snapNil)

	// 读回验证：CF_DIB 尺寸 + 排除标记存在
	if r, _, _ := procOpenClipboard.Call(0); r != 0 {
		d, _, _ := procGetClipboardData.Call(cfDIB)
		if d == 0 {
			logf("FAIL: 读回 CF_DIB 为空")
		} else {
			got := globalRead(d)
			if len(got) != len(dib) {
				logf("FAIL: CF_DIB 读回 %dB != 写入 %dB", len(got), len(dib))
			} else {
				logf("CF_DIB 读回一致: %d bytes", len(got))
			}
		}
		for _, name := range []struct {
			f   uint16
			tag string
		}{{fmtCanHistory, "CanIncludeInClipboardHistory"}, {fmtCanUpload, "CanUploadToCloudClipboard"}} {
			d, _, _ := procGetClipboardData.Call(uintptr(name.f))
			if d == 0 {
				logf("FAIL: 缺排除标记 %s", name.tag)
			} else {
				logf("排除标记 OK: %s = %v", name.tag, globalRead(d))
			}
		}
		procCloseClipboard.Call()
	}
	seq1, _, _ := procGetSeqNumber.Call()
	logf("写入后序号: %d（60s/10s 窗口结束前不要动剪贴板）", seq1)

	time.Sleep(11 * time.Second)
	seq2, _, _ := procGetSeqNumber.Call()
	logf("窗口后序号: %d（还原会使序号变化或内容回到原状）", seq2)
	logf("自检完成")
}
