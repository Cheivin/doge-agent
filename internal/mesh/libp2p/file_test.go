//go:build mesh

package libp2p

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doge/internal/coding/uploads"
	"doge/internal/mesh"
)

// newNodeWithStore 带上传暂存区的测试节点（store 必须在 New 前传入，
// protoFile handler 才会注册）
func newNodeWithStore(t *testing.T, tok *mesh.Token, name string) *meshNode {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mesh-"+name+".json")
	cfg := &mesh.Config{Name: name, Roles: []string{name}}
	cfg.SetToken(tok)
	m, err := New(mesh.Options{
		ConfigPath: path,
		Config:     cfg,
		Probe:      true,
		Version:    "test",
		Uploads:    uploads.NewStore(filepath.Join(t.TempDir(), "uploads"), uploads.Config{MaxFileBytes: 1 << 20}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// stageBytes 直接往 store 放一个文件
func stageBytes(t *testing.T, store *uploads.Store, name, content string) *uploads.Meta {
	t.Helper()
	meta, err := store.Save(name, "agent", "", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

// TestFileSendAndPull 全链路：A send → B store 落地；B stage → A pull 回来。
func TestFileSendAndPull(t *testing.T) {
	tok := testToken(t)
	a := newNodeWithStore(t, tok, "node-a")
	b := newNodeWithStore(t, tok, "node-b")
	b.cfg.AllowRemoteFiles = true
	a.cfg.AllowRemoteFiles = true
	connectAndWait(t, a, b)
	ctx := context.Background()

	// A → B
	meta := stageBytes(t, a.uploadStore(), "report.csv", "col1,col2\n1,2\n")
	ref, ack, err := a.SendFile(ctx, b.host.ID().String(), meta)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !ack.OK {
		t.Fatalf("ack = %+v", ack)
	}
	got, err := b.uploadStore().Meta(ref.UploadID)
	if err != nil {
		t.Fatalf("remote meta: %v", err)
	}
	if got.Name != "report.csv" || got.Size != meta.Size || got.SHA256 != meta.SHA256 {
		t.Fatalf("remote meta = %+v, want name/size/sha256 match", got)
	}
	data, _ := io.ReadAll(mustOpen(t, got.Path))
	if string(data) != "col1,col2\n1,2\n" {
		t.Fatalf("remote content = %q", data)
	}

	// B stage → A pull（任务产物回传路径）
	out := stageBytes(t, b.uploadStore(), "result.txt", "task output")
	pulled, err := a.PullFile(ctx, b.host.ID().String(), out.ID)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	local, err := a.uploadStore().Meta(pulled.UploadID)
	if err != nil {
		t.Fatalf("pulled meta: %v", err)
	}
	data, _ = io.ReadAll(mustOpen(t, local.Path))
	if string(data) != "task output" {
		t.Fatalf("pulled content = %q", data)
	}
}

// TestFileSendDenied 未开 allowRemoteFiles 的节点拒绝接收。
func TestFileSendDenied(t *testing.T) {
	tok := testToken(t)
	a := newNodeWithStore(t, tok, "node-a")
	b := newNodeWithStore(t, tok, "node-b") // allowRemoteFiles 默认 false
	connectAndWait(t, a, b)
	meta := stageBytes(t, a.uploadStore(), "x.txt", "x")
	_, ack, err := a.SendFile(context.Background(), b.host.ID().String(), meta)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if ack.OK || ack.Code != "denied" {
		t.Fatalf("ack = %+v, want denied", ack)
	}
}

// TestFileSendBadHash 哈希错配的推送被拒且无残留。
func TestFileSendBadHash(t *testing.T) {
	tok := testToken(t)
	a := newNodeWithStore(t, tok, "node-a")
	b := newNodeWithStore(t, tok, "node-b")
	b.cfg.AllowRemoteFiles = true
	connectAndWait(t, a, b)

	// 手工开 stream，推一个声明哈希与内容不符的文件
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := a.host.NewStream(ctx, b.host.ID(), protoFile)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := writeFrame(s, fileHeader{ID: newID(), Name: "evil.bin", Size: 4, SHA256: strings.Repeat("0", 64), Purpose: "send"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	var ack fileAck
	if err := readFrame(s, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.OK || ack.Code != "bad-hash" {
		t.Fatalf("ack = %+v, want bad-hash", ack)
	}
	// 半成品已删除
	metas, _ := b.uploadStore().List()
	if len(metas) != 0 {
		t.Fatalf("half-written file left: %d", len(metas))
	}
}

// TestFileSendTooLarge 超过对端 fileMaxBytes 的推送被拒。
func TestFileSendTooLarge(t *testing.T) {
	tok := testToken(t)
	a := newNodeWithStore(t, tok, "node-a")
	b := newNodeWithStore(t, tok, "node-b")
	b.cfg.AllowRemoteFiles = true
	b.cfg.FileMaxBytes = 8
	connectAndWait(t, a, b)
	meta := stageBytes(t, a.uploadStore(), "big.bin", strings.Repeat("x", 100))
	_, ack, err := a.SendFile(context.Background(), b.host.ID().String(), meta)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if ack.OK || ack.Code != "too-large" {
		t.Fatalf("ack = %+v, want too-large", ack)
	}
}

// TestListRemoteFiles list 只返回可分享文件（mesh 来源与 agent 暂存），
// web 上传的文件不对 mesh 暴露。
func TestListRemoteFiles(t *testing.T) {
	tok := testToken(t)
	a := newNodeWithStore(t, tok, "node-a")
	b := newNodeWithStore(t, tok, "node-b")
	b.cfg.AllowRemoteFiles = true
	connectAndWait(t, a, b)

	stageBytes(t, b.uploadStore(), "agent-out.txt", "out")                              // agent 暂存：可拉
	_, _ = b.uploadStore().Save("web-only.txt", "web", "", strings.NewReader("secret")) // web 来源：不暴露

	files, err := a.ListRemoteFiles(context.Background(), b.host.ID().String())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 1 || files[0].Name != "agent-out.txt" {
		t.Fatalf("files = %+v, want only agent-out.txt", files)
	}
}

func mustOpen(t *testing.T, path string) io.Reader {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestRemoteTaskFilesDeniedReadonly 只读任务带文件被从侧拒绝（写副作用）。
func TestRemoteTaskFilesDeniedReadonly(t *testing.T) {
	p := newCollabPair(t, "readonly")
	tp := mesh.TaskPayload{
		Task: "x", Context: "fresh",
		Files: []mesh.RemoteFileRef{{UploadID: "abc", Name: "f.txt", Size: 1}},
	}
	data, _ := json.Marshal(tp)
	taskID := "task-files-denied"
	if err := p.a.Send(p.b.host.ID().String(), mesh.Envelope{Type: mesh.MsgTask, ID: taskID, To: p.b.host.ID().String(), Payload: data}); err != nil {
		t.Fatal(err)
	}
	ch := make(chan mesh.ErrorPayload, 1)
	unsub := p.a.Subscribe(func(env mesh.Envelope) error {
		if env.Type == mesh.MsgError && env.ReplyTo == taskID {
			var ep mesh.ErrorPayload
			if json.Unmarshal(env.Payload, &ep) == nil {
				select {
				case ch <- ep:
				default:
				}
			}
		}
		return nil
	})
	defer unsub()
	select {
	case ep := <-ch:
		if ep.Code != "denied" || !strings.Contains(ep.Message, "remoteTaskLevel") {
			t.Fatalf("error = %+v, want denied/remoteTaskLevel", ep)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for rejection")
	}
}
