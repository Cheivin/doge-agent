//go:build mesh

package libp2p

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// 流协议。agent 协议承载长度前缀 JSON Envelope frame（点对点 stream，每条
// stream 一帧或多帧）；握手协议承载一次互验帧；转发流
// （/doge/forward/<name>/0.1）是无 framing 的裸字节管道
const (
	protoAgent     = "/doge/agent/0.1"
	protoHandshake = "/doge/handshake/0.1"
)

// maxFrameBytes 单条 Envelope frame 上限（16 MiB，远大于任何 gzip 压缩的会话快照）
const maxFrameBytes = 16 << 20

// writeFrame 写一条长度前缀 JSON frame 到 w
func writeFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > maxFrameBytes {
		return fmt.Errorf("frame too large: %d bytes", len(data))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// readFrame 从 r 读一条长度前缀 JSON frame 到 v
func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameBytes {
		return fmt.Errorf("frame too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// handshakeMsg /doge/handshake/0.1 上交换的单一 frame。
// Token 为 HMAC-SHA256(psk, remotePeerID)；两侧各用
// HMAC-SHA256(psk, 自身ID) 验证对方
type handshakeMsg struct {
	PeerID string `json:"peerId"`
	Token  []byte `json:"token"`
}
