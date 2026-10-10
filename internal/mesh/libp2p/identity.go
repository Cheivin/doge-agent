//go:build mesh

package libp2p

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
)

// meshKeyPath 返回 mesh.json 旁边的节点身份密钥路径。密钥刻意不在 mesh.json 里：
// mesh.json（及其 token）跨机器共享，但每个节点必须有自己的身份——两个节点同
// PeerID 会冲突。把 mesh.json 拷到另一台机器会在那边生成全新身份
func meshKeyPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "mesh-key")
}

// loadOrCreateKey 加载持久化的 libp2p 身份，或生成并持久化新的 Ed25519 密钥
// （0600）。稳定的 PeerID 让 token 内嵌的 bootstrap 地址跨进程/重启有效：
// 种子节点同时保持密钥与监听端口，重启后 peer 仍可按原地址重新加入
func loadOrCreateKey(configPath string) (crypto.PrivKey, error) {
	path := meshKeyPath(configPath)
	if data, err := os.ReadFile(path); err == nil {
		key, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("mesh key %s: %w", path, err)
		}
		return key, nil
	}
	key, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		return nil, fmt.Errorf("generate mesh key: %w", err)
	}
	data, err := crypto.MarshalPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
