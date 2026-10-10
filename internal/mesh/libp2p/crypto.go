//go:build mesh

package libp2p

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// 密钥派生与记录加密。传输层已由 libp2p 加密（TLS/Noise 或 PrivateNetwork PSK
// 保护）；这些 helper 提供其上的应用层准入与内容保护：连接时 HMAC 互验、
// advertise 记录 AES-GCM 加密，未持 token 的节点读不出 profile 内容

// deriveKey 从共享 psk 派生用途专属 32 字节密钥：HMAC-SHA256(psk, "mesh-"+purpose)
func deriveKey(psk []byte, purpose string) []byte {
	m := hmac.New(sha256.New, psk)
	m.Write([]byte("mesh-" + purpose))
	return m.Sum(nil)
}

// hmacToken 计算 peer 的互验 token：HMAC-SHA256(psk, peerID)
func hmacToken(psk []byte, peerID string) []byte {
	m := hmac.New(sha256.New, psk)
	m.Write([]byte(peerID))
	return m.Sum(nil)
}

// pskBytes 解码 token 中的 hex psk
func pskBytes(hexPSK string) ([]byte, error) {
	b, err := hex.DecodeString(hexPSK)
	if err != nil {
		return nil, fmt.Errorf("invalid psk: %w", err)
	}
	return b, nil
}

// hmacEqual 握手 token 的常量时间比较
func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }

// encryptRecord 用用途派生密钥 AES-GCM 加密 payload，nonce 前置在密文前
func encryptRecord(psk []byte, purpose string, plain []byte) ([]byte, error) {
	gcm, err := newGCM(psk, purpose)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

// decryptRecord 解密 encryptRecord 的结果
func decryptRecord(psk []byte, purpose string, data []byte) ([]byte, error) {
	gcm, err := newGCM(psk, purpose)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, fmt.Errorf("record too short")
	}
	nonce, ct := data[:ns], data[ns:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}

func newGCM(psk []byte, purpose string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(psk, purpose))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
