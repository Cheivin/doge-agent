package services

import (
	"errors"
	"strconv"
	"sync"
	"time"

	"doge/internal/agent"
	"doge/internal/coding/permissions"
	"doge/internal/web"
	"doge/pkg/logx"
)

// askTimeout 权限询问的应答预算（与 Web 控制台 hub 一致）
const askTimeout = 5 * time.Minute

type (
	// Console 事件桥：agent 事件序列化为 console 帧发往前端（替代 Web 控制台
	// 的 WS 事件流，帧结构 1:1 对齐），权限询问走「发帧 + 等待应答」模型。
	// 帧一律经 frames 队列由 pump 单线程按序发送：wails 的 Event.Emit 同步
	// 派发到 webview 主线程，在 service 绑定回调里同步调用会自己等自己死锁
	// （Promise 永不 resolve），事件也必须保序
	Console struct {
		emit    func(frame any) // 发送一帧 console 事件
		backend web.Backend
		frames  chan any // 事件帧队列（保序、有界，满则丢帧记警告）

		askMu  sync.Mutex
		asks   map[string]chan permissions.AskResponse
		serial int64
	}
)

// NewConsole 构建事件桥并启动发送泵
func NewConsole(backend web.Backend, emit func(frame any)) *Console {
	c := &Console{
		emit:    emit,
		backend: backend,
		frames:  make(chan any, 512),
		asks:    map[string]chan permissions.AskResponse{},
	}
	go c.pump()
	return c
}

// pump 单线程按序发送事件帧；Emit 异常慢时记日志（排查事件通道）
func (c *Console) pump() {
	for frame := range c.frames {
		start := time.Now()
		c.emit(frame)
		if cost := time.Since(start); cost > 200*time.Millisecond {
			logx.Warnf("[desktop] console emit slow cost=%s", cost)
		}
	}
}

// Start 订阅 agent 事件并安装权限处理器，返回清理函数
func (c *Console) Start() func() {
	unsub, _ := c.backend.Subscribe(func(ev agent.AgentEvent) error {
		c.EmitFrame(web.AgentEventFrom(ev))
		return nil
	})
	c.backend.InstallPermissionHandler(c)
	return unsub
}

// Snapshot 组装 session_info 快照帧（前端启动时续接视图，对应 WS 连接首帧）
func (c *Console) Snapshot() any {
	st := c.backend.Status()
	info := c.backend.SessionInfo()
	frame := map[string]any{
		"type":         "session_info",
		"sessionId":    st.SessionID,
		"cwd":          st.Cwd,
		"model":        st.Model,
		"thinking":     st.Thinking,
		"streaming":    st.Streaming,
		"pendingTools": st.PendingTools,
		"errorMessage": st.ErrorMessage,
		"version":      st.Version,
	}
	msgs := info.Messages
	earlier := 0
	if len(msgs) > tailMessages {
		earlier = len(msgs) - tailMessages
		msgs = msgs[earlier:]
	}
	frame["messages"] = msgs
	frame["earlierCount"] = earlier
	if st.Streaming {
		frame["streamingMessage"] = c.backend.StreamingMessage()
	}
	return frame
}

// EmitFrame 事件帧入队（永不阻塞调用方：service 绑定与 agent loop 都不能卡）
func (c *Console) EmitFrame(frame any) {
	select {
	case c.frames <- frame:
	default:
		logx.Warnf("[desktop] console frame queue full, dropped")
	}
}

// Ask 广播权限询问并等待前端应答（permissions.AskHandler 实现）。桌面端恒有
// 一个窗口消费者，不做「无客户端即拒绝」；5 分钟无应答按超时拒绝。
func (c *Console) Ask(req permissions.AskRequest) (permissions.AskResponse, error) {
	id := c.nextAskID()
	reply := make(chan permissions.AskResponse, 1)
	c.askMu.Lock()
	c.asks[id] = reply
	c.askMu.Unlock()
	defer func() {
		c.askMu.Lock()
		delete(c.asks, id)
		c.askMu.Unlock()
	}()

	c.EmitFrame(map[string]any{"type": "permission_ask", "id": id, "request": req})

	select {
	case resp := <-reply:
		return resp, nil
	case <-time.After(askTimeout):
		return permissions.AskResponse{}, errors.New("permission prompt timed out")
	}
}

// AnswerPermission 把应答送回等待中的询问，返回是否仍在等待
func (c *Console) AnswerPermission(id string, resp permissions.AskResponse) bool {
	c.askMu.Lock()
	reply := c.asks[id]
	c.askMu.Unlock()
	if reply == nil {
		return false
	}
	select {
	case reply <- resp:
	default:
	}
	return true
}

func (c *Console) nextAskID() string {
	c.askMu.Lock()
	c.serial++
	id := c.serial
	c.askMu.Unlock()
	return "ask-" + strconv.FormatInt(id, 10)
}
