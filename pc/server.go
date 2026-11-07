package main

// TCP 监听、握手、接收循环 — 协议 §3/§4/§5

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

const (
	handshakeTimeout = 15 * time.Second
	frameTimeout     = 45 * time.Second
	plaintextMax     = MaxPlaintextFrame
	maxFileSize      = 200 << 20 // 单文件上限 200MB（超出即断）
)

// ---------- 配对速率限制（协议 §3.4） ----------
//
// 规则（协议原文）：每个 IP 每分钟配对失败 ≤5 次；连续失败 5 次 → 冷却 60 秒；
// 冷却期内该 IP 的配对请求一律以 MODE err{reason:"pairing_cooldown"} 明确拒绝
// （不静默断链，让手机知道要等）。
//
// 计数口径（协议 §3.4 原文：「计数点 = 收到 PAIR_CONFIRM 且校验失败时」）：
//   · **计入冷却**的只有两类异常：① 未展示向导窗却收到 pair_confirm；
//     ② SAS 或签名校验失败。
//   · **不计入冷却**：PC 用户点「不一致」、PC 端等待确认超时、手机端主动 pair_reject。
//     这三条都是协议 §3.2 的**正常工作路径**（用户人眼核对后主动终止），不是攻击。
//     旧实现把前两条也计了进去，且与手机端**口径不对称**（提交注释特意声明
//     「手机端主动 pair_reject 不计」）→ 用户连续 5 次没看/没点配对对话框
//     （或看错数字点了「不一致」）就被锁 60 秒；
//     而手机端没有 pairing_cooldown 分支 → 用户只看到「对方电脑已经拒绝或断开了配对」，
//     完全不知道自己被限流 —— 限流变成玄学失败。
//   · 用户拒绝/超时**单独计数**（pairingUserFail，阈值更高），只记日志，不触发冷却。
//   · 已配对设备的重连失败不进这里（诊断计数 authStats 与之分离）。
//   · 配对成功 → 清空该 IP 的全部失败记录。
// 内存安全：按需回收 24 小时无活动的条目，防恶意刷 IP 撑爆 map。

const (
	pairFailWindow     = 60 * time.Second // 失败计数窗口
	pairFailMax        = 5                // 窗口内允许的失败次数上限
	pairCooldown       = 60 * time.Second // 触发后的冷却时长
	pairStatRetention  = 24 * time.Hour   // 条目保留时长
	pairStatGCInterval = 5 * time.Minute  // 回收检查间隔
	// [F2] 用户拒绝/确认超时的**单独统计**参数：属正常工作路径，
	// 阈值远高于异常失败（5 次），只用于诊断留痕，**绝不触发冷却**。
	pairUserFailWindow = 10 * time.Minute
	pairUserFailMax    = 20
)

type pairFailStat struct {
	times         []time.Time // 窗口内的失败时刻
	cooldownUntil time.Time   // 冷却截止时刻（零值 = 不在冷却期）
	lastSeen      time.Time   // 最近一次活动（GC 用）
	lastNotify    time.Time   // 最近一次「限流原因」提示时间（防刷屏）
}

var (
	pairStatMu       sync.Mutex
	pairStatByIP     = map[string]*pairFailStat{}
	pairUserFailByIP = map[string][]time.Time{} // [F2] 用户拒绝/超时单独统计
	pairStatGCLas    = time.Time{}
)

// pairingCooldownRemaining：该 IP 剩余冷却时长（0 = 不在冷却期）。
func pairingCooldownRemaining(ip string) time.Duration {
	pairStatMu.Lock()
	defer pairStatMu.Unlock()
	s := pairStatByIP[ip]
	if s == nil {
		return 0
	}
	if d := time.Until(s.cooldownUntil); d > 0 {
		return d
	}
	return 0
}

// pairingCooldownFail：记录一次配对失败；窗口内达到上限 → 进入冷却。
func pairingCooldownFail(ip string) {
	now := time.Now()
	pairStatMu.Lock()
	defer pairStatMu.Unlock()
	pairStatGC(now)
	s := pairStatByIP[ip]
	if s == nil {
		s = &pairFailStat{}
		pairStatByIP[ip] = s
	}
	s.lastSeen = now
	// 只保留窗口内的失败时刻（过期的不再计入）
	keep := s.times[:0]
	for _, t := range s.times {
		if now.Sub(t) < pairFailWindow {
			keep = append(keep, t)
		}
	}
	s.times = append(keep, now)
	if s.cooldownUntil.Before(now) && len(s.times) >= pairFailMax {
		s.cooldownUntil = now.Add(pairCooldown)
		s.times = nil // 冷却期内不再累计，等冷却自然过期
		logWarn("IP %s 在 %v 内配对失败达 %d 次 → 冷却 %v", ip, pairFailWindow, pairFailMax, pairCooldown)
	}
}

// pairingCooldownReset：配对成功 → 清空该 IP 的失败记录与冷却。
func pairingCooldownReset(ip string) {
	pairStatMu.Lock()
	delete(pairStatByIP, ip)
	delete(pairUserFailByIP, ip)
	pairStatMu.Unlock()
}

// pairingUserFail：记录一次「用户主动拒绝 / 等待确认超时」这类**正常工作路径**的失败。
//
// [F2] 与 pairingCooldownFail **分开统计**：协议 §3.4 的计数点只有
// 「收到 PAIR_CONFIRM 且校验失败」，用户没看对话框、看错数字点了「不一致」
// 都不该被当成攻击而锁 60 秒。这里只做诊断留痕，达到更高阈值时记一条日志，
// **绝不触发冷却**。
func pairingUserFail(ip, reason string) {
	now := time.Now()
	pairStatMu.Lock()
	defer pairStatMu.Unlock()
	times := pairUserFailByIP[ip]
	keep := times[:0]
	for _, t := range times {
		if now.Sub(t) < pairUserFailWindow {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	pairUserFailByIP[ip] = keep
	if len(keep) >= pairUserFailMax {
		logWarn("IP %s 在 %v 内多次拒绝/超时配对（%d 次，最近：%s）—— 属正常工作路径，不触发冷却",
			ip, pairUserFailWindow, len(keep), reason)
	}
}

// notifyPairingCooldown：PC 端把「为什么被限流」明确告诉用户。
//
// [F2] 背景：手机端没有 pairing_cooldown 的文案分支，用户只会看到
// 「对方电脑已经拒绝或断开了配对（连接已关闭）」，完全不知道自己被限流 ——
// 限流变成了一种「玄学失败」。PC 端在这里补上明确原因。
// 同一个 IP 的一次冷却期内只提示一次（冷却 60 秒，期间手机可能反复重试）。
func notifyPairingCooldown(ip string, left time.Duration) {
	pairStatMu.Lock()
	s := pairStatByIP[ip]
	if s == nil {
		pairStatMu.Unlock()
		return
	}
	if !s.lastNotify.IsZero() && time.Since(s.lastNotify) < pairCooldown {
		pairStatMu.Unlock()
		return // 本次冷却已提示过 → 不重复打扰用户
	}
	s.lastNotify = time.Now()
	pairStatMu.Unlock()
	messageBox(fmt.Sprintf(
		"来自 %s 的配对请求被暂时限流，约 %d 秒后才能重试。\n\n"+
			"原因：短时间内多次配对校验未通过（协议 §3.4 的速率限制）。\n"+
			"如果这不是你在操作，请检查局域网内是否有陌生设备在反复尝试配对。",
		ip, int(left.Seconds())+1),
		"PhotoBeam 配对被限流")
}

// pairStatGC：回收长期不活跃的条目。调用方必须已持有 pairStatMu。
func pairStatGC(now time.Time) {
	if now.Sub(pairStatGCLas) < pairStatGCInterval {
		return
	}
	pairStatGCLas = now
	for ip, s := range pairStatByIP {
		if now.Sub(s.lastSeen) > pairStatRetention {
			delete(pairStatByIP, ip)
		}
	}
	// [F2] 用户拒绝/超时的单独统计同样要回收（否则 map 会被恶意 IP 撑爆）
	for ip, times := range pairUserFailByIP {
		if len(times) == 0 || now.Sub(times[len(times)-1]) > pairStatRetention {
			delete(pairUserFailByIP, ip)
		}
	}
}

// ---------- 明文帧（HELLO/MODE，[u32 BE len][body]） ----------

func readPlainFrame(conn net.Conn) ([]byte, error) {
	var lb [4]byte
	if _, err := io.ReadFull(conn, lb[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n == 0 || n > plaintextMax {
		return nil, errors.New("明文帧长度非法")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(conn, b); err != nil {
		return nil, err
	}
	return b, nil
}

func writePlainFrame(conn net.Conn, body []byte) error {
	if len(body) > plaintextMax {
		return errors.New("明文帧超限")
	}
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(body)))
	if _, err := conn.Write(lb[:]); err != nil {
		return err
	}
	_, err := conn.Write(body)
	return err
}

// ---------- 加密帧 ----------

func readEncFrame(conn net.Conn, key []byte) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return 0, nil, err
	}
	plen := binary.BigEndian.Uint32(head[1:5])
	if plen > MaxFrameLen {
		return 0, nil, errFrameTooLarge
	}
	rest := make([]byte, 12+int(plen)+16)
	if _, err := io.ReadFull(conn, rest); err != nil {
		return 0, nil, err
	}
	full := append(head[:], rest...)
	return openFrame(key, full)
}

func writeEncFrame(conn net.Conn, key []byte, frameType byte, payload []byte) error {
	f, err := sealFrame(key, nil, frameType, payload)
	if err != nil {
		return err
	}
	_, err = conn.Write(f)
	return err
}

// ---------- TCP 服务 ----------

// tcpListener 全局保存：托盘「重启 PhotoBeam」需要**先释放端口**，
// 否则新进程一起来就撞上「5416-5436 全部被占」而弹框退出。
var (
	tcpLnMu sync.Mutex
	tcpLn   net.Listener
)

func startTCPServer() error {
	var ln net.Listener
	var err error
	for port := TCPPortBase; port <= TCPPortMax; port++ {
		ln, err = net.Listen("tcp4", fmt.Sprintf(":%d", port))
		if err == nil {
			cfgMu.Lock()
			cfg.TCPPort = port
			cfgMu.Unlock()
			saveConfig(cfg)
			logf("TCP 监听 :%d", port)
			break
		}
	}
	if err != nil {
		return errors.New("TCP 端口 5416-5436 全部被占")
	}
	tcpLnMu.Lock()
	tcpLn = ln
	tcpLnMu.Unlock()
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				// [修复] 一次瞬时 accept 错误不能结束监听循环——否则托盘看似正常，
				// PC 却永久不再接受新连接。只有监听器确实被关闭（stopTCPServer /
				// 进程退出）才退出；其余错误短暂退避后继续。
				if errors.Is(e, net.ErrClosed) {
					logf("accept 停止: %v", e)
					return
				}
				logWarn("accept 失败: %v（100ms 后继续监听）", e)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			go goroutineSafe("handleConn", func() { handleConn(conn) })
		}
	}()
	return nil
}

// stopTCPServer：关闭 TCP 监听（重启前调用，释放端口）
func stopTCPServer() {
	tcpLnMu.Lock()
	ln := tcpLn
	tcpLn = nil
	tcpLnMu.Unlock()
	if ln != nil {
		_ = ln.Close()
		logf("TCP 监听已释放（准备重启）")
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()
	// [Stage 5 第五轮] 记下对端 IP —— 托盘 tooltip 用它挑「手机所在网段」的本地地址
	remote := conn.RemoteAddr().String()
	notePeerIP(remote)
	// 配对速率限制（协议 §3.4）按连接源 IP 统计
	peerIP, _, _ := net.SplitHostPort(remote)
	logf("连接进入: %s", remote)

	conn.SetDeadline(timeNow().Add(handshakeTimeout))

	// P0-07：先写自己的 HELLO 再读对方（协议 §3.1：任何一端不得等待对方先发）
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		logf("生成临时密钥失败: %v", err)
		return
	}
	myNonce := randomBytes(16)
	receiverHello := Hello{
		PB: 1, Type: "hello", Ver: ProtoVer,
		EdPub:       b64encode(identity.EdPub),
		X25519IDPub: b64encode(identity.XIDPub[:]),
		X25519Eph:   b64encode(eph.PublicKey().Bytes()),
		Nonce:       b64encode(myNonce),
		DevName:     hostname(),
		Role:        "receiver",
	}
	hb, _ := jsonMarshalIndent(receiverHello)
	if err := writePlainFrame(conn, hb); err != nil {
		logf("写 HELLO 失败: %v", err)
		return
	}

	// 读 Sender HELLO（明文）
	b, err := readPlainFrame(conn)
	if err != nil {
		// [R-8] 手机端存活探测（直连即断）每 30s 一次，EOF-before-HELLO 不记错误日志防刷屏
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			if err.Error() != "EOF" {
				logf("读 HELLO 失败: %v", err)
			}
		}
		return
	}
	senderHello, err := parseJSON[Hello](b)
	if err != nil || senderHello.Type != "hello" || senderHello.PB != 1 {
		logf("HELLO 非法")
		return
	}
	// ver 校验（明文阶段经 MODE.err 报告 [S-02]）
	if senderHello.Ver > ProtoVer || senderHello.Ver < MinProtoVer {
		modeFrame, _ := jsonMarshalIndent(map[string]any{
			"pb": 1, "type": "mode", "mode": "pairing", "fp": identity.FP(),
			"reset": false,
			"err":   map[string]any{"reason": "version_unsupported", "min_ver": MinProtoVer, "ver": senderHello.Ver},
		})
		writePlainFrame(conn, modeFrame)
		logf("版本不匹配: 对端 ver=%d", senderHello.Ver)
		return
	}

	edRaw, _ := b64decode(senderHello.EdPub)
	xidRaw, _ := b64decode(senderHello.X25519IDPub)
	senderFP := fingerprint(ed25519.PublicKey(edRaw), to32(xidRaw))
	mode := "pairing"
	if _, known := deviceByFP(senderFP); known {
		mode = "reconnect"
	}
	// [协议 §3.4] 按 IP 的配对失败速率限制：该 IP 在冷却期内 → MODE 帧回
	// err{reason:"pairing_cooldown"} 并断链（不静默，让手机明确知道要等）。
	// 只对 pairing 生效——已配对设备的重连失败只进诊断计数，不触发冷却（两类计数分离）。
	if mode == "pairing" {
		if left := pairingCooldownRemaining(peerIP); left > 0 {
			logf("IP %s 处于配对冷却期（剩余 %v），拒绝配对请求", peerIP, left.Round(time.Second))
			cooldownFrame, _ := jsonMarshalIndent(map[string]any{
				"pb": 1, "type": "mode", "mode": "pairing", "fp": identity.FP(), "reset": false,
				// [F2] retry_after：告诉手机还要等多久（旧版手机忽略未知字段，向后兼容）
				"err": map[string]any{"reason": "pairing_cooldown", "retry_after": int(left.Seconds()) + 1},
			})
			_ = writePlainFrame(conn, cooldownFrame)
			// [F2] PC 端给出明确原因：手机端没有 pairing_cooldown 文案分支，
			// 用户只看到「对方电脑已经拒绝或断开了配对」，不知道自己被限流。
			// 放 goroutine 里弹窗：不阻塞连接收尾，也不拖住本连接。
			go goroutineSafe("pairing-cooldown", func() {
				notifyPairingCooldown(peerIP, left)
			})
			return
		}
	}
	// [Stage 5 第五轮 · 配对待命闸门]
	// 新设备（PC 不认识这个指纹）的配对请求，只有在 PC 处于「配对新设备」待命态时才受理。
	// 不在待命态 → **不弹窗**，MODE 帧带 err{reason:"receiver_not_ready"}，
	// 手机端立刻提示「让对方在托盘点『配对新设备』」，**不进入 SAS 阶段**。
	// 根因：手机首次配对时没有指纹可比，旧实现取「第一个应答」就连接 →
	//       可能连到同一局域网里**别人的电脑**，之后照片持续串到对方机器；
	//       反过来你的电脑也会被陌生人的手机触发弹窗，你点「不一致」还会打断对方配对。
	// 注意：**已配对设备的重连（mode=reconnect）完全不受待命态影响**。
	if mode == "pairing" && senderHello.pairIntent() && !pairingArmed() {
		logf("收到陌生设备的配对请求，但本机不在『配对新设备』待命态 → 拒绝（不弹窗）")
		modeFrame, _ := jsonMarshalIndent(map[string]any{
			"pb": 1, "type": "mode", "mode": "pairing", "fp": identity.FP(), "reset": false,
			"err": map[string]any{"reason": "receiver_not_ready"},
		})
		_ = writePlainFrame(conn, modeFrame)
		return
	}
	logf("Sender fp=%s mode=%s", senderFP, mode)

	// MODE 帧（明文）
	modeFrame, _ := jsonMarshalIndent(map[string]any{
		"pb": 1, "type": "mode", "mode": mode, "fp": identity.FP(), "reset": false,
	})
	if err := writePlainFrame(conn, modeFrame); err != nil {
		logf("写 MODE 失败: %v", err)
		return
	}

	// 密钥协商 [N-01]：shared = X25519(本端临时私钥, 对端临时公钥)
	senderEphRaw, _ := b64decode(senderHello.X25519Eph)
	senderNonceRaw, _ := b64decode(senderHello.Nonce)
	keys, err := deriveSession(eph, senderEphRaw,
		senderEphRaw, eph.PublicKey().Bytes(),
		senderNonceRaw, myNonce)
	if err != nil {
		logf("密钥协商失败: %v", err)
		return
	}
	th, err := transcriptHash(&senderHello, &receiverHello)
	if err != nil {
		logf("transcript 失败: %v", err)
		return
	}

	// 认证（P0-05：reconnect 验签失败 → 降级 pairing（reset:true）→ SAS 重建，只降级一次）
	conn.SetDeadline(timeNow().Add(frameTimeout))
	for {
		if mode == "reconnect" {
			ft, pt, err := readEncFrame(conn, keys.Session)
			if err != nil {
				logf("读认证帧失败: %v", err)
				return
			}
			if ft != FrameCtrlJSON {
				return
			}
			var m struct {
				Type string `json:"type"`
				Sig  string `json:"sig"`
			}
			if parseJSONInto(pt, &m) != nil || m.Type != "auth" {
				return
			}
			dev, known := deviceByFP(senderFP)
			sigOK := known && ed25519.Verify(ed25519.PublicKey(edRaw), th, mustDecode(m.Sig))
			logf("重连验签: fp=%s dev=%v 结果=%v", senderFP, known, sigOK)
			if sigOK {
				okSig := ed25519.Sign(identity.EdPriv, th)
				// [Stage 10] 回传 dev_dir：手机端据此显示「我的照片在电脑的 <dev_dir> 目录」。
				// **不参与 transcript**（与 pair_intent 同理）→ 不影响签名、不影响 SAS、
				// 不触发重新配对；旧版手机端收到多出的字段会直接忽略（向后兼容）。
				if err := sendCtrl(conn, keys.Session, map[string]any{
					"type": "auth_ok", "sig": b64encode(okSig), "dev_dir": deviceDirName(dev.DevName, senderFP),
				}); err != nil {
					return
				}
				ip, portStr, _ := net.SplitHostPort(remoteOf(conn))
				var pn int
				fmt.Sscanf(portStr, "%d", &pn)
				// [修复] 设备表的更新走锁内 helper，不能在锁外改切片元素（数据竞争）
				updateDeviceNet(senderFP, ip, tcpPort()) // last_port = PC 自身监听端口 [P1-01]
				logf("重连认证通过: %s", dev.DevName)
				authStats.ok()
				break
			}
			// 验签失败（pin 过期/伪造）。
			// [修复] 不能在这里补发第二条明文 MODE（mode=pairing, reset:true）：
			// 加密通道已经建立，手机端 handshake() 只在加密前读一次 MODE，
			// 这条明文帧会被当成加密帧解析 → 手机抛异常断链，
			// 「配对记录已重置」的提示永远到不了。
			// 直接关闭本连接，让手机下一轮重连走正常的 HELLO→MODE(pairing) 流程。
			authStats.fail("重连验签失败（对端身份不匹配）")
			logf("重连验签失败（对端身份不匹配 / pin 过期），关闭连接；请让手机重新配对")
			return
		}
		// pairing 分支
		logf("配对模式 SAS=%s（等待手机 pair_confirm）", keys.SAS)
		// [PC-1] 弹出配对向导窗：协议 §3.2 / 验收 §9.1 要求「两端同时展示 SAS」——
		// 此前 PC 端只有这一行日志，用户看不到那 6 位数字，手机端的核对框无从完成
		// 配对阶段要留出 pairTimeout（60s）给用户核对 6 位数字，帧超时（45s）盖不住 →
		// 这里把连接截止时间延长到「向导超时 + 30s 余量」。
		// [Stage 5 修订] 原先沿用 frameTimeout(45s) 配 15s 向导窗；窗口放宽到 60s 后必须同步放宽。
		conn.SetDeadline(timeNow().Add(pairTimeout + 30*time.Second))
		//
		// [Stage 5 修复 · 配对意图 + 单实例]
		// (1) 意图：只有 Sender 明确声明「用户主动发起配对」(HELLO.pair_intent=true，
		//     字段缺省按 true 处理以兼容旧版本)才弹向导窗。
		//     手机端的**后台同步服务每 30 秒自动重连一次**，若也弹窗，
		//     用户正在核对的窗口会被反复顶掉（实测 19:11~19:16 每 30 秒弹一个新窗）。
		// (2) 单实例：showPairingWizard 内部保证同一时刻只有一个向导窗。
		//     pw == nil 时人眼核对根本未发生 —— 即便收到 pair_confirm 也必须拒绝（fail closed）。
		var pw *pairingSession
		if senderHello.pairIntent() {
			pw = showPairingWizard(keys.SAS, identity.FP(), senderHello.DevName)
			defer pw.close()
		} else {
			logf("后台连接请求配对（pair_intent=false）：按协议不弹向导窗，人眼核对不成立")
		}
		// 向导窗的用户动作：点『不一致』→ 发 pair_reject（协议 §3.2：任一端均可发）；
		// 60 秒超时 → 断开（与手机端 SAS 对话框 60s 等待对齐）
		// [Stage 5 第五轮 · 人眼核对硬闸门 D-b]
		// 旧实现：PC 的「一致」只是一行日志，**收到 pair_confirm 就接受** → PC 端的人眼核对形同虚设
		//        （真正的闸门只有手机端，用户在电脑上「看一眼」这个动作根本不参与决策）。
		// 现在：PC 用户必须点「一致」，本端才接受 pair_confirm；不一致 / 超时 = 拒绝。
		// 双方表态通过 agreeCh 传递（容量 1）：用户先点或后到都能正确取到。
		var pairingDecided int32 // 0=进行中 1=已决（原子：与主流程互斥）
		agreeCh := make(chan bool, 1) // true=用户点了「一致」
		if pw != nil {
			go func() {
				ev, ok := <-pw.events
				if !ok {
					// 会话被关闭且没有用户决策：
					//   · 收到 pair_confirm 时主流程已 CAS（不会走到这里）
					//   · 被**新**的向导窗取代（重复配对）→ 中断旧连接，别让旧连接干等 45 秒
					if atomic.CompareAndSwapInt32(&pairingDecided, 0, 1) {
						logf("配对向导窗被新会话取代，断开旧连接")
						abortConn(conn)
					}
					agreeCh <- false
					return
				}
				switch ev {
				case pairEvMatch:
					// 点『一致』→ 记下用户已确认（窗口此刻已关闭）；等 pair_confirm 到达后据此放行
					logf("用户已核对 SAS（一致），等待手机 pair_confirm")
					agreeCh <- true
				case pairEvMismatch:
					if !atomic.CompareAndSwapInt32(&pairingDecided, 0, 1) {
						return
					}
					logf("用户拒绝配对（已发送 pair_reject）")
					if err := sendCtrl(conn, keys.Session, map[string]any{"type": "pair_reject"}); err != nil {
						logErr("发送 pair_reject 失败: %v", err)
					}
					abortConn(conn)
					// [Stage 5] 协议 §3.2 要求「Receiver 向导显示『用户已拒绝配对』」——
					// 此前只有托盘气泡（多数情况下用户根本没注意到），现在给一个明确的对话框。
					agreeCh <- false
					trayNotify("配对已拒绝", "你在电脑上点了『不一致』，已断开连接")
					messageBox("你在电脑上点了『不一致』，已拒绝本次配对。\n"+
						"连接已断开。请确认两端显示的 6 位数字确实不同，再重新配对。",
						"PhotoBeam 配对已拒绝")
				case pairEvTimeout:
					if !atomic.CompareAndSwapInt32(&pairingDecided, 0, 1) {
						return
					}
					logf("配对超时：%d 秒内未完成双方确认，已断开", pairTimeoutMs/1000)
					abortConn(conn)
					agreeCh <- false
					trayNotify("配对超时", fmt.Sprintf("%d 秒内未完成双方确认，连接已断开", pairTimeoutMs/1000))
					messageBox(fmt.Sprintf("%d 秒内没有完成双方确认（手机或电脑任一侧没点），配对已取消。\n"+
						"请重新点手机上的「② 配对电脑」，并在电脑上点「一致」。", pairTimeoutMs/1000),
						"PhotoBeam 配对超时")
				}
			}()
		}
		ft, pt, err := readEncFrame(conn, keys.Session)
		if err != nil {
			if atomic.LoadInt32(&pairingDecided) == 1 {
				return // 用户拒绝 / 超时（原因已记日志）
			}
			logf("读认证帧失败: %v", err)
			return
		}
		// 关闭向导窗并标记「已决」（否则看门 goroutine 会误报『被新会话取代』）
		finish := func() {
			atomic.StoreInt32(&pairingDecided, 1)
			if pw != nil {
				pw.close()
			}
		}
		if ft != FrameCtrlJSON {
			finish()
			return
		}
		var m struct {
			Type    string `json:"type"`
			SAS     string `json:"sas"`
			Sig     string `json:"sig"`
			DevName string `json:"dev_name"`
		}
		if parseJSONInto(pt, &m) != nil {
			finish()
			return
		}
		if m.Type == "pair_reject" {
			finish()
			logf("手机端已拒绝配对（pair_reject）")
			trayNotify("手机端已拒绝配对", "两端数字不一致，已断开")
			// [Stage 5] 同上：手机上点了「不一致」/ 超时取消，电脑端也要明确告知用户
			messageBox("手机端拒绝配对（两端显示的数字不一致），连接已断开。\n"+
				"请确认中间没有第三方设备，然后重新配对。",
				"PhotoBeam 配对未完成")
			return
		}
		if m.Type != "pair_confirm" {
			finish()
			return
		}
		if !atomic.CompareAndSwapInt32(&pairingDecided, 0, 1) {
			// 用户已点『不一致』或已超时 → 不再确认配对
			finish()
			return
		}
		// [Stage 5 修复 · fail closed] 没弹向导窗 = 用户从未看见 PC 端的 6 位码，
		// 人眼核对没有发生 → **不能接受配对**。
		// （程序侧 SAS 比对只能证明两端算出同一个数，证明不了「没有人被中间人骗过」。）
		if pw == nil {
			logf("收到 pair_confirm 但本端未展示 SAS 向导窗（pair_intent=false），拒绝配对")
			authStats.fail("未展示 SAS 向导窗却收到 pair_confirm")
			pairingCooldownFail(peerIP) // [F2] 协议 §3.4 计数点之一：收到 pair_confirm 却校验不成立 → 计入冷却
			finish()
			return
		}
		// [Stage 5 第五轮 · D-b 硬闸门] 收到 pair_confirm **还不够** —— 必须等 PC 用户点「一致」。
		// 旧实现到这里就直接接受，PC 端的人眼核对（让人看到那 6 位码）形同虚设。
		// 现在：用户点了「一致」才放行；点了「不一致」/ 超时 / 被新会话取代 → 拒绝。
		// agreeCh 容量 1：用户先点（已入缓冲）或后到（这里等）都能正确取到。
		select {
		case agreed := <-agreeCh:
			if !agreed {
				logf("PC 用户未确认（点不一致 / 超时 / 被取代）→ 不接受配对")
				authStats.fail("PC 用户未确认，拒绝配对")
				// [F2] 用户主动拒绝是协议 §3.2 的正常工作路径，**不计入冷却**
				// （与手机端主动 pair_reject 口径对称）→ 单独统计，只留痕。
				pairingUserFail(peerIP, "PC 用户未确认（点不一致/超时/被新会话取代）")
				finish()
				return
			}
		case <-time.After(time.Duration(pairTimeoutMs+5) * time.Millisecond):
			logf("等待 PC 用户确认超时 → 不接受配对")
			authStats.fail("等待 PC 用户确认超时")
			// [F2] 同上：用户一直没点确认不是攻击，不触发 60 秒冷却。
			pairingUserFail(peerIP, "等待 PC 用户确认超时")
			finish()
			return
		}
		pw.close() // 用户已点「一致」→ 关闭向导窗
		if !ctEqual(m.SAS, keys.SAS) || !ed25519.Verify(ed25519.PublicKey(edRaw), th, mustDecode(m.Sig)) {
			logf("配对校验失败（SAS 或签名），静默断开")
			authStats.fail("配对校验失败（SAS 或签名不一致）")
			pairingCooldownFail(peerIP) // [F2] 协议 §3.4 计数点之一：SAS/签名校验失败 → 计入冷却
			return
		}
		authStats.ok()
		pairingCooldownReset(peerIP) // 协议 §3.4：配对成功 → 清空该 IP 的失败记录
		okSig := ed25519.Sign(identity.EdPriv, th)
		// [Stage 10] 回传 dev_dir（不参与 transcript，密码学中性，向后兼容）
		pairDevName := firstNonEmpty(m.DevName, senderHello.DevName)
		if err := sendCtrl(conn, keys.Session, map[string]any{
			"type": "pair_ok", "sig": b64encode(okSig), "dev_dir": deviceDirName(pairDevName, senderFP),
		}); err != nil {
			return
		}
		ip, portStr, _ := net.SplitHostPort(remoteOf(conn))
		var pn int
		fmt.Sscanf(portStr, "%d", &pn)
		upsertDevice(Device{FP: senderFP, EdPub: senderHello.EdPub, XIDPub: senderHello.X25519IDPub,
			DevName: pairDevName, LastIP: ip, LastPort: tcpPort()})
		// [Stage 5 第五轮] 配对待命**一次性消耗**：成功接上一台新设备就退出待命。
		// 这样「每加一台手机都要显式点一次『配对新设备』」—— 避免陌生人随时蹭配对。
		disarmPairing()
		updateTrayTooltip()
		trayNotify("配对成功", "已与 "+firstNonEmpty(m.DevName, senderHello.DevName)+" 完成配对")
		logf("配对成功: %s (%s)", m.DevName, senderFP)
		break
	}

	// 消息循环（文件接收）；落盘成功 → 剪贴板+弹窗（异步 [MiniMax 坑 6]）
	logf("进入消息循环")
	// [PC-4] 弹窗显示**手机端原始文件名** + 已保存目录（落盘名带毫秒前缀，用户对不上）
	onFile := func(destPath, origName, saveDir string) {
		go goroutineSafe("clipboard+popup", func() {
			if !clipboardEnabled() {
				return
			}
			// [Stage 6 · 延迟优化] 只做关键路径：解码 + DIB（约 100ms）。
			// PNG 重编码（846ms）由 writeClipboardWithRestore 的后台阶段完成——
			// CF_DIB 写入后用户立刻就能 Ctrl+V。
			name := origName
			if name == "" {
				name = filepath.Base(destPath)
			}
			saved := "已保存到 " + ellipsisTail(saveDir, 34) // [PC-4] 路径过长时保留尾部
			t0 := timeNow()
			dib, imgW, imgH, err := prepareImageDataFast(destPath)
			if err != nil {
				logf("图片解码失败: %v", err)
				// [M2] 解码失败（gif/bmp/webp/heic 等未注册解码器的格式）时，
				// 图片**已经落盘**（用户在接收文件夹里能看到），只是没进剪贴板。
				// 旧实现只 logf 就 return —— 用户零反馈，以为「同步失败了」，
				// 实际图就在硬盘上，白白重传。这里明确告诉用户「已保存，但格式不支持写剪贴板」。
				showPopup4("已保存，但不支持写入剪贴板", name,
					"该格式暂不支持写入剪贴板（仅 JPG/PNG 可写）", saved, 8000)
				return
			}
			tDIB := timeNow()
			initClipboardFormats()
			// 写入成功才弹窗承诺 [F-03]：失败只弹"写入失败"提示，绝不假承诺可粘贴
			// PNG 编码交给后台阶段（代际号校验，避免覆盖更新的图）
			ok, snapNil := writeClipboardWithRestore(dib, func() []byte {
				return preparePNGAsync(destPath)
			}, clipboardSeconds(), nil)
			tClip := timeNow()
			logf("剪贴板关键路径: 解码+DIB=%v 写入=%v（PNG 后台补写）",
				tDIB.Sub(t0), tClip.Sub(tDIB))
			if ok {
				hint := "Ctrl+V 可粘贴 · " + fmt.Sprint(clipboardSeconds()) + " 秒后还原"
				if snapNil {
					hint = "原内容无可还原，本次不还原" // 中性表述：真因多为上一次同步的图 [S3-13]
				}
				showPopupThumb4("已同步到剪贴板", name, hint, saved, 5000, dib, imgW, imgH)
			} else {
				showPopup4("剪贴板写入失败", name, "剪贴板被其他程序长期占用", saved, 5000)
			}
		})
	}
	// [Stage 10] 设备目录名：<设备名>-<指纹前8位>
	// 手机端会收到这个值并显示出来，用户据此知道「我这台手机的图在电脑哪个目录」。
	// 设备名从已配对设备记录取（配对成功后 upsertDevice 已入库；重连时更是现成的）。
	devName := senderHello.DevName
	if d, known := deviceByFP(senderFP); known && d.DevName != "" {
		devName = d.DevName
	}
	devDir := deviceDirName(devName, senderFP)
	logf("设备目录: %s", devDir)
	messageLoop(conn, keys.Session, senderFP, devDir, onFile)
}

// abortConn：把连接截止时间推到过去 → 阻塞中的读立即返回
// （用于『不一致』/ 配对超时后立即断开，不等 45 秒帧超时）
func abortConn(conn net.Conn) {
	_ = conn.SetDeadline(timeNow().Add(-time.Second))
}

func remoteOf(conn net.Conn) string { return conn.RemoteAddr().String() }

// ---------- 消息循环与文件接收（协议 §4/§5） ----------

type rxFile struct {
	fileID   string
	name     string
	origName string // [PC-4] 手机端原始文件名（弹窗显示用）
	size     int64
	part     *os.File
	hasher   hash.Hash
	written  int64
	skip     bool   // 幂等命中：吞掉剩余块
	relPath  string // [Stage 7] 清洗后的相对目录（落盘子目录），空 = 落在接收目录根
	devDir   string // [Stage 10] 设备目录名（<设备名>-<fp8>），多机隔离用
	senderFP string // [Stage 10] 发送方指纹（幂等键的设备维度）
}

func messageLoop(conn net.Conn, key []byte, senderFP string, devDir string, onFileReceived func(string, string, string)) {
	var rx *rxFile
	for {
		conn.SetDeadline(timeNow().Add(frameTimeout))
		ft, pt, err := readEncFrame(conn, key)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				logf("45 秒无入站帧，断开")
			} else {
				logf("连接结束: %v", err)
			}
			if rx != nil && rx.part != nil {
				rx.part.Close()
			}
			return
		}
		switch ft {
		case FrameFileData:
			if rx != nil && !rx.skip && rx.part != nil {
				// [修复] 写错误不能吞：磁盘写满/IO 错误此前被静默忽略，直到最后
				// 哈希不匹配才报 transient（用户的图已经丢了一半才发现）。
				// 现在写失败即按 transient 报错并断开，让手机端重试。
				if _, werr := rx.part.Write(pt); werr != nil {
					logErr("写临时文件失败: %v", werr)
					sendCtrl(conn, key, map[string]any{
						"type": "ack", "file_id": rx.fileID,
						"status": "error", "reason": "io_error", "err_class": "transient",
					})
					notifyRecvFailure("io_error", "写入接收目录失败: "+werr.Error())
					rx.part.Close()
					os.Remove(rx.part.Name())
					rx = nil
					return
				}
				rx.written += int64(len(pt))
				rx.hasher.Write(pt)
			}
		case FrameCtrlJSON:
			var base struct {
				Type string `json:"type"`
			}
			if parseJSONInto(pt, &base) != nil {
				return
			}
			switch base.Type {
			case "ping":
				sendCtrl(conn, key, map[string]any{"type": "pong"})
			case "bye":
				return
			case "file_meta":
				var meta struct {
					Type            string `json:"type"`
					FileID          string `json:"file_id"`
					Name            string `json:"name"`
					Size            int64  `json:"size"`
					Mime            string `json:"mime"`
					CapturedAt      string `json:"captured_at"`
					BatchID         string `json:"batch_id"`
					IsLatestInBatch bool   `json:"is_latest_in_batch"`
					BatchSize       int    `json:"batch_size"`
					RelPath         string `json:"rel_path"` // [Stage 7] 手机端相对目录
				}
				if parseJSONInto(pt, &meta) != nil {
					return
				}
				// [修复] 上一个文件还没 file_end 就又来 file_meta：旧实现直接覆盖 rx，
				// 旧 part 句柄泄漏（文件一直开到进程结束）、旧 hasher 里的数据丢弃。
				// 协议 §5.1 要求按状态机处理——这里收掉旧句柄记违规日志后继续，
				// 不断连接（单帧乱序不值得撕整条连接），但绝不留泄漏。
				if rx != nil && rx.part != nil {
					logf("协议违规：上一个文件未 file_end 即收到新 file_meta，收掉旧临时文件")
					rx.part.Close()
					os.Remove(rx.part.Name())
					rx = nil
				}
				rx = handleFileMeta(conn, key, senderFP, devDir, &meta)
			case "file_end":
				if rx == nil {
					return
				}
				// [L1] 协议 §5.1：file_end 带 file_id —— 必须与当前接收中的文件比对。
				// 此前 PC 完全忽略它：手机在「未 file_end 又来 file_meta」被 PC 收掉旧 part 之后，
				// 再补发**旧文件**的 file_end → 会把**新文件**的 rx.part 关掉并 finalize
				// （哈希不匹配 → 新图被删）。不一致就丢弃这条 file_end 并记违规日志。
				var fe struct {
					Type   string `json:"type"`
					FileID string `json:"file_id"`
				}
				if parseJSONInto(pt, &fe) != nil {
					return
				}
				// rx.fileID 为空 = 该文件已在 file_meta 阶段被拒（file_id 非法）→ 没有可比对的基准
				if rx.fileID != "" && fe.FileID != "" && !strings.EqualFold(fe.FileID, rx.fileID) {
					logWarn("协议违规：file_end 的 file_id 与当前文件不一致（收到 %s，期望 %s）→ 丢弃该 file_end",
						fe.FileID, rx.fileID)
					continue
				}
				rx.part.Close()
				if rx.skip {
					rx = nil
					continue
				}
				finalizeFile(conn, key, rx, onFileReceived)
				rx = nil
			case "error":
				logf("对端 error: %s", string(pt))
			}
		}
	}
}

func handleFileMeta(conn net.Conn, key []byte, senderFP string, devDir string, meta *struct {
	Type            string `json:"type"`
	FileID          string `json:"file_id"`
	Name            string `json:"name"`
	Size            int64  `json:"size"`
	Mime            string `json:"mime"`
	CapturedAt      string `json:"captured_at"`
	BatchID         string `json:"batch_id"`
	IsLatestInBatch bool   `json:"is_latest_in_batch"`
	BatchSize       int    `json:"batch_size"`
	RelPath         string `json:"rel_path"` // [Stage 7] 手机端相对目录
}) *rxFile {
	sendAck := func(status, reason, class string) {
		sendCtrl(conn, key, map[string]any{
			"type": "ack", "file_id": meta.FileID, "status": status,
			"reason": reason, "err_class": class,
		})
	}
	// 幂等检查（第一优先）[R-15]
	// [Stage 10] 幂等键含设备维度：两台手机传同一张图（内容相同 → file_id 相同）时
	// 各存一份，互不吞；否则第二台手机的那张图会被静默丢弃（还回「已送达」的成功回执）。
	// [修复·安全] file_id 必须**先校验**再用：协议声称它是 SHA-256 hex，但此前 PC 端
	// 从不验证格式，把它直接拼进临时文件路径（os.Create(saveDir/.tmp/<file_id>.part)）。
	// 恶意对端发 file_id="..\\..\\x" 就能把 .part 写到接收目录之外（filepath.Join 会
	// 解析 .. 逃逸），失败路径的 os.Remove(rx.part.Name()) 又变成任意 .part 删除。
	// 协议 §5.2 对 name/rel_path/dev_name 都有消毒要求，唯独 file_id 裸奔——这里补上。
	if !isValidFileID(meta.FileID) {
		sendAck("error", "invalid_file_id", "permanent")
		logErr("file_id 非法（应为 64 位十六进制）: %q", meta.FileID)
		return &rxFile{fileID: "", skip: true}
	}
	seenKey := seenKeyFor(senderFP, meta.FileID)
	seenMu.Lock()
	dup := seenIDs[seenKey]
	seenMu.Unlock()
	if dup {
		sendAck("ok", "duplicate", "permanent")
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	// 文件名消毒 + 扩展名白名单（§5.2）
	clean, err := sanitizeName(meta.Name)
	if err != nil {
		// [PC-10] 协议 §5.2:198：白名单外 → reason=ext_rejected（原为 invalid_name）
		sendAck("error", "ext_rejected", "permanent")
		logErr("文件名被拒: %q（%v）", meta.Name, err)
		notifyRecvFailure("ext_rejected", "文件名不在白名单: "+meta.Name)
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	if meta.Size > maxFileSize {
		sendAck("error", "file_too_large", "permanent")
		logErr("文件过大被拒: %s（%d 字节 > 上限 %d）", meta.Name, meta.Size, maxFileSize)
		notifyRecvFailure("file_too_large", fmt.Sprintf("文件超过 %d MB 上限: %s", maxFileSize>>20, meta.Name))
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	if meta.Size <= 0 {
		// 0 字节 = 发送端未就绪/空文件 [P0-02]，一律拒绝
		sendAck("error", "empty_file", "permanent")
		logErr("空文件被拒: %s", meta.Name)
		notifyRecvFailure("empty_file", "手机发来空文件: "+meta.Name)
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	// 磁盘配额（第二优先）[R-15]
	if free, err := diskFreeBytes(getSaveDir()); err == nil && free < 500<<20 {
		sendAck("error", "disk_full", "permanent")
		logErr("磁盘空间不足，拒收: %s（剩余 %d MB）", meta.Name, free>>20)
		notifyRecvFailure("disk_full", fmt.Sprintf("磁盘剩余不足 500 MB（当前 %d MB）", free>>20))
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	// 临时文件写在接收目录根的 .tmp 下（不进子目录——子目录还没建好）。
	// [修复] 文件名加**发送方指纹前 8 位**前缀：v1.9 的幂等键已有设备维度，但 .part
	// 临时名此前是裸 file_id——两台手机传同一张图（内容相同 → file_id 相同）时，
	// 第二个 os.Create 会截断第一个连接正在写的同一文件，字节交错 → 双方哈希不匹配、
	// 传输失败重试（恰是 v1.9 主打的「多机同图」场景）。
	partName := senderFP[:8] + "-" + meta.FileID + ".part"
	part, err := os.Create(filepath.Join(getSaveDir(), ".tmp", partName))
	if err != nil {
		sendAck("error", "io_error", "transient")
		logErr("创建临时文件失败: %v", err)
		notifyRecvFailure("io_error", "无法写入接收目录: "+err.Error())
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	// [Stage 7] 手机端目录结构保留：按 rel_path 建子目录。
	// rel_path 来自对端（不可信）→ 必须经 sanitizeRelPath 清洗 + resolveSaveSubDir 二次逃逸检查。
	saveDir := getSaveDir()
	subDir, relSub := resolveSaveSubDir(saveDir, meta.RelPath)
	if relSub != "" {
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			// 建目录失败 → 落平到接收目录根（不能因此拒收整张图）
			logErr("创建子目录失败，退回接收目录根: %s（%v）", relSub, err)
			subDir, relSub = saveDir, ""
			part.Close()
			os.Remove(part.Name())
			sendAck("error", "io_error", "transient")
			notifyRecvFailure("io_error", "无法创建子目录: "+err.Error())
			return &rxFile{fileID: meta.FileID, skip: true}
		}
	}
	dirLabel := relSub
	if dirLabel == "" {
		dirLabel = "<root>"
	}
	logf("接收文件: %s (%d bytes) batch=%s dir=%s", clean, meta.Size, meta.BatchID, dirLabel)
	return &rxFile{fileID: meta.FileID, name: clean, origName: meta.Name, size: meta.Size, part: part,
		hasher: sha256.New(), relPath: relSub, devDir: devDir, senderFP: senderFP}
}

// notifyRecvFailure：接收失败的用户提示 —— 合并同类 + 限流 [PC-6]
// 规则：错误类型变化时立即提示；同类错误 10 秒内只提示一次；一次成功后重置
var recvFailState struct {
	mu      sync.Mutex
	lastKey string
	lastAt  time.Time
}

func notifyRecvFailure(reason, detail string) {
	recvFailState.mu.Lock()
	now := timeNow()
	dup := reason == recvFailState.lastKey && now.Sub(recvFailState.lastAt) < 10*time.Second
	if !dup {
		recvFailState.lastKey = reason
		recvFailState.lastAt = now
	}
	recvFailState.mu.Unlock()
	if dup {
		return // 同类限流：只记日志，不打扰用户
	}
	title, l1, l2 := recvFailureText(reason, detail)
	showPopup(title, l1, l2, 6000)
}

// resetRecvFailState：成功收图后清空限流状态（下次出错会重新提示）
func resetRecvFailState() {
	recvFailState.mu.Lock()
	recvFailState.lastKey = ""
	recvFailState.mu.Unlock()
}

// recvFailureText：ACK reason → 用户可读文案
func recvFailureText(reason, detail string) (title, l1, l2 string) {
	switch reason {
	case "ext_rejected":
		return "接收失败：文件名被拒", "只接受 jpg/png/webp/gif/bmp/heic", detail
	case "file_too_large":
		return "接收失败：文件过大", "单文件超过 200 MB 上限", detail
	case "disk_full":
		return "接收失败：磁盘空间不足", "接收目录剩余空间不足 500 MB", detail
	case "io_error":
		return "接收失败：写入出错", "无法写入接收目录（权限/路径问题）", detail
	case "empty_file":
		return "接收失败：空文件", "手机发来 0 字节文件", detail
	default:
		return "接收失败", reason, detail
	}
}

func finalizeFile(conn net.Conn, key []byte, rx *rxFile, onFileReceived func(string, string, string)) {
	sum := rx.hasher.Sum(nil)
	fileID := fmt.Sprintf("%x", sum)
	finalize := func(status, reason, class string) {
		sendCtrl(conn, key, map[string]any{
			"type": "ack", "file_id": rx.fileID, "status": status,
			"reason": reason, "err_class": class,
		})
	}
	if !strings.EqualFold(fileID, rx.fileID) {
		rx.part.Close()
		os.Remove(rx.part.Name())
		finalize("error", "hash_mismatch", "transient")
		logErr("哈希不匹配: %s", rx.name)
		notifyRecvFailure("hash_mismatch", "文件校验失败（可能传输损坏）: "+rx.name)
		return
	}
	// [Stage 7] 落在手机端对应的子目录（relPath 空 = 接收目录根）
	// [Stage 10] **设备目录作为第一级**：Received\<设备名>-<fp8>\<相册目录>\
	//             多台手机同步到同一台 PC 时物理隔离，用户一眼分得出哪张图来自哪台手机。
	saveDir := getSaveDir()
	devDirPath := resolveDeviceDir(saveDir, rx.devDir)
	destDir := devDirPath
	if rx.relPath != "" {
		destDir = filepath.Join(devDirPath, rx.relPath)
	}
	// 设备目录 / 子目录可能还不存在 → 建出来
	if destDir != saveDir {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			rx.part.Close()
			os.Remove(rx.part.Name())
			finalize("error", "io_error", "transient")
			logErr("创建设备/子目录失败: %v", err)
			notifyRecvFailure("io_error", "无法创建目录: "+err.Error())
			return
		}
	}
	finalName := fmt.Sprintf("%d_%s", timeNow().UnixMilli(), rx.name)
	dest := filepath.Join(destDir, finalName)
	// 落盘名冲突加 -1/-2 后缀（协议 §5.2）[P2-06]
	for i := 1; fileExists(dest); i++ {
		ext := filepath.Ext(rx.name)
		base := strings.TrimSuffix(rx.name, ext)
		dest = filepath.Join(destDir, fmt.Sprintf("%d_%s-%d%s", timeNow().UnixMilli(), base, i, ext))
	}
	if err := os.Rename(rx.part.Name(), dest); err != nil {
		os.Remove(rx.part.Name()) // 残留清理 [P2-07]
		finalize("error", "io_error", "transient")
		logErr("rename 失败: %v", err)
		notifyRecvFailure("io_error", "落盘失败: "+err.Error())
		return
	}
	addSeenID(seenKeyFor(rx.senderFP, rx.fileID))
	finalize("ok", "ok", "permanent")
	logf("落盘完成: %s", dest)
	resetRecvFailState() // 成功一次 → 清掉限流状态（下次出错重新提示）[PC-6]
	if onFileReceived != nil {
		onFileReceived(dest, rx.origName, destDir)
	}
}

// ---------- 工具 ----------

// isValidFileID：协议 §5.1 规定 file_id = SHA-256(content) 的十六进制串（64 字符）。
// 只允许十六进制字符 → 天然免疫路径分隔符、..、绝对路径等一切穿越把戏
// （比「黑名单过滤危险字符」更可靠：白名单之外的输入一律拒绝）。
func isValidFileID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func sendCtrl(conn net.Conn, key []byte, v any) error {
	b, err := jsonMarshal(v)
	if err != nil {
		return err
	}
	return writeEncFrame(conn, key, FrameCtrlJSON, b)
}

func sanitizeName(name string) (string, error) {
	base := name
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.Map(func(r rune) rune {
		if r < 32 || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return -1
		}
		return r
	}, base)
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." {
		return "", errors.New("文件名非法")
	}
	// [修复] Windows 保留设备名（CON/NUL/COM1…）与超长名：手机发「CON.jpg」或
	// 300 字符文件名时落盘必然失败，旧实现按 io_error/transient **无限重试**、
	// 永不 dropped，队列被僵尸项占满。这里直接判 permanent 拒绝。
	if ext := filepath.Ext(base); ext != "" {
		if reservedNames[strings.ToUpper(strings.TrimSuffix(base, ext))] {
			return "", fmt.Errorf("%q 是 Windows 保留设备名", base)
		}
	}
	if reservedNames[strings.ToUpper(base)] {
		return "", fmt.Errorf("%q 是 Windows 保留设备名", base)
	}
	// [L2] 上限按**字符（rune）**计，不是字节：文案写的是「200 字符」，
	// 旧实现用 len()（字节数）→ 中文文件名约 66 字就被拒，与文案口径不符；
	// 也与 relpath.go 的 maxPathSegment（已改成 rune 计数）不一致。
	if utf8.RuneCountInString(base) > 200 {
		return "", errors.New("文件名超过 200 字符上限")
	}
	ext := strings.ToLower(filepath.Ext(base))
	wl := map[string]bool{"jpg": true, "jpeg": true, "png": true, "webp": true, "gif": true, "bmp": true, "heic": true}
	if !wl[strings.TrimPrefix(ext, ".")] {
		return "", fmt.Errorf("扩展名 %q 不在白名单", ext)
	}
	return base, nil
}

func diskFreeBytes(path string) (uint64, error) {
	var free uint64
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return free, windows.GetDiskFreeSpaceEx(p, nil, nil, &free)
}
