//go:build mesh

package libp2p

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"doge/internal/coding/uploads"
	"doge/internal/mesh"
)

// /doge/file/0.1：mesh 节点间文件传输。独立 stream（复用长度前缀 frame 写
// 头帧/尾帧，中间是原始字节），一次传输一条 stream：
//
//	send：发起方 frame(FileHeader) → 原始字节 → 对方校验收货回 frame(FileAck)
//	pull：发起方 frame(FileHeader{Purpose:pull}) → 对方回 FileHeader(ok)+字节 → 发起方回 FileAck
//	list：发起方 frame(FileHeader{Purpose:list}) → 对方回 frame(FileList)
//
// 任何一步失败：send 侧回 FileAck{ok:false}；pull 侧只回 FileAck（无数据）。
const protoFile = "/doge/file/0.1"

// defaultFileMaxBytes 单文件传输上限默认值（mesh.json fileMaxBytes 覆盖）
const defaultFileMaxBytes = 256 << 20

// fileRecvTimeout 接收侧文件流整体期限：大文件经中继链路可能较慢，给足
// 余量；发起侧由自身 ctx（10 分钟）约束
const fileRecvTimeout = 15 * time.Minute

// fileHeader protoFile 的头帧
type fileHeader struct {
	ID       string `json:"id"`                 // 本次传输的随机 ID
	Name     string `json:"name"`               // 清洗后的文件名
	Size     int64  `json:"size"`               // 字节数
	SHA256   string `json:"sha256,omitempty"`   // 发送方计算的哈希（send 用）
	MimeType string `json:"mimeType,omitempty"` // 发送方声明（接收侧仍嗅探）
	// Purpose: "send"（推文件）| "pull"（拉文件）| "list"（列暂存区）
	Purpose  string `json:"purpose"`
	UploadID string `json:"uploadId,omitempty"` // pull/list：目标文件
}

// fileAck protoFile 的尾帧
type fileAck struct {
	OK       bool   `json:"ok"`
	UploadID string `json:"uploadId,omitempty"` // send 成功后对端暂存区 ID
	Code     string `json:"code,omitempty"`     // denied|too-large|quota|bad-hash|no-store|not-found|failed
	Message  string `json:"message,omitempty"`
}

// fileList list purpose 的回帧
type fileList struct {
	Files []mesh.RemoteFileRef `json:"files"`
}

// uploadStore 本节点的上传暂存区（nil = 不参与 mesh 文件传输）
func (m *meshNode) uploadStore() *uploads.Store { return m.opts.Uploads }

// fileMax returns the effective per-file cap for this node.
func (m *meshNode) fileMax() int64 {
	if m.cfg.FileMaxBytes > 0 {
		return m.cfg.FileMaxBytes
	}
	return defaultFileMaxBytes
}

// effectiveFileMax 实际生效的单文件收发上限：mesh 配置与暂存区上限取小——
// 落地必然过 Store.Save 的 MaxFileBytes，前置按小值校验才能回干净的
// too-large，而不是落盘失败回 failed
func (m *meshNode) effectiveFileMax() int64 {
	limit := m.fileMax()
	if st := m.uploadStore(); st != nil {
		if store := st.Config().MaxFileBytes; store > 0 && store < limit {
			limit = store
		}
	}
	return limit
}

// SendFile 推一个文件到 peer 的暂存区。meta 携带内容路径；sha256 由发送端
// 现算（不信任 meta 里落盘的值）。
func (m *meshNode) SendFile(ctx context.Context, peerID string, meta *uploads.Meta) (mesh.RemoteFileRef, *fileAck, error) {
	return m.fileRoundTrip(ctx, peerID, fileHeader{
		ID: newID(), Name: meta.Name, Size: meta.Size, Purpose: "send",
	}, meta.Path)
}

// PullFile 从 peer 的暂存区拉一个文件回本节点暂存区（purpose=pull）。
func (m *meshNode) PullFile(ctx context.Context, peerID, uploadID string) (mesh.RemoteFileRef, error) {
	ref, _, err := m.fileRoundTrip(ctx, peerID, fileHeader{
		ID: newID(), Purpose: "pull", UploadID: uploadID,
	}, "")
	return ref, err
}

// ListRemoteFiles 列 peer 暂存区里与 mesh 传输相关的文件。
func (m *meshNode) ListRemoteFiles(ctx context.Context, peerID string) ([]mesh.RemoteFileRef, error) {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return nil, fmt.Errorf("invalid peer id %q: %w", peerID, err)
	}
	if m.host.Network().Connectedness(pid) != network.Connected {
		return nil, mesh.ErrPeerNotConnected
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := m.host.NewStream(sctx, pid, protoFile)
	if err != nil {
		return nil, fmt.Errorf("open file stream to %s: %w", pid, err)
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(30 * time.Second))
	if err := writeFrame(s, fileHeader{ID: newID(), Purpose: "list"}); err != nil {
		return nil, err
	}
	var fl fileList
	if err := readFrame(s, &fl); err != nil {
		return nil, err
	}
	return fl.Files, nil
}

// fileRoundTrip 跑一次 send/pull 传输：写头帧 → send 写字节流 / pull 读字节流 →
// 收/发 ack。返回暂存区引用（本侧落地的文件）。
func (m *meshNode) fileRoundTrip(ctx context.Context, peerID string, hdr fileHeader, localPath string) (mesh.RemoteFileRef, *fileAck, error) {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return mesh.RemoteFileRef{}, nil, fmt.Errorf("invalid peer id %q: %w", peerID, err)
	}
	if m.host.Network().Connectedness(pid) != network.Connected {
		return mesh.RemoteFileRef{}, nil, mesh.ErrPeerNotConnected
	}
	if m.uploadStore() == nil {
		return mesh.RemoteFileRef{}, nil, errors.New("this node has no upload store (mesh file transfer disabled)")
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	s, err := m.host.NewStream(sctx, pid, protoFile)
	if err != nil {
		return mesh.RemoteFileRef{}, nil, fmt.Errorf("open file stream to %s: %w", pid, err)
	}
	defer s.Close()
	// 读写整体限期：对端挂住流不回 ack 时到点报错，不永久阻塞调用方
	deadline, ok := sctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Minute)
	}
	s.SetDeadline(deadline)

	switch hdr.Purpose {
	case "send":
		// 先读一遍算哈希与实际大小（不信任落盘值），再发送
		f, err := os.Open(localPath)
		if err != nil {
			return mesh.RemoteFileRef{}, nil, err
		}
		defer f.Close()
		hash := sha256.New()
		size, err := io.Copy(hash, f)
		if err != nil {
			return mesh.RemoteFileRef{}, nil, err
		}
		if size > m.fileMax() {
			return mesh.RemoteFileRef{}, nil, errors.New("file exceeds mesh transfer limit")
		}
		hdr.Size = size
		hdr.SHA256 = hex.EncodeToString(hash.Sum(nil))
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return mesh.RemoteFileRef{}, nil, err
		}
		if err := writeFrame(s, hdr); err != nil {
			return mesh.RemoteFileRef{}, nil, err
		}
		// 只发 size 字节：两次读之间文件变大时余量不进流，不污染对端 ack 帧解析
		if _, err := io.Copy(s, io.LimitReader(f, size)); err != nil {
			return mesh.RemoteFileRef{}, nil, err
		}
		var ack fileAck
		if err := readFrame(s, &ack); err != nil {
			return mesh.RemoteFileRef{}, nil, err
		}
		if !ack.OK {
			return mesh.RemoteFileRef{}, &ack, nil
		}
		return mesh.RemoteFileRef{UploadID: ack.UploadID, Name: hdr.Name, Size: size, SHA256: hdr.SHA256}, &ack, nil
	case "pull":
		if err := writeFrame(s, hdr); err != nil {
			return mesh.RemoteFileRef{}, nil, err
		}
		// 对端先回 FileHeader（含 size/sha256），随后是字节流
		var rh fileHeader
		if err := readFrame(s, &rh); err != nil {
			return mesh.RemoteFileRef{}, nil, err
		}
		if rh.Name == "" {
			var ack fileAck
			_ = readFrame(s, &ack)
			return mesh.RemoteFileRef{}, nil, fmt.Errorf("pull rejected: %s", ack.Code)
		}
		if rh.Size < 0 || rh.Size > m.effectiveFileMax() {
			_ = s.Reset()
			return mesh.RemoteFileRef{}, nil, errors.New("pulled file exceeds mesh transfer limit")
		}
		meta, err := m.uploadStore().Save(rh.Name, "mesh:"+peerID, "", io.LimitReader(s, rh.Size))
		if err != nil {
			code := "failed"
			if errors.Is(err, uploads.ErrQuota) {
				code = "quota"
			} else if errors.Is(err, uploads.ErrTooLarge) {
				code = "too-large"
			}
			_ = writeFrame(s, fileAck{OK: false, Code: code, Message: err.Error()})
			return mesh.RemoteFileRef{}, nil, err
		}
		// 完整性核对
		if rh.SHA256 != "" && !hmacEqualStr(meta.SHA256, rh.SHA256) {
			_ = m.uploadStore().Delete(meta.ID)
			_ = writeFrame(s, fileAck{OK: false, Code: "bad-hash"})
			return mesh.RemoteFileRef{}, nil, errors.New("pulled file hash mismatch")
		}
		_ = writeFrame(s, fileAck{OK: true, UploadID: meta.ID})
		return mesh.RemoteFileRef{UploadID: meta.ID, Name: meta.Name, Size: meta.Size, SHA256: meta.SHA256}, nil, nil
	}
	return mesh.RemoteFileRef{}, nil, fmt.Errorf("unknown file purpose %q", hdr.Purpose)
}

// handleFileStream 响应端：一 stream 一次传输，按 Purpose 分派。
// 未完成互验的 peer 不放行（等待互验，口径同 agent 流）。
func (m *meshNode) handleFileStream(s network.Stream) {
	defer s.Close()
	pid := s.Conn().RemotePeer()
	if !m.waitPeerAdmitted(pid, m.authWait) {
		meshLogf("file stream from %s rejected: mutual auth not complete", pid)
		_ = s.Reset()
		return
	}
	// 收流整体限期：对端中途挂住时到点断流，释放 handler
	s.SetDeadline(time.Now().Add(fileRecvTimeout))
	from := pid.String()
	var hdr fileHeader
	if err := readFrame(s, &hdr); err != nil {
		meshLogf("file stream from %s: %v", from, err)
		return
	}
	switch hdr.Purpose {
	case "send":
		m.handleFileSend(s, from, hdr)
	case "pull":
		m.handleFilePull(s, from, hdr)
	case "list":
		m.handleFileList(s, from)
	default:
		_ = writeFrame(s, fileAck{OK: false, Code: "failed", Message: "unknown purpose"})
	}
}

// fileReceiveAllowed 本节点是否接收 from 的文件（开关 + trustedPeers，同
// allowRemoteTasks 口径）
func (m *meshNode) fileReceiveAllowed(from string) bool {
	return peerTrusted(m.cfg.AllowRemoteFiles, m.cfg.TrustedPeers, from)
}

// handleFileSend 收一份推来的文件：校验链（授权 → 上限 → 哈希）后落暂存区。
// 协议：头帧（含 size/sha256）→ 字节流 → 回 FileAck。
func (m *meshNode) handleFileSend(s network.Stream, from string, hdr fileHeader) {
	if !m.fileReceiveAllowed(from) {
		_ = writeFrame(s, fileAck{OK: false, Code: "denied", Message: "peer not authorized for file transfer"})
		return
	}
	if m.uploadStore() == nil {
		_ = writeFrame(s, fileAck{OK: false, Code: "no-store", Message: "no upload store on this node"})
		return
	}
	if hdr.Size < 0 || hdr.Size > m.effectiveFileMax() {
		_ = writeFrame(s, fileAck{OK: false, Code: "too-large", Message: "file exceeds mesh transfer limit"})
		return
	}
	meta, err := m.uploadStore().Save(hdr.Name, "mesh:"+from, "", io.LimitReader(s, hdr.Size))
	if err != nil {
		code := "failed"
		if errors.Is(err, uploads.ErrQuota) {
			code = "quota"
		} else if errors.Is(err, uploads.ErrTooLarge) {
			code = "too-large"
		}
		_ = writeFrame(s, fileAck{OK: false, Code: code, Message: err.Error()})
		return
	}
	if hdr.SHA256 != "" && !hmacEqualStr(meta.SHA256, hdr.SHA256) {
		_ = m.uploadStore().Delete(meta.ID)
		_ = writeFrame(s, fileAck{OK: false, Code: "bad-hash", Message: "sha256 mismatch"})
		return
	}
	meshLogf("file %s (%s, %d bytes) received from %s -> upload %s", hdr.Name, meta.MIME, meta.Size, from, meta.ID)
	_ = writeFrame(s, fileAck{OK: true, UploadID: meta.ID})
}

// handleFilePull 响应一次拉取：文件必须来自 mesh 传输或 agent 暂存（本地 web
// 上传的文件不通过 mesh 暴露）。回 FileHeader + 字节流。
func (m *meshNode) handleFilePull(s network.Stream, from string, hdr fileHeader) {
	if !m.fileReceiveAllowed(from) { // pull 与 send 共用同一开关
		_ = writeFrame(s, fileAck{OK: false, Code: "denied", Message: "peer not authorized for file transfer"})
		return
	}
	if m.uploadStore() == nil {
		_ = writeFrame(s, fileAck{OK: false, Code: "no-store"})
		return
	}
	meta, err := m.uploadStore().Meta(hdr.UploadID)
	if err != nil {
		_ = writeFrame(s, fileAck{OK: false, Code: "not-found", Message: "upload not found"})
		return
	}
	if !meshFileShareable(meta.Source, from) {
		_ = writeFrame(s, fileAck{OK: false, Code: "denied", Message: "file not shareable with requesting peer"})
		return
	}
	// 回头帧（对方按 Size/SHA256 收流），随后写字节
	if err := writeFrame(s, fileHeader{ID: hdr.ID, Name: meta.Name, Size: meta.Size, SHA256: meta.SHA256, Purpose: "pull"}); err != nil {
		return
	}
	f, err := os.Open(meta.Path)
	if err != nil {
		// 头帧已回、无法补 FileAck：reset 让对端 Save 报流错误，而不是
		// 截断流被误读成 hash mismatch
		_ = s.Reset()
		return
	}
	defer f.Close()
	if _, err := io.Copy(s, f); err != nil {
		_ = s.Reset()
		return
	}
}

// meshFileShareable pull 归属校验：mesh 传输来的文件回发给原发送方，agent
// 暂存的（任务产物）任何受信 peer 可拉；web 上传的文件不对 mesh 暴露。
func meshFileShareable(source, from string) bool {
	return source == "mesh:"+from || source == "agent"
}

// handleFileList 列暂存区里可分享给 from 的文件。
func (m *meshNode) handleFileList(s network.Stream, from string) {
	if !m.fileReceiveAllowed(from) {
		_ = writeFrame(s, fileList{})
		return
	}
	metas, err := m.uploadStore().List()
	if err != nil {
		_ = writeFrame(s, fileList{})
		return
	}
	var fl fileList
	for i := range metas {
		m := &metas[i]
		if !meshFileShareable(m.Source, from) {
			continue
		}
		fl.Files = append(fl.Files, mesh.RemoteFileRef{UploadID: m.ID, Name: m.Name, Size: m.Size, SHA256: m.SHA256})
	}
	_ = writeFrame(s, fl)
}

// hmacEqualStr 常量时间字符串比较（哈希比对用）
func hmacEqualStr(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
