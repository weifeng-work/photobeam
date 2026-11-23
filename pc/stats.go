package main

// 每设备统计 + 最近活动（状态面板数据源）
//
// 落盘：appDir()/device_stats.json（原子写：tmp → fsync → rename）
// 只落「时间戳 + 计数」；文件名/目录名只在内存活动环里，不写盘 —— 面板显示离线设备的
// 历史活动靠磁盘目录扫描，不需要把文件名长期留在 JSON 里。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type devStat struct {
	LastAuth int64 `json:"last_auth"` // 最近一次认证成功（unix 秒，0 = 从未）
	LastSeen int64 `json:"last_seen"` // 最近一次「见到这个 IP 的连接」（含未认证的 30s 探测）
	LastFile int64 `json:"last_file"` // 最近一次落盘成功
	Total    int64 `json:"total"`     // 累计收到图片数
}

// lastActive：在线判定的时间基准 —— 认证时间与探测时间取较大者
func (s devStat) lastActive() int64 {
	if s.LastSeen > s.LastAuth {
		return s.LastSeen
	}
	return s.LastAuth
}

type activityItem struct {
	At   time.Time
	Dev  string
	File string
}

var (
	statsMu      sync.Mutex
	statsMap     = map[string]*devStat{}
	statsSession = map[string]int{} // 本次运行收到张数（fp → n）
	activityRing []activityItem
	statsSaveT   *time.Timer

	// procStart：本进程启动时刻（面板显示"运行多久"）
	procStart = time.Now()
)

const (
	statsFileName    = "device_stats.json"
	activityMaxKeep  = 8
	statsSaveDelay   = 2 * time.Second // 连拍时合并写盘，避免每张都 fsync
	onlineWindowSecs = 90              // 90 秒内有认证 = 在线（手机 30s 一轮探测）
)

func statsPath() string { return filepath.Join(appDir(), statsFileName) }

// loadDeviceStats：启动时读一次；文件损坏只记日志（面板显示空统计，不影响收发）
func loadDeviceStats() {
	b, err := os.ReadFile(statsPath())
	if err != nil {
		if !os.IsNotExist(err) {
			logf("读取设备统计失败: %v", err)
		}
		return
	}
	var wrapper struct {
		Devices map[string]*devStat `json:"devices"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		logf("设备统计 JSON 损坏，已忽略: %v", err)
		return
	}
	statsMu.Lock()
	for fp, s := range wrapper.Devices {
		if s != nil {
			statsMap[fp] = s
		}
	}
	statsMu.Unlock()
}

// statsRecordAuth：认证成功（含首次配对与每次重连）
func statsRecordAuth(fp, devName string) {
	if fp == "" {
		return
	}
	now := time.Now().Unix()
	statsMu.Lock()
	s := statsMap[fp]
	if s == nil {
		s = &devStat{}
		statsMap[fp] = s
	}
	s.LastAuth = now
	statsMu.Unlock()
	scheduleStatsSave()
}

// statsRecordFile：一张图落盘成功
func statsRecordFile(fp, devName, fileName string) {
	if fp == "" {
		return
	}
	now := time.Now().Unix()
	statsMu.Lock()
	s := statsMap[fp]
	if s == nil {
		s = &devStat{}
		statsMap[fp] = s
	}
	s.LastFile = now
	s.Total++
	statsSession[fp]++
	if devName == "" {
		devName = "未知设备"
	}
	activityRing = append(activityRing, activityItem{At: time.Now(), Dev: devName, File: fileName})
	if len(activityRing) > activityMaxKeep {
		activityRing = activityRing[len(activityRing)-activityMaxKeep:]
	}
	statsMu.Unlock()
	scheduleStatsSave()
}

// statsRecordSeenByIP：任何连进来的 TCP 都记一次「最近见到」
//
// 手机队列为空时每 30 秒只做 TCP 连通探测（probeLinkAlive），不发 HELLO、不认证，
// 所以只看认证时间会让状态面板恒显示「离线」。这里按已配对设备的 last_ip 认领，
// 多个设备共用同一 IP（几乎不可能）时只认第一个。
// 仅影响显示，不参与任何安全判断。
func statsRecordSeenByIP(ip string) {
	if ip == "" {
		return
	}
	// 先在锁外解析设备（避免 statsMu → cfgMu 的嵌套加锁）
	fp := ""
	for _, d := range devicesSnapshot() {
		if d.LastIP == ip {
			fp = d.FP
			break
		}
	}
	if fp == "" {
		return
	}
	statsMu.Lock()
	s := statsMap[fp]
	if s == nil {
		s = &devStat{}
		statsMap[fp] = s
	}
	s.LastSeen = time.Now().Unix()
	statsMu.Unlock()
	scheduleStatsSave()
}

// deviceNameOf：fp → 设备名（状态面板/活动环显示用）
func deviceNameOf(fp string) string {
	for _, d := range devicesSnapshot() {
		if d.FP == fp {
			return d.DevName
		}
	}
	return ""
}

// statsSnapshot：读一份副本（面板用，避免长时间持锁）
func statsSnapshot() (map[string]devStat, map[string]int) {
	statsMu.Lock()
	defer statsMu.Unlock()
	cp := make(map[string]devStat, len(statsMap))
	for k, v := range statsMap {
		cp[k] = *v
	}
	sess := make(map[string]int, len(statsSession))
	for k, v := range statsSession {
		sess[k] = v
	}
	return cp, sess
}

func recentActivities() []activityItem {
	statsMu.Lock()
	defer statsMu.Unlock()
	out := make([]activityItem, len(activityRing))
	copy(out, activityRing)
	return out
}

// scheduleStatsSave：2 秒防抖（200 张连拍只写一次盘）
func scheduleStatsSave() {
	statsMu.Lock()
	if statsSaveT != nil {
		statsSaveT.Reset(statsSaveDelay)
		statsMu.Unlock()
		return
	}
	statsSaveT = time.AfterFunc(statsSaveDelay, func() {
		statsMu.Lock()
		statsSaveT = nil
		statsMu.Unlock()
		saveDeviceStats()
	})
	statsMu.Unlock()
}

func saveDeviceStats() {
	statsMu.Lock()
	wrapper := struct {
		Devices map[string]*devStat `json:"devices"`
	}{Devices: make(map[string]*devStat, len(statsMap))}
	for k, v := range statsMap {
		cp := *v
		wrapper.Devices[k] = &cp
	}
	statsMu.Unlock()

	b, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		logf("序列化设备统计失败: %v", err)
		return
	}
	tmp := statsPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		logf("写设备统计失败: %v", err)
		return
	}
	if _, err = f.Write(b); err == nil {
		_ = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, statsPath())
	}
	if err != nil {
		logf("落盘设备统计失败: %v", err)
		_ = os.Remove(tmp)
	}
}
