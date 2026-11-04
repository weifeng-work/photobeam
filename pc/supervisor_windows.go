package main

// PC 端自监护（崩溃后自动重启）[Stage 5 第五轮 · L]
//
// 背景：协议 §8 原要求「计划任务看门狗」，但**注册系统计划任务需要提权/用户单独授权**，
// 上一轮按决策「不做」。这里换成**不需要任何授权**的方案：
//
//	主进程启动时拉起一个**隐藏的监护进程**（同一个 exe，参数 --supervise）：
//	监护进程只做一件事 —— 盯着主进程；主进程异常消失（非用户主动退出）就把它拉起来。
//
//	关键设计（避免各种「双实例/抢端口/关不掉」的坑）：
//   · 监护进程**不绑定任何端口**、**不创建托盘图标**（它只是等一个进程句柄）；
//   · 监护进程用**命名互斥体**保证全机只有一个（第二次启动直接退出）；
//   · 主进程把**自己的 PID**写进 supervisor.json，监护进程据此判断「是崩了」还是「交接了」：
//       - 退出后文件里的 PID 变了 → 托盘「重启 PhotoBeam」已交接给新进程 → 监护新进程，不重启；
//       - PID 没变且 clean_exit 里记的是这个 PID → 用户主动退出 → 监护进程一起退出；
//       - PID 没变且没有标记 → 异常退出（崩溃/被杀）→ 重新拉起主进程。
//   · 用户从任务管理器强行结束主进程 → 会被拉回来（这就是看门狗的意义；
//     不想这样可以在托盘里关掉「崩溃后自动重启」，或用托盘「退出」正常关闭）。
//
// 仍然不做：注册系统计划任务（需要授权）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const (
	supervisorMutexName = `Local\PhotoBeam.Supervisor.Mutex`
	supervisorFlag      = "--supervise"
)

var (
	supervisorFile = filepath.Join(appDir(), "logs", "supervisor.json")
	cleanExitFile  = filepath.Join(appDir(), "logs", "clean_exit")
)

// writeSupervisorPID：把当前主进程 PID 写进监护交接文件
func writeSupervisorPID() {
	p := filepath.Join(appDir(), "logs", "supervisor.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// readSupervisorPID：读交接文件里的 PID（0 = 没有/无效）
func readSupervisorPID() int {
	b, err := os.ReadFile(supervisorFile)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// markCleanExit：标记「用户主动退出」→ 监护进程不再拉起
// 写入**退出的那个主进程的 PID**：监护进程只认自己守着的那一个 PID，
// 别的（上一轮遗留的）标记一律当过期忽略 —— 避免旧标记把「崩溃」误判成「主动退出」。
func markCleanExit() {
	p := filepath.Join(appDir(), "logs", "clean_exit")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func clearCleanExit() {
	_ = os.Remove(cleanExitFile)
}

// cleanExitFor：指定 PID 是否是「主动退出」
func cleanExitFor(pid int) bool {
	b, err := os.ReadFile(cleanExitFile)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && n == pid
}

// startSupervisor：拉起隐藏的监护进程（已有则跳过）
func startSupervisor() {
	if !supervisorEnabled() {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, supervisorFlag)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		logWarn("拉起监护进程失败: %v", err)
		return
	}
	logf("监护进程已启动（pid=%d）：主进程异常退出时自动拉起", cmd.Process.Pid)
}

// supervisorEnabled：托盘开关（崩溃后自动重启）
// 标记文件不存在 = **默认开启**；写了 "0" = 关闭。
// （注意：早期版本把这里写反了 —— 文件不存在时返回 false，导致监护进程根本不起，
//  CI 编译查不出来，只有实机跑才暴露。）
func supervisorEnabled() bool {
	b, err := os.ReadFile(filepath.Join(appDir(), "logs", "supervisor_off"))
	if err != nil {
		return true // 没有标记文件 → 开启
	}
	return strings.TrimSpace(string(b)) != "0"
}

// setSupervisorEnabled：切换托盘开关
func setSupervisorEnabled(on bool) {
	p := filepath.Join(appDir(), "logs", "supervisor_off")
	if on {
		_ = os.Remove(p)
	} else {
		_ = os.WriteFile(p, []byte("0"), 0o644)
	}
}

// runSupervisor：监护进程主体
// 用法：PhotoBeam.exe --supervise
func runSupervisor() {
	// 全机只允许一个监护进程（命名互斥体）
	h, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(supervisorMutexName))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		return // 已经有一个监护进程了
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}
	cur := readSupervisorPID()
	logf("监护进程启动，盯住主进程 pid=%d", cur)
	for {
		// ① 等当前主进程结束
		for cur > 0 && processAlive(cur) {
			time.Sleep(500 * time.Millisecond)
		}
		// ② 判断是「托盘重启交接」还是「异常退出」
		next := readSupervisorPID()
		if next > 0 && next != cur && processAlive(next) {
			// 托盘「重启 PhotoBeam」已交接给新进程 → 继续监护它，不重启
			cur = next
			continue
		}
		if cleanExitFor(cur) {
			// 用户主动退出（托盘「退出」）→ 监护进程一起退出
			clearCleanExit()
			return
		}
		// ③ 异常退出 → 重新拉起主进程
		cmd := exec.Command(exe)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		if err := cmd.Start(); err != nil {
			return
		}
		cur = cmd.Process.Pid
		_ = os.WriteFile(supervisorFile, []byte(strconv.Itoa(cur)), 0o644)
	}
}

// processAlive：进程是否还活着
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	st, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return st == uint32(windows.WAIT_TIMEOUT) // 还没结束 = 还活着
}
