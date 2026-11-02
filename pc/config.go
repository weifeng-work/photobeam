package main

// PhotoBeam Receiver (Windows PC) — M1
// 协议实现依据：docs/protocol-v1.md（v1.2，MiniMax 签字放行）

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"strings"
)

const (
	AppName       = "PhotoBeam"
	ProtoVer      = 1
	MinProtoVer   = 1
	UDPDiscoverPort = 5416 // 固定，被占则告警拒绝启动（协议 §0）
	TCPPortBase     = 5416
	TCPPortMax      = 5436 // 可漂移范围（协议 §0）
	MaxPlaintextFrame = 4096 // 明文 HELLO/MODE 上限（§3.1）
	MaxFrameLen     = 1 << 20 // 加密帧明文上限 1 MiB（§4）
	MaxChunkLen     = 256 << 10
)

type Device struct {
	FP       string `json:"fp"`
	EdPub    string `json:"ed_pub"`    // base64 std
	XIDPub   string `json:"x25519_id_pub"`
	DevName  string `json:"dev_name"`
	LastIP   string `json:"last_ip"`
	LastPort int    `json:"last_port"`
	PairedAt int64  `json:"paired_at"`
}

type Config struct {
	SaveDir          string   `json:"save_dir"`
	TCPPort          int      `json:"tcp_port"`
	UDPPort          int      `json:"udp_port"`
	Autostart        bool     `json:"autostart"`
	FWDone           bool     `json:"firewall_attempted"`
	ClipboardEnabled bool     `json:"clipboard_enabled"`
	ClipboardSeconds int      `json:"clipboard_seconds"`
	Devices          []Device `json:"devices"`
}

var (
	cfg      *Config
	identity *Identity
	cfgMu    sync.Mutex

	// 已落盘文件的幂等集合（file_id），M1 内存 + 落盘 received_ids.json
	seenMu   sync.Mutex
	seenIDs  = map[string]bool{}
)

func appDir() string {
	la := os.Getenv("LOCALAPPDATA")
	if la == "" {
		la = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
	}
	return filepath.Join(la, "PhotoBeam")
}

func loadConfig() *Config {
	p := filepath.Join(appDir(), "config.json")
	c := &Config{
		SaveDir:          filepath.Join(appDir(), "Received"),
		TCPPort:          TCPPortBase,
		UDPPort:          UDPDiscoverPort,
		ClipboardEnabled: true,
		ClipboardSeconds: 60,
	}
	if b, err := os.ReadFile(p); err == nil {
		if c2, err2 := parseJSON[Config](b); err2 == nil {
			c = &c2
		}
	}
	if c.SaveDir == "" {
		c.SaveDir = filepath.Join(appDir(), "Received")
	}
	if c.TCPPort < TCPPortBase || c.TCPPort > TCPPortMax {
		c.TCPPort = TCPPortBase
	}
	if c.UDPPort == 0 {
		c.UDPPort = UDPDiscoverPort
	}
	// 旧 config.json 缺剪贴板字段时补默认值（否则零值 false = 永久禁用）
	// [F-05] 只在"键缺失"时补 true；用户显式写了 false 必须尊重（隐私开关可关闭）
	if b, err := os.ReadFile(p); err == nil {
		if rawMap, err2 := parseJSON[map[string]any](b); err2 == nil {
			if _, present := rawMap["clipboard_enabled"]; !present {
				c.ClipboardEnabled = true
			}
		}
	}
	if c.ClipboardSeconds <= 0 {
		c.ClipboardSeconds = 60
	}
	// OneDrive 重定向检测（协议 §8）：保存目录在 OneDrive 路径下时警告
	if isUnderOneDrive(c.SaveDir) {
		trayWarn("接收目录位于 OneDrive 同步范围内，会违背『仅局域网、不上云』的承诺。\n" +
			"建议修改 config.json 的 save_dir（如 " + filepath.Join(appDir(), "Received") + "）")
	}
	os.MkdirAll(c.SaveDir, 0o755)
	os.MkdirAll(filepath.Join(c.SaveDir, ".tmp"), 0o755)
	os.MkdirAll(filepath.Join(appDir(), "logs"), 0o755)
	saveConfig(c)
	return c
}

func saveConfig(c *Config) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	b, _ := jsonMarshalIndent(c)
	tmp := filepath.Join(appDir(), "config.json.tmp")
	os.WriteFile(tmp, b, 0o644)
	os.Rename(tmp, filepath.Join(appDir(), "config.json"))
}

func findDevice(fp string) *Device {
	for i := range cfg.Devices {
		if cfg.Devices[i].FP == fp {
			return &cfg.Devices[i]
		}
	}
	return nil
}

func upsertDevice(d Device) {
	if e := findDevice(d.FP); e != nil {
		d.PairedAt = e.PairedAt
		*e = d
	} else {
		d.PairedAt = nowUnix()
		cfg.Devices = append(cfg.Devices, d)
	}
	saveConfig(cfg)
}

func clearDevices() {
	cfg.Devices = nil
	saveConfig(cfg)
}

// seenIDsMax [R-6]：幂等集合上限（FIFO 近似裁剪）——防 received_ids.json 无限增长
// 满时丢弃"最早插入"的 ID：map 无序，用 seenOrder 队列维持插入序
const seenIDsMax = 5000

var seenOrder []string // 插入顺序（配合 seenIDs 做裁剪 [R-6]）

func loadSeenIDs() {
	p := filepath.Join(appDir(), "received_ids.json")
	if b, err := os.ReadFile(p); err == nil {
		var arr []string
		if parseJSONInto(b, &arr) == nil {
			for _, id := range arr {
				if len(seenIDs) >= seenIDsMax {
					break // [R-6] 超限截断
				}
				seenIDs[id] = true
				seenOrder = append(seenOrder, id)
			}
		}
	}
}

func addSeenID(id string) {
	seenMu.Lock()
	if seenIDs[id] {
		seenMu.Unlock()
		return
	}
	seenIDs[id] = true
	seenOrder = append(seenOrder, id)
	for len(seenOrder) > seenIDsMax {
		oldest := seenOrder[0]
		seenOrder = seenOrder[1:]
		delete(seenIDs, oldest)
	}
	arr := append([]string(nil), seenOrder...)
	seenMu.Unlock()
	b, _ := jsonMarshalIndent(arr)
	tmp := filepath.Join(appDir(), "received_ids.json.tmp")
	os.WriteFile(tmp, b, 0o644)
	os.Rename(tmp, filepath.Join(appDir(), "received_ids.json"))
}

func isUnderOneDrive(dir string) bool {
	od := os.Getenv("OneDrive")
	if od == "" {
		return false
	}
	rel, err := filepath.Rel(od, dir)
	if err != nil {
		return false
	}
	// rel 以 ".." 开头 = 目标在 OneDrive 之外（此前只排除恰好 ".."，
	// 导致 ..\..\AppData 这类路径被误判，每次启动误弹警告）
	return !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func nowUnix() int64 { return timeNow().Unix() }

func logf(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	appendLog(AppName + " " + s)
	fmt.Println(s)
}
