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
				break
			}
			// 验签失败（pin 过期/伪造）→ 降级 pairing，SAS 人眼比对重建
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
		ft, pt, err := readEncFrame(conn, keys.Session)
		if err != nil {
			logf("读认证帧失败: %v", err)
			return
		}
		if ft != FrameCtrlJSON {
			return
		}
		var m struct {
			Type    string `json:"type"`
			SAS     string `json:"sas"`
			Sig     string `json:"sig"`
			DevName string `json:"dev_name"`
		}
		if parseJSONInto(pt, &m) != nil {
			return
		}
		if m.Type == "pair_reject" {
			trayNotify("手机端已拒绝配对", "两端数字不一致，已断开")
			return
		}
		if m.Type != "pair_confirm" {
			return
		}
		if !ctEqual(m.SAS, keys.SAS) || !ed25519.Verify(ed25519.PublicKey(edRaw), th, mustDecode(m.Sig)) {
			logf("配对校验失败（SAS 或签名），静默断开")
			return
		}
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
	onFile := func(destPath string) {
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
			name := filepath.Base(destPath)
			if ok {
				hint := "Ctrl+V 可粘贴 · " + fmt.Sprint(cfg.ClipboardSeconds) + " 秒后还原"
				if snapNil {
					hint = "原内容无可还原，本次不还原" // 中性表述：真因多为上一次同步的图 [S3-13]
				}
				showPopupThumb("已同步到剪贴板", name, hint, 5000, dib, imgW, imgH)
			} else {
				showPopup("剪贴板写入失败", "图片已落盘", "剪贴板被其他程序长期占用", 5000)
			}
		})
	}
	messageLoop(conn, keys.Session, senderFP, onFile)
}

func remoteOf(conn net.Conn) string { return conn.RemoteAddr().String() }

// ---------- 消息循环与文件接收（协议 §4/§5） ----------

type rxFile struct {
	fileID  string
	name    string
	size    int64
	part    *os.File
	hasher  hash.Hash
	written int64
	skip    bool // 幂等命中：吞掉剩余块
}

func messageLoop(conn net.Conn, key []byte, senderFP string, onFileReceived func(string)) {
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
		sendAck("error", "invalid_name", "permanent")
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	if meta.Size > maxFileSize {
		sendAck("error", "file_too_large", "permanent")
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	if meta.Size <= 0 {
		// 0 字节 = 发送端未就绪/空文件 [P0-02]，一律拒绝
		sendAck("error", "empty_file", "permanent")
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	// 磁盘配额（第二优先）[R-15]
	if free, err := diskFreeBytes(cfg.SaveDir); err == nil && free < 500<<20 {
		sendAck("error", "disk_full", "permanent")
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	part, err := os.Create(filepath.Join(cfg.SaveDir, ".tmp", meta.FileID+".part"))
	if err != nil {
		sendAck("error", "io_error", "transient")
		return &rxFile{fileID: meta.FileID, skip: true}
	}
	logf("接收文件: %s (%d bytes) batch=%s", clean, meta.Size, meta.BatchID)
	return &rxFile{fileID: meta.FileID, name: clean, size: meta.Size, part: part,
		hasher: sha256.New()}
}

func finalizeFile(conn net.Conn, key []byte, rx *rxFile, onFileReceived func(string)) {
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
		logf("哈希不匹配: %s", rx.name)
		return
	}
	finalName := fmt.Sprintf("%d_%s", timeNow().UnixMilli(), rx.name)
	dest := filepath.Join(cfg.SaveDir, finalName)
	// 落盘名冲突加 -1/-2 后缀（协议 §5.2）[P2-06]
	for i := 1; fileExists(dest); i++ {
		ext := filepath.Ext(rx.name)
		base := strings.TrimSuffix(rx.name, ext)
		dest = filepath.Join(cfg.SaveDir, fmt.Sprintf("%d_%s-%d%s", timeNow().UnixMilli(), base, i, ext))
	}
	if err := os.Rename(rx.part.Name(), dest); err != nil {
		os.Remove(rx.part.Name()) // 残留清理 [P2-07]
		finalize("error", "io_error", "transient")
		logf("rename 失败: %v", err)
		return
	}
	addSeenID(rx.fileID)
	finalize("ok", "ok", "permanent")
	logf("落盘完成: %s", dest)
	if onFileReceived != nil {
		onFileReceived(dest)
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
