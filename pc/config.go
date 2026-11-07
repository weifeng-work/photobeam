package main

// PhotoBeam Receiver (Windows PC) — M1
// 协议实现依据：docs/protocol-v1.md（v1.2，MiniMax 签字放行）

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

// ---------- 设备表读写（cfgMu 保护） ----------

// findDeviceLocked：调用方必须已持有 cfgMu。
func findDeviceLocked(fp string) *Device {
	for i := range cfg.Devices {
		if cfg.Devices[i].FP == fp {
			return &cfg.Devices[i]
		}
	}
	return nil
}

// deviceByFP：返回设备记录的**副本**（不存在时 ok=false）。
// 返回副本而不是切片内指针，避免调用方在锁外读写 cfg.Devices 造成数据竞争。
func deviceByFP(fp string) (Device, bool) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if e := findDeviceLocked(fp); e != nil {
		return *e, true
	}
	return Device{}, false
}

func upsertDevice(d Device) {
	cfgMu.Lock()
	if e := findDeviceLocked(d.FP); e != nil {
		d.PairedAt = e.PairedAt
		*e = d
	} else {
		d.PairedAt = nowUnix()
		cfg.Devices = append(cfg.Devices, d)
	}
	cfgMu.Unlock()
	saveConfig(cfg) // saveConfig 自己会再锁 cfgMu（不能持锁调用，否则自锁）
}

func clearDevices() {
	cfgMu.Lock()
	cfg.Devices = nil
	cfgMu.Unlock()
	saveConfig(cfg)
}

// updateDeviceNet：更新已配对设备的最近地址（锁内改字段，锁外落盘）。
func updateDeviceNet(fp, ip string, port int) {
	cfgMu.Lock()
	if e := findDeviceLocked(fp); e != nil {
		e.LastIP, e.LastPort = ip, port
	}
	cfgMu.Unlock()
	saveConfig(cfg)
}

// hasDevices：是否已配对过设备（discovery 应答的 paired 提示用）。
func hasDevices() bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return len(cfg.Devices) > 0
}

// devicesSnapshot：诊断文本用的设备列表副本（持锁读，锁外遍历）。
func devicesSnapshot() []Device {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	out := make([]Device, len(cfg.Devices))
	copy(out, cfg.Devices)
	return out
}

// ---------- 配置项并发访问 ----------
//
// saveConfig 在 cfgMu 内序列化**整个** Config，因此任何 cfg 字段的读写
// （尤其托盘 goroutine 与连接 goroutine 共享的开关）都必须走同一把锁，
// 否则会与 saveConfig 的 marshal 形成数据竞争。

func clipboardEnabled() bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.ClipboardEnabled
}

// tcpPort：当前生效的 TCP 监听端口（startTCPServer 探测落定后写入）。
func tcpPort() int {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.TCPPort
}

// udpPort：当前生效的 UDP 发现端口。
// [L3] 与 tcpPort 同风格：统一走 cfgMu，避免其他 goroutine 裸读 cfg 字段
// （与 saveConfig 的 marshal 形成数据竞争）。端口在启动后不再变化。
func udpPort() int {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.UDPPort
}

func setClipboardEnabled(on bool) {
	cfgMu.Lock()
	cfg.ClipboardEnabled = on
	cfgMu.Unlock()
}

func clipboardSeconds() int {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.ClipboardSeconds
}

func setClipboardSeconds(sec int) {
	cfgMu.Lock()
	cfg.ClipboardSeconds = sec
	cfgMu.Unlock()
}

func autostartFlag() bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.Autostart
}

func setAutostartFlag(on bool) {
	cfgMu.Lock()
	cfg.Autostart = on
	cfgMu.Unlock()
}

func firewallAttempted() bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.FWDone
}

func setFirewallAttempted(v bool) {
	cfgMu.Lock()
	cfg.FWDone = v
	cfgMu.Unlock()
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

// seenKeyFor：幂等键 = 设备指纹 + 文件 ID（Stage 10）
//
// [Stage 10] 原实现只用 file_id（内容 SHA-256）做幂等键 —— 两台手机传**同一张图**
// （内容相同 → file_id 相同）时，第二台会被判 duplicate 而**静默丢图**。
// 加入设备维度后：同一张图从两台手机传来，各存一份（落在各自的设备目录下）。
//
// 键的格式：`fp16:fileID64`，两者都用固定分隔符，避免拼接歧义。
// 旧版本写入的纯 file_id 键仍然留在 received_ids.json 里 —— 它们不再匹配新键，
// 只会让集合略微膨胀（上限 5000 会自动裁剪），不影响正确性。
func seenKeyFor(fp, fileID string) string {
	return fp + ":" + fileID
}

func addSeenID(id string) {
	seenMu.Lock()
	defer seenMu.Unlock()
	if seenIDs[id] {
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
	// [修复] marshal + 写 tmp + rename 必须都在 seenMu 内：
	// 多连接并发收图时，两个 goroutine 同时写 received_ids.json.tmp 会互相截断，
	// 写出损坏的 JSON → 下次启动 loadSeenIDs 解析失败 → 幂等历史全部丢失。
	b, _ := jsonMarshalIndent(arr)
	tmp := filepath.Join(appDir(), "received_ids.json.tmp")
	_ = os.WriteFile(tmp, b, 0o644)
	_ = os.Rename(tmp, filepath.Join(appDir(), "received_ids.json"))
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

// ---------- 保存目录读写（PC-2：向导窗/托盘可在运行时改目录） ----------

// cfg.SaveDir 与 Config 的其他字段一样由 cfgMu 保护——
// saveConfig 在 cfgMu 内序列化整个 Config，另用一把锁写同一字段会形成数据竞争。

func getSaveDir() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.SaveDir
}

func setSaveDir(d string) {
	cfgMu.Lock()
	cfg.SaveDir = d
	cfgMu.Unlock()
}

// applySaveDir：校验 → 建目录（含 .tmp）→ 写回配置并落盘
// 失败不写配置（保持旧目录继续可用）
func applySaveDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("目录路径为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("路径无效: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("无法创建目录: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(abs, ".tmp"), 0o755); err != nil {
		return fmt.Errorf("无法创建临时子目录: %w", err)
	}
	setSaveDir(abs)
	saveConfig(cfg)
	logf("接收目录已改为: %s", abs)
	return nil
}

// cleanupStaleParts：启动时清理接收目录 .tmp 下超过 24 小时的 .part 残留
// （协议 §9 第 11 条：崩溃后 .tmp 可清理；v1 不做断点续写）。
// 幂等：目录不存在 / 读取失败 / 删除失败都只记日志，不影响启动。
func cleanupStaleParts() {
	dir := filepath.Join(getSaveDir(), ".tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // 目录还不存在（首次运行）或不可读 → 无需清理
	}
	cutoff := timeNow().Add(-24 * time.Hour)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue // 24 小时内 → 可能是正在进行的传输，保留
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			logWarn("清理过期临时文件失败: %s（%v）", e.Name(), err)
			continue
		}
		removed++
	}
	if removed > 0 {
		logf("已清理过期临时文件 %d 个（.tmp 下超过 24 小时的 .part）", removed)
	}
}
