package main

// 身份密钥（Ed25519 签名 + X25519 身份 KEM）与指纹 — 协议 §1

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"golang.org/x/crypto/curve25519"
)

type Identity struct {
	EdPriv  ed25519.PrivateKey
	EdPub   ed25519.PublicKey
	XIDPriv [32]byte // X25519 身份私钥（仅用于指纹与设备绑定证明，不参与会话密钥 — 协议 §1）
	XIDPub  [32]byte
}

// fp = hex(SHA-256(ed25519_pub_raw || x25519_id_pub_raw))[:16] — 协议 §1 [R-25]
func fingerprint(edPub ed25519.PublicKey, xidPub [32]byte) string {
	h := sha256.New()
	h.Write(edPub)
	h.Write(xidPub[:])
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func loadOrCreateIdentity() *Identity {
	p := filepath.Join(appDir(), "identity.json")
	type blob struct {
		EdPriv    string `json:"ed_priv"`
		XIDPriv   string `json:"x25519_id_priv"`
	}
	id := &Identity{}

	if b, err := os.ReadFile(p); err == nil {
		var bl blob
		if parseJSONInto(b, &bl) == nil {
			if eb, e := hex.DecodeString(bl.EdPriv); e == nil && len(eb) == ed25519.PrivateKeySize {
				id.EdPriv = ed25519.PrivateKey(eb)
				id.EdPub = id.EdPriv.Public().(ed25519.PublicKey)
			}
			if xb, e := hex.DecodeString(bl.XIDPriv); e == nil && len(xb) == 32 {
				copy(id.XIDPriv[:], xb)
				curve25519.ScalarBaseMult(&id.XIDPub, &id.XIDPriv)
			}
		}
	}
	if id.EdPriv == nil || id.XIDPriv == [32]byte{} {
		id.EdPriv = ed25519.NewKeyFromSeed(randomBytes(32))
		id.EdPub = id.EdPriv.Public().(ed25519.PublicKey)
		copy(id.XIDPriv[:], randomBytes(32))
		curve25519.ScalarBaseMult(&id.XIDPub, &id.XIDPriv)
		bl := blob{
			EdPriv:  hex.EncodeToString(id.EdPriv),
			XIDPriv: hex.EncodeToString(id.XIDPriv[:]),
		}
		b, _ := jsonMarshalIndent(bl)
		tmp := p + ".tmp"
		os.WriteFile(tmp, b, 0o600)
		os.Rename(tmp, p)
		logf("已生成新的设备身份密钥，指纹 %s", fingerprint(id.EdPub, id.XIDPub))
	}
	return id
}

func (id *Identity) FP() string { return fingerprint(id.EdPub, id.XIDPub) }

func randomBytes(n int) []byte {
	b := make([]byte, n)
	cryptoRead(b)
	return b
}
