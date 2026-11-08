package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"doge/internal/agent"
	"doge/internal/coding/permissions"
)

// TestSnapshotContainsPendingAsk 待应答的权限询问必须在重连快照里重放
// （pendingAsks，形状与 permission_ask 广播帧一致），否则重连的客户端
// 看不到悬着的提问卡片。
func TestSnapshotContainsPendingAsk(t *testing.T) {
	sess := newRealSession(t)
	s, ts := newTestServer(t, NewBackend(sess, "test", false))
	c := login(t, ts, "tok-123")

	conn, ctx := newAuthedWS(t, ts, c)
	readJSON(t, ctx, conn) // 初始快照

	// 发起一条待应答权限询问（hub 直接触发，等价 agent 命中 ask 策略）
	askDone := make(chan permissions.AskResponse, 1)
	go func() {
		resp, _ := s.hub.Ask(permissions.AskRequest{Kind: "bash", ToolName: "bash", Command: "rm -rf /tmp/x"})
		askDone <- resp
	}()
	m := readJSON(t, ctx, conn)
	if m["type"] != "permission_ask" {
		t.Fatalf("frame = %#v, want permission_ask", m)
	}

	// 重连：快照必须包含该询问
	conn.Close(websocket.StatusNormalClosure, "reconnect")
	conn2, ctx2 := newAuthedWS(t, ts, c)
	snap := readJSON(t, ctx2, conn2)
	if snap["type"] != "session_info" {
		t.Fatalf("frame = %#v, want session_info", snap)
	}
	asks, ok := snap["pendingAsks"].([]any)
	if !ok || len(asks) != 1 {
		t.Fatalf("snapshot pendingAsks = %#v, want one ask", snap["pendingAsks"])
	}
	ask, _ := asks[0].(map[string]any)
	if ask["type"] != "permission_ask" || ask["id"] == "" {
		t.Fatalf("pendingAsk frame = %#v", ask)
	}
	req, _ := ask["request"].(map[string]any)
	if req == nil || req["toolName"] != "bash" {
		t.Fatalf("pendingAsk request = %#v, want bash ask", ask["request"])
	}
	if qs, ok := snap["pendingQuestions"].([]any); !ok || len(qs) != 0 {
		t.Fatalf("snapshot pendingQuestions = %#v, want empty", snap["pendingQuestions"])
	}

	// REST 应答后询问闭合，Ask 返回该决定
	id, _ := ask["id"].(string)
	if code := doWindow(t, c, "POST", ts.URL+"/api/permission/answer", `{"id":"`+id+`","allowOnce":true}`, ""); code != http.StatusOK {
		t.Fatalf("permission answer = %d, want 200", code)
	}
	if resp := <-askDone; !resp.AllowOnce {
		t.Fatalf("ask response = %+v, want allowOnce", resp)
	}
}

// TestSnapshotContainsPendingQuestion 待应答的 mesh 提问必须在重连快照里
// 重放（pendingQuestions，形状与 question 广播帧一致），应答成功后不再重放。
func TestSnapshotContainsPendingQuestion(t *testing.T) {
	sess := newRealSession(t)
	_, ts := newTestServer(t, NewBackend(sess, "test", false))
	c := login(t, ts, "tok-123")

	conn, ctx := newAuthedWS(t, ts, c)
	readJSON(t, ctx, conn) // 初始快照
	waitFor(t, "session interactive after browser connect", func() bool {
		return sess.InteractiveCount() > 0
	})

	answerCh, err := sess.AskQuestion(agent.Question{ID: "q-snap", Prompt: "Which port?", Kind: "mesh", From: "peer-b", Task: "task-1"})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	m := readJSON(t, ctx, conn)
	if m["type"] != string(agent.EventQuestion) {
		t.Fatalf("frame = %#v, want question", m)
	}

	// 重连：快照必须包含该提问
	conn.Close(websocket.StatusNormalClosure, "reconnect")
	conn2, ctx2 := newAuthedWS(t, ts, c)
	snap := readJSON(t, ctx2, conn2)
	qs, ok := snap["pendingQuestions"].([]any)
	if !ok || len(qs) != 1 {
		t.Fatalf("snapshot pendingQuestions = %#v, want one question", snap["pendingQuestions"])
	}
	frame, _ := qs[0].(map[string]any)
	if frame["type"] != string(agent.EventQuestion) {
		t.Fatalf("pendingQuestion frame = %#v", frame)
	}
	q, _ := frame["question"].(map[string]any)
	if q == nil || q["id"] != "q-snap" || q["prompt"] != "Which port?" {
		t.Fatalf("pendingQuestion payload = %#v", q)
	}

	// 应答成功后，再连入的快照不再重放
	body := `{"id":"q-snap","text":"use 8080"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/question/answer", strings.NewReader(body))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("question answer = %d, want 200", resp.StatusCode)
	}
	select {
	case ans := <-answerCh:
		if ans != "use 8080" {
			t.Fatalf("answer = %q", ans)
		}
	default:
		t.Fatal("answer not delivered")
	}

	conn3, ctx3 := newAuthedWS(t, ts, c)
	snap3 := readJSON(t, ctx3, conn3)
	if qs, ok := snap3["pendingQuestions"].([]any); !ok || len(qs) != 0 {
		t.Fatalf("snapshot after answer pendingQuestions = %#v, want empty", snap3["pendingQuestions"])
	}
}

// TestIsSessionReadOnly 只读所有权哨兵可被 errors.Is 识别（桌面服务层据此
// 区分只读 403 与磁盘/内部错误），其他错误不误判。
func TestIsSessionReadOnly(t *testing.T) {
	if !IsSessionReadOnly(errSessionReadOnly) {
		t.Fatal("sentinel itself not recognized")
	}
	if !IsSessionReadOnly(fmt.Errorf("prompt: %w", errSessionReadOnly)) {
		t.Fatal("wrapped sentinel not recognized")
	}
	if IsSessionReadOnly(errors.New("disk full")) {
		t.Fatal("unrelated error misclassified as read-only")
	}
}
