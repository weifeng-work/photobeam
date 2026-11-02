package main

// PhotoBeam Receiver 入口：配置、身份、UDP 发现、TCP 监听、托盘

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	logf("PhotoBeam 启动 · 指纹 %s · 保存目录 %s", identity.FP(), cfg.SaveDir)

	// TCP 监听（5416 起探测空闲端口）
	if err := startTCPServer(); err != nil {
		trayWarn("TCP 端口 5416-5436 全部被占，无法启动。\n" + err.Error())
		os.Exit(1)
	}
	// UDP 发现（5416 固定，被占告警拒绝启动 — 协议 §0 [P0-11]）
	if err := startDiscoveryResponder(); err != nil {
		trayWarn("UDP 发现端口 5416 被占用，无法启动发现服务。\n" +
			"手机将无法通过广播找到本机。请释放 5416/udp 后重新启动 PhotoBeam。")
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

	mOpen := systray.AddMenuItem("打开接收文件夹", "打开 "+cfg.SaveDir)
	mRePair := systray.AddMenuItem("重新配对", "清除已配对设备，下次连接重新走配对流程")
	mDiag := systray.AddMenuItem("复制诊断信息", "生成诊断文本（M1 为弹窗展示）")
	mFW := systray.AddMenuItem("修复防火墙规则", "以管理员权限重建防火墙入站规则")
	mAuto := systray.AddMenuItemCheckbox("开机自启（登录时启动）", "写入当前用户的启动项", cfg.Autostart)
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出 PhotoBeam")

	syncAutostartCheck(mAuto)

	for {
		select {
		case <-mOpen.ClickedCh:
			openFolder(cfg.SaveDir)
		case <-mRePair.ClickedCh:
			clearDevices()
			messageBox("已清除全部配对记录。\n手机下次连接时将重新进入配对流程。", "重新配对")
		case <-mDiag.ClickedCh:
			// [Stage3] 一键复制诊断到剪贴板（比弹窗更易粘贴给支持方）
			go goroutineSafe("copy-diagnostics", func() {
				ok := setClipboardTextSimple(buildDiagnostics())
				if ok {
					showPopup("诊断已复制", "诊断信息已进入剪贴板", "可直接粘贴发送", 5000)
				} else {
					messageBox("复制失败（剪贴板被占用）。", "诊断信息")
				}
			})
		case <-mFW.ClickedCh:
			ok := addFirewallRulesElevated()
			if ok > 32 {
				messageBox("防火墙规则已重建。", "修复防火墙")
			} else {
				messageBox("重建失败或被取消。可手动以管理员身份运行：\n"+
					"netsh advfirewall firewall add rule name=\"PhotoBeam-TCP\" dir=in action=allow protocol=TCP localport=5416-5436\n"+
					"netsh advfirewall firewall add rule name=\"PhotoBeam-UDP\" dir=in action=allow protocol=UDP localport=5416", "修复防火墙")
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

func buildDiagnostics() string {
	var b strings.Builder
	fmt.Fprintf(&b, "PhotoBeam 诊断信息\r\n")
	fmt.Fprintf(&b, "协议版本: %d\r\n", ProtoVer)
	fmt.Fprintf(&b, "指纹: %s\r\n", identity.FP())
	fmt.Fprintf(&b, "TCP 监听: %d\r\nUDP 发现: %d\r\n", cfg.TCPPort, cfg.UDPPort)
	fmt.Fprintf(&b, "保存目录: %s\r\n", cfg.SaveDir)
	fmt.Fprintf(&b, "防火墙规则: %s\r\n", firewallRuleState())
	fmt.Fprintf(&b, "已配对设备: %d\r\n", len(cfg.Devices))
	for _, d := range cfg.Devices {
		fmt.Fprintf(&b, "  - %s (%s) 最近 %s:%d\r\n", d.DevName, d.FP, d.LastIP, d.LastPort)
	}
	fmt.Fprintf(&b, "\r\n日志尾部:\r\n")
	if lb, err := os.ReadFile(filepath.Join(appDir(), "logs", "pc.log")); err == nil {
		lines := strings.Split(strings.TrimRight(string(lb), "\r\n"), "\r\n")
		start := len(lines) - 20
		if start < 0 {
			start = 0
		}
		b.WriteString(strings.Join(lines[start:], "\r\n"))
	}
	return b.String()
}

func firewallRuleState() string {
	out, err := runCapture("netsh", "advfirewall", "firewall", "show", "rule", "name=PhotoBeam-TCP")
	if err != nil || !strings.Contains(out, "PhotoBeam-TCP") {
		return "未找到（入站可能被拦，请用托盘『修复防火墙规则』）"
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

func addFirewallRulesElevated() int {
	r1 := shellExecuteRunAs("netsh", `advfirewall firewall add rule name="PhotoBeam-TCP" dir=in action=allow protocol=TCP localport=5416-5436`)
	if r1 <= 32 {
		return r1
	}
	r2 := shellExecuteRunAs("netsh", `advfirewall firewall add rule name="PhotoBeam-UDP" dir=in action=allow protocol=UDP localport=5416`)
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
