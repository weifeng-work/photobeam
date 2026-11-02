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

	"golang.org/x/sys/windows"
)

const (
	handshakeTimeout = 15 * time.Second
	frameTimeout     = 45 * time.Second
	plaintextMax     = MaxPlaintextFrame
	maxFileSize      = 200 << 20 // 单文件上限 200MB（超出即断）
)

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

func startTCPServer() error {
	var ln net.Listener
	var err error
	for port := TCPPortBase; port <= TCPPortMax; port++ {
		ln, err = net.Listen("tcp4", fmt.Sprintf(":%d", port))
		if err == nil {
			cfg.TCPPort = port
			saveConfig(cfg)
			logf("TCP 监听 :%d", port)
			break
		}
	}
	if err != nil {
		return errors.New("TCP 端口 5416-5436 全部被占")
	}
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				logf("accept 失败: %v", e)
				return
			}
			go goroutineSafe("handleConn", func() { handleConn(conn) })
		}
	}()
	return nil
}

func handleConn(conn net.Conn) {
	defer conn.Close()
	logf("连接进入: %s", conn.RemoteAddr().String())

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
	if findDevice(senderFP) != nil {
		mode = "reconnect"
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
	authAttempts := 0
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
			dev := findDevice(senderFP)
			logf("重连验签: fp=%s dev=%v 结果=%v", senderFP, dev != nil,
				dev != nil && ed25519.Verify(ed25519.PublicKey(edRaw), th, mustDecode(m.Sig)))
			if dev != nil && ed25519.Verify(ed25519.PublicKey(edRaw), th, mustDecode(m.Sig)) {
				okSig := ed25519.Sign(identity.EdPriv, th)
				if err := sendCtrl(conn, keys.Session, map[string]any{"type": "auth_ok", "sig": b64encode(okSig)}); err != nil {
					return
				}
				ip, portStr, _ := net.SplitHostPort(remoteOf(conn))
				var pn int
				fmt.Sscanf(portStr, "%d", &pn)
				dev.LastIP, dev.LastPort = ip, cfg.TCPPort // last_port = PC 自身监听端口 [P1-01]
				saveConfig(cfg)
				logf("重连认证通过: %s", dev.DevName)
				authStats.ok()
				break
			}
			// 验签失败（pin 过期/伪造）→ 降级 pairing，SAS 人眼比对重建
			authStats.fail("重连验签失败（对端身份不匹配）")
			if authAttempts >= 1 {
				logf("重连认证失败且已降级过，断开")
				return
			}
			authAttempts++
			mode = "pairing"
			modeFrame, _ := jsonMarshalIndent(map[string]any{
				"pb": 1, "type": "mode", "mode": "pairing", "fp": identity.FP(), "reset": true,
			})
			if err := writePlainFrame(conn, modeFrame); err != nil {
				return
			}
			continue
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
		var pairingDecided int32 // 0=进行中 1=已决（原子：与主流程互斥）
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
					return
				}
				switch ev {
				case pairEvMatch:
					// 点『一致』只关窗 —— PC 侧仍以程序比对 SAS 为准（绝不跳过校验）
					logf("用户已核对 SAS（一致），继续等待手机 pair_confirm")
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
					trayNotify("配对已拒绝", "你在电脑上点了『不一致』，已断开连接")
					messageBox("你在电脑上点了『不一致』，已拒绝本次配对。\n"+
						"连接已断开。请确认两端显示的 6 位数字确实不同，再重新配对。",
						"PhotoBeam 配对已拒绝")
				case pairEvTimeout:
					if !atomic.CompareAndSwapInt32(&pairingDecided, 0, 1) {
						return
					}
					logf("配对超时：%d 秒内未收到手机确认，已断开", pairTimeoutMs/1000)
					abortConn(conn)
					trayNotify("配对超时", fmt.Sprintf("%d 秒内未收到手机确认，连接已断开", pairTimeoutMs/1000))
					messageBox(fmt.Sprintf("%d 秒内没有收到手机的确认，配对已取消。\n"+
						"请重新点手机上的「② 配对电脑」，并在电脑上完成核对。", pairTimeoutMs/1000),
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
			finish()
			return
		}
		pw.close() // 收到 pair_confirm → 关闭向导窗
		if !ctEqual(m.SAS, keys.SAS) || !ed25519.Verify(ed25519.PublicKey(edRaw), th, mustDecode(m.Sig)) {
			logf("配对校验失败（SAS 或签名），静默断开")
			authStats.fail("配对校验失败（SAS 或签名不一致）")
			return
		}
		authStats.ok()
		okSig := ed25519.Sign(identity.EdPriv, th)
		if err := sendCtrl(conn, keys.Session, map[string]any{"type": "pair_ok", "sig": b64encode(okSig)}); err != nil {
			return
		}
		ip, portStr, _ := net.SplitHostPort(remoteOf(conn))
		var pn int
		fmt.Sscanf(portStr, "%d", &pn)
		upsertDevice(Device{FP: senderFP, EdPub: senderHello.EdPub, XIDPub: senderHello.X25519IDPub,
			DevName: firstNonEmpty(m.DevName, senderHello.DevName), LastIP: ip, LastPort: cfg.TCPPort})
		trayNotify("配对成功", "已与 "+firstNonEmpty(m.DevName, senderHello.DevName)+" 完成配对")
		logf("配对成功: %s (%s)", m.DevName, senderFP)
		break
	}

	// 消息循环（文件接收）；落盘成功 → 剪贴板+弹窗（异步 [MiniMax 坑 6]）
	logf("进入消息循环")
	// [PC-4] 弹窗显示**手机端原始文件名** + 已保存目录（落盘名带毫秒前缀，用户对不上）
	onFile := func(destPath, origName, saveDir string) {
		go goroutineSafe("clipboard+popup", func() {
			if !cfg.ClipboardEnabled {
				return
			}
			dib, pngData, imgW, imgH, err := prepareImageData(destPath)
			if err != nil {
				logf("图片解码失败: %v", err)
				return
			}
			initClipboardFormats()
			// 写入成功才弹窗承诺 [F-03]：失败只弹"写入失败"提示，绝不假承诺可粘贴
			ok, snapNil := writeClipboardWithRestore(dib, pngData, cfg.ClipboardSeconds, nil)
			name := origName
			if name == "" {
				name = filepath.Base(destPath)
			}
			saved := "已保存到 " + ellipsisTail(saveDir, 34) // [PC-4] 路径过长时保留尾部
			if ok {
				hint := "Ctrl+V 可粘贴 · " + fmt.Sprint(cfg.ClipboardSeconds) + " 秒后还原"
				if snapNil {
					hint = "原内容无可还原，本次不还原" // 中性表述：真因多为上一次同步的图 [S3-13]
				}
				showPopupThumb4("已同步到剪贴板", name, hint, saved, 5000, dib, imgW, imgH)
			} else {
				showPopup4("剪贴板写入失败", name, "剪贴板被其他程序长期占用", saved, 5000)
			}
		})
	}
	messageLoop(conn, keys.Session, senderFP, onFile)
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
	skip     bool // 幂等命中：吞掉剩余块
}

func messageLoop(conn net.Conn, key []byte, senderFP string, onFileReceived func(string, string, string)) {
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
				rx.part.Write(pt)
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
				}
				if parseJSONInto(pt, &meta) != nil {
					return
				}
				rx = handleFileMeta(conn, key, &meta)
			case "file_end":
				if rx == nil {
					return
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

func handleFileMeta(conn net.Conn, key []byte, meta *struct {
	Type            string `json:"type"`
	FileID          string `json:"file_id"`
	Name            string `json:"name"`
	Size            int64  `json:"size"`
	Mime            string `json:"mime"`
	CapturedAt      string `json:"captured_at"`
	BatchID         string `json:"batch_id"`
	IsLatestInBatch bool   `json:"is_latest_in_batch"`
	BatchSize       int    `json:"batch_size"`
}) *rxFile {
	sendAck := func(status, reason, class string) {
		sendCtrl(conn, key, map[string]any{
			"type": "ack", "file_id": meta.FileID, "status": status,
			"reason": reason, "err_class": class,
		})
	}
	// 幂等检查（第一优先）[R-15]
	seenMu.Lock()
	dup := seenIDs[meta.FileID]
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
	saveDir := getSaveDir()
	if free, err := diskFreeBytes(saveDir); err == nil && free < 500<<20 {
		sendAck("error", "disk_full", "permanent")
		logErr("磁盘空间不足，拒收: %s（剩余 %d MB）", meta.Name, free>>20)
		notifyRecvFailure("disk_full", fmt.Sprintf("磁盘剩余不足 500 MB（当前 %d MB）", free>>20))
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	part, err := os.Create(filepath.Join(saveDir, ".tmp", meta.FileID+".part"))
	if err != nil {
		sendAck("error", "io_error", "transient")
		logErr("创建临时文件失败: %v", err)
		notifyRecvFailure("io_error", "无法写入接收目录: "+err.Error())
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	logf("接收文件: %s (%d bytes) batch=%s", clean, meta.Size, meta.BatchID)
	return &rxFile{fileID: meta.FileID, name: clean, origName: meta.Name, size: meta.Size, part: part,
		hasher: sha256.New()}
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
	saveDir := getSaveDir()
	finalName := fmt.Sprintf("%d_%s", timeNow().UnixMilli(), rx.name)
	dest := filepath.Join(saveDir, finalName)
	// 落盘名冲突加 -1/-2 后缀（协议 §5.2）[P2-06]
	for i := 1; fileExists(dest); i++ {
		ext := filepath.Ext(rx.name)
		base := strings.TrimSuffix(rx.name, ext)
		dest = filepath.Join(saveDir, fmt.Sprintf("%d_%s-%d%s", timeNow().UnixMilli(), base, i, ext))
	}
	if err := os.Rename(rx.part.Name(), dest); err != nil {
		os.Remove(rx.part.Name()) // 残留清理 [P2-07]
		finalize("error", "io_error", "transient")
		logErr("rename 失败: %v", err)
		notifyRecvFailure("io_error", "落盘失败: "+err.Error())
		return
	}
	addSeenID(rx.fileID)
	finalize("ok", "ok", "permanent")
	logf("落盘完成: %s", dest)
	resetRecvFailState() // 成功一次 → 清掉限流状态（下次出错重新提示）[PC-6]
	if onFileReceived != nil {
		onFileReceived(dest, rx.origName, saveDir)
	}
}

// ---------- 工具 ----------

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
