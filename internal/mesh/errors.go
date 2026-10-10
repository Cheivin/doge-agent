package mesh

import "errors"

var (
	// ErrNoToken 未配置网络 token
	ErrNoToken = errors.New("no mesh token: run `doge mesh new` first")

	// ErrPeerNotConnected 目标 peer 未连接
	ErrPeerNotConnected = errors.New("peer not connected")
)
