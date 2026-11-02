package main

// 协议基础：JSON 工具、transcript、SAS、帧加解密 — 协议 §3.1/§3.2/§4

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------- JSON 小工具 ----------

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func parseJSON[T any](b []byte) (T, error) {
	var v T
	err := json.Unmarshal(b, &v)
	return v, err
}

func parseJSONInto(b []byte, v any) error { return json.Unmarshal(b, v) }

// ---------- HELLO / transcript（协议 §3.1 [P0-01][N-03]） ----------

type Hello struct {
	PB          int    `json:"pb"`
	Type        string `json:"type"`
	Ver         int    `json:"ver"`
	EdPub       string `json:"ed_pub"`         // b64 std
	X25519IDPub string `json:"x25519_id_pub"`  // b64 std
	X25519Eph   string `json:"x25519_eph_pub"` // b64 std
	Nonce       string `json:"nonce"`          // b64 std 16B
	DevName     string `json:"dev_name"`
	Role        string `json:"role"` // "sender"|"receiver"
}

// 单条 HELLO 的规范化字段项（协议 §3.1 表格），按字段名字典序
func helloFields(h *Hello) (items []byte, err error) {
	roleByte := byte(0x01)
	if h.Role == "sender" {
		roleByte = 0x00
	}
	ed, e1 := b64decode(h.EdPub)
	xid, e2 := b64decode(h.X25519IDPub)
	eph, e3 := b64decode(h.X25519Eph)
	nc, e4 := b64decode(h.Nonce)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return nil, errors.New("hello base64 字段非法")
	}
	if len(ed) != 32 || len(xid) != 32 || len(eph) != 32 || len(nc) != 16 {
		return nil, errors.New("hello 字段长度非法")
	}
	put := func(name string, val []byte) {
		var lb [4]byte
		nb := []byte(name)
		binary.BigEndian.PutUint32(lb[:], uint32(len(nb)))
		items = append(items, lb[:]...)
		items = append(items, nb...)
		var vb [4]byte
		binary.BigEndian.PutUint32(vb[:], uint32(len(val)))
		items = append(items, vb[:]...)
		items = append(items, val...)
	}
	put("dev_name", []byte(h.DevName))
	put("ed_pub", ed)
	put("nonce", nc)
	put("role", []byte{roleByte})
	var vb [4]byte
	binary.BigEndian.PutUint32(vb[:], uint32(h.Ver))
	put("ver", vb[:])
	put("x25519_eph_pub", eph)
	put("x25519_id_pub", xid)
	return items, nil
}

// transcript = SHA-256( [0x01][Sender 字段项] || [0x00][Receiver 字段项] ) — 协议 §3.1 [N-03]
func transcriptHash(senderHello, receiverHello *Hello) ([]byte, error) {
	si, err := helloFields(senderHello)
	if err != nil {
		return nil, err
	}
	ri, err := helloFields(receiverHello)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, 1+len(si)+1+len(ri))
	buf = append(buf, 0x01)
	buf = append(buf, si...)
	buf = append(buf, 0x00)
	buf = append(buf, ri...)
	h := sha256.Sum256(buf)
	return h[:], nil
}

// ---------- 密钥协商与 SAS（协议 §3.1 [N-01][P0-02]） ----------

type SessionKeys struct {
	Session []byte // 32B，AES-256-GCM
	SAS     string // 6 位数字
}

// shared = X25519(本端临时私钥, 对端临时公钥) — ephemeral×ephemeral，双方必相等 [N-01]
func deriveSession(
	ephPriv *ecdh.PrivateKey, peerEphPubRaw []byte,
	senderEphPubRaw, receiverEphPubRaw, senderNonce, receiverNonce []byte,
) (*SessionKeys, error) {
	peerPub, err := ecdh.X25519().NewPublicKey(peerEphPubRaw)
	if err != nil {
		return nil, fmt.Errorf("对端临时公钥非法: %w", err)
	}
	shared, err := ephPriv.ECDH(peerPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH 失败: %w", err)
	}
	session, err := hkdf.Key(sha256.New, shared, append(append([]byte{}, senderNonce...), receiverNonce...),
		"photobeam-session-v1", 32)
	if err != nil {
		return nil, err
	}
	// SAS salt 按发送方视角固定顺序（协议 §3.1）：sender_eph‖receiver_eph‖sender_nonce‖receiver_nonce
	sasSalt := make([]byte, 0, 32+32+16+16)
	sasSalt = append(sasSalt, senderEphPubRaw...)
	sasSalt = append(sasSalt, receiverEphPubRaw...)
	sasSalt = append(sasSalt, senderNonce...)
	sasSalt = append(sasSalt, receiverNonce...)
	sasBytes, err := hkdf.Key(sha256.New, shared, sasSalt, "photobeam-sas-v1", 4)
	if err != nil {
		return nil, err
	}
	sasNum := binary.BigEndian.Uint32(sasBytes) % 1000000
	return &SessionKeys{Session: session, SAS: fmt.Sprintf("%06d", sasNum)}, nil
}

// ---------- 加密帧（协议 §4 [P0-05][P0-06]） ----------

const (
	FrameCtrlJSON = 0x01
	FrameFileData = 0x02
)

var errFrameTooLarge = errors.New("帧超限")

func sealFrame(key, aadTail []byte, frameType byte, plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxFrameLen {
		return nil, errFrameTooLarge
	}
	nonce := randomBytes(12)
	// AAD = [u8 frame_type][u32 BE payload_len][12B nonce]
	head := make([]byte, 0, 1+4+12)
	head = append(head, frameType)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(plaintext)))
	head = append(head, lb[:]...)
	head = append(head, nonce...)
	aead, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(aead)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, head)
	out := make([]byte, 0, 1+4+12+len(ct))
	out = append(out, frameType)
	out = append(out, lb[:]...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

func openFrame(key []byte, frame []byte) (frameType byte, plaintext []byte, err error) {
	if len(frame) < 1+4+12+16 {
		return 0, nil, errors.New("帧过短")
	}
	ft := frame[0]
	plen := binary.BigEndian.Uint32(frame[1:5])
	nonce := frame[5:17]
	ct := frame[17:]
	if int(plen) > MaxFrameLen || len(ct) != int(plen)+16 {
		return 0, nil, errors.New("帧长度非法")
	}
	head := frame[:17]
	aead, err := aes.NewCipher(key)
	if err != nil {
		return 0, nil, err
	}
	gcm, err := cipher.NewGCM(aead)
	if err != nil {
		return 0, nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, head)
	if err != nil {
		return 0, nil, fmt.Errorf("解密失败: %w", err)
	}
	return ft, pt, nil
}

// 常量时间比较（SAS/签名校验用）
func ctEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
