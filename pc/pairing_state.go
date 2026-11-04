package main

// 配对「待命态」[Stage 5 第五轮]
//
// 背景：同一局域网里可能同时跑着多对 PhotoBeam 用户（同事的、另一台机器上的）。
//
//	① 手机**首次**配对时手里没有任何可比对的指纹（指纹要握手之后才有），
//	   旧实现取「第一个应答」就连接 → 可能连到**别人的电脑** → 之后照片持续串到对方机器；
//	② 反过来，你的电脑也会被**陌生人的手机**触发弹窗；
//	   你若点「不一致」→ 真发 pair_reject → **打断对方的配对**。
//
// 闸门：PC 只有在「待命态」下才接受**新设备**的配对请求。
//   · 用户在托盘点「配对新设备」→ 待命（**一次性消耗**：成功配上一台即退出）；
//   · 首次启动且尚未配对过任何设备 → 自动待命 10 分钟（首配对零学习成本）；
//   · 不在待命态 → **不弹窗**，MODE 帧带 err{reason:"receiver_not_ready"}，
//     手机端立刻提示「让对方在托盘点『配对新设备』」，**不进入 SAS 阶段**；
//   · **已配对设备的重连完全不受待命态影响**（mode=reconnect，不走这条分支）。
//
// 关键性质：「对方没在等你」本身就是最强的甄别信号 ——
// 首次配对没有指纹可比，但「双方都同意」不依赖任何指纹。

import (
	"strings"
	"sync"
	"time"
)

var (
	pairArmMu    sync.Mutex
	pairArmUntil time.Time
)

const (
	pairArmWindow   = 2 * time.Minute  // 手动点「配对新设备」的待命时长
	pairArmFirstRun = 10 * time.Minute // 首次启动（无已配对设备）自动待命时长
)

// armPairing：进入待命态
func armPairing(d time.Duration) {
	pairArmMu.Lock()
	pairArmUntil = timeNow().Add(d)
	pairArmMu.Unlock()
	logf("进入配对待命态（%v 内接受新设备）", d)
}

// disarmPairing：退出待命态（成功配上一台即消耗掉）
func disarmPairing() {
	pairArmMu.Lock()
	was := !pairArmUntil.IsZero()
	pairArmUntil = time.Time{}
	pairArmMu.Unlock()
	if was {
		logf("配对待命态已结束")
	}
}

// pairingArmed：当前是否处于待命态（发现应答里的 pair_ready 用它）
func pairingArmed() bool {
	pairArmMu.Lock()
	defer pairArmMu.Unlock()
	if pairArmUntil.IsZero() {
		return false
	}
	return timeNow().Before(pairArmUntil)
}

// pairingArmRemaining：剩余待命秒数（tooltip 用；不在待命态返回 0）
func pairingArmRemaining() int {
	pairArmMu.Lock()
	defer pairArmMu.Unlock()
	if pairArmUntil.IsZero() {
		return 0
	}
	d := pairArmUntil.Sub(timeNow())
	if d <= 0 {
		return 0
	}
	return int(d.Seconds()) + 1
}

// ---------- 最近一次连上来的对端（手机）IP ----------

var (
	peerMu   sync.Mutex
	peerIP   string
)

// notePeerIP：记下最近一次连入的对端 IP（只取 IP 部分）
func notePeerIP(remoteAddr string) {
	ip := remoteAddr
	if i := strings.Index(ip, ":"); i > 0 {
		ip = ip[:i]
	}
	if ip == "" {
		return
	}
	peerMu.Lock()
	peerIP = ip
	peerMu.Unlock()
}

// lastPeerIP：最近一次连入的对端 IP（空 = 还没有连接过）
func lastPeerIP() string {
	peerMu.Lock()
	defer peerMu.Unlock()
	return peerIP
}
