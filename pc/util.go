package main

// 小工具：base64、时间、随机数、日志、Win32 消息框

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func b64encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func b64decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func timeNow() time.Time { return time.Now() }

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func mustDecode(s string) []byte {
	b, err := b64decode(s)
	if err != nil {
		return nil
	}
	return b
}

func to32(b []byte) [32]byte {
	var out [32]byte
	copy(out[:], b)
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func cryptoRead(b []byte) (int, error) { return rand.Read(b) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func execOpen(app, args string) error {
	return exec.Command(app, args).Start()
}

func runCapture(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// ---------- 日志 ----------

// 后台 goroutine 的 panic 防护：写日志后仅终止当前 goroutine，不拖垮进程 [P2-08]
func goroutineSafe(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logf("PANIC in %s: %v", name, r)
		}
	}()
	fn()
}

// 日志分级（PC-9）：logf 保持既有签名与调用点不变（INFO），新增 logWarn / logErr
const (
	logLevelInfo  = "INFO"
	logLevelWarn  = "WARN"
	logLevelError = "ERROR"
)

const (
	logRingMax    = 200       // 内存环形缓冲条数（协议 §8：崩溃时带最后 200 条）
	logRotateSize = 5 << 20   // 单个日志文件超过 5MB 轮转
	logRotateKeep = 3         // 最多保留 3 份历史（pc.log.1 ~ pc.log.3）
)

var (
	logMu   sync.Mutex
	logRing []string // 内存环形缓冲（最近 200 条）
)

// appendLog：写文件 + 写内存环形缓冲（200 条）[PC-8]
func appendLog(level, line string) {
	logMu.Lock()
	defer logMu.Unlock()
	full := fmt.Sprintf("[%s] [%s] %s", timeNow().Format("2006-01-02 15:04:05"), level, line)
	logRing = append(logRing, full)
	if len(logRing) > logRingMax {
		logRing = append(logRing[:0], logRing[len(logRing)-logRingMax:]...)
	}
	p := filepath.Join(appDir(), "logs", "pc.log")
	if err := rotateLogIfNeeded(p); err != nil {
		fmt.Fprintln(os.Stderr, "日志轮转失败:", err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\r\n", full)
}

// rotateLogIfNeeded：单文件超过 5MB 时改名为 .1（最多保留 3 份）[PC-9]
// 调用方持有 logMu —— 内部不得再调用 log*（会自锁）。
func rotateLogIfNeeded(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return nil // 文件还不存在 → 无需轮转（不是错误）
	}
	if fi.Size() < logRotateSize {
		return nil
	}
	os.Remove(path + ".3")
	os.Rename(path+".2", path+".3")
	os.Rename(path+".1", path+".2")
	return os.Rename(path, path+".1")
}

// logTail：返回内存环形缓冲里最近 n 条（崩溃 dump 用）[PC-8]
func logTail(n int) []string {
	logMu.Lock()
	defer logMu.Unlock()
	if n > len(logRing) {
		n = len(logRing)
	}
	out := make([]string, n)
	copy(out, logRing[len(logRing)-n:])
	return out
}

// logf / logWarn / logErr：日志分级（PC-9）
func logf(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	appendLog(logLevelInfo, AppName+" "+s)
	fmt.Println(s)
}

func logWarn(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	appendLog(logLevelWarn, AppName+" "+s)
	fmt.Println(logLevelWarn + " " + s)
}

func logErr(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	appendLog(logLevelError, AppName+" "+s)
	fmt.Println(logLevelError + " " + s)
}

// ---------- Win32 ----------

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	procBox  = user32.NewProc("MessageBoxW")
	procExec = shell32.NewProc("ShellExecuteW")
)

func messageBox(text, caption string) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(caption)
	procBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0)
}

func trayWarn(text string) { go messageBox(text, "PhotoBeam") }

func trayNotify(title, text string) {
	// M1 用消息框展示关键事件（配对成功）；M2 换原生无边框弹窗
	go messageBox(text, title)
}

// 以管理员权限运行 netsh 添加防火墙规则（返回 >32 表示成功）[P1-11]
func shellExecuteRunAs(app, args string) int {
	a, _ := windows.UTF16PtrFromString(app)
	b, _ := windows.UTF16PtrFromString(args)
	v, _ := windows.UTF16PtrFromString("runas")
	r, _, _ := procExec.Call(0, uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)), 0, 1)
	return int(r)
}

