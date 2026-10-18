package coding

import (
	"testing"

	"doge/internal/coding/permissions"
)

// fakeAskFunc 适配 permissions.AskHandler 的测试桩
type fakeAskFunc func()

func (f fakeAskFunc) Ask(permissions.AskRequest) (permissions.AskResponse, error) {
	f()
	return permissions.AskResponse{AllowOnce: true}, nil
}

// TestForkAutomationUnattended：automation fork 拒装 ask 处理器（无人值守，
// ask 全走 autoDecide 风险分级），且会话 Kind 正确标注。
func TestForkAutomationUnattended(t *testing.T) {
	base := newToolSession(t, nil, nil)
	target := t.TempDir()

	fork, err := base.ForkAutomation(target, nil, RemoteWrite)
	if err != nil {
		t.Fatalf("ForkAutomation: %v", err)
	}
	if fork.CurrentSession().Header.Kind != SessionKindAutomation {
		t.Fatalf("kind = %q", fork.CurrentSession().Header.Kind)
	}
	fork.SetPermissionAskHandler(fakeAskFunc(func() {}))
	if fork.permissions.AskHandlerInstalled() {
		t.Fatal("ask handler must not be installed on automation forks")
	}
	// 对照组：普通会话可正常安装
	base.SetPermissionAskHandler(fakeAskFunc(func() {}))
	if !base.permissions.AskHandlerInstalled() {
		t.Fatal("ask handler should install on normal sessions")
	}
}
