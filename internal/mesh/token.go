package mesh

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const pskBytes = 32

// Token mesh 网络凭据，网络内所有节点共享。嵌入 mesh.json（base64 JSON），
// 组建网络的机器之间带外分享
type Token struct {
	// Rendezvous 随机串，派生 mDNS 服务标签与 DHT rendezvous key，隐匿网络
	Rendezvous string `json:"rendezvous"`
	// PSK 共享密钥（hex，32 字节），用于应用层 HMAC 互验与派生 AES-GCM 记录密钥
	PSK string `json:"psk"`
	// BootstrapPeers 种子节点 multiaddr，`doge mesh new --bootstrap ...` 时
	// 嵌入 token，新节点无需带外拿地址；空则用公共 DHT bootstrap
	BootstrapPeers []string `json:"bootstrapPeers,omitempty"`
}

// NewToken 生成全新网络 token；bootstrap 条目做轻校验（须以 / 开头），
// 完整 multiaddr 解析在节点连接时进行
func NewToken(bootstrap ...string) (*Token, error) {
	rv := make([]byte, 16)
	if _, err := rand.Read(rv); err != nil {
		return nil, err
	}
	psk := make([]byte, pskBytes)
	if _, err := rand.Read(psk); err != nil {
		return nil, err
	}
	t := &Token{
		Rendezvous: hex.EncodeToString(rv),
		PSK:        hex.EncodeToString(psk),
	}
	for _, b := range bootstrap {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		if !strings.HasPrefix(b, "/") {
			return nil, fmt.Errorf("invalid bootstrap multiaddr %q: must be a libp2p multiaddr", b)
		}
		t.BootstrapPeers = append(t.BootstrapPeers, b)
	}
	return t, nil
}

// Encode 序列化 token 为 base64 形式
func (t *Token) Encode() string {
	data, err := json.Marshal(t)
	if err != nil {
		// Token 必然可 marshal，此处仅防御
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// DecodeToken 解析 base64 token，拒绝缺 rendezvous 或 psk 的 token
func DecodeToken(s string) (*Token, error) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	if t.Rendezvous == "" || t.PSK == "" {
		return nil, fmt.Errorf("invalid mesh token: missing rendezvous or psk")
	}
	return &t, nil
}
