package webui

// 契约测试：共享 UI 内核与两端传输适配层的一致性护栏。
//
//  1. interface 契约：platform-web.js 与 desktop/frontend/platform-desktop.js
//     必须导出同一组符号（app.js 只 import /platform.js 一份 interface）。
//  2. 接线契约：app.js 里出现的每一个 "/api/..." 字面量都必须能在桌面适配层
//     的 desktopApi 路由表里找到匹配分支——防"未接线的 API 调用"类回归
//     （桌面端 console.warn + 404 not wired）。
//  3. Web 面契约：同一个字面量还必须注册在 internal/web 的路由表里（两端
//     同源，共享 UI 不发平台孤立的请求）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoFile 从包目录出发定位仓库内文件（go test 的工作目录 = 包源码目录）
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

var exportRe = regexp.MustCompile(`(?m)^export\s+(?:async\s+)?(?:function|const)\s+(\w+)`)

// TestPlatformInterfaceParity 两端适配层导出同一 interface
func TestPlatformInterfaceParity(t *testing.T) {
	web := exportRe.FindAllStringSubmatch(webFile(t, "platform-web.js"), -1)
	desktop := exportRe.FindAllStringSubmatch(repoFile(t, "desktop/frontend/platform-desktop.js"), -1)
	if len(web) == 0 || len(desktop) == 0 {
		t.Fatalf("no exports found (web=%d desktop=%d)", len(web), len(desktop))
	}
	want := map[string]bool{}
	for _, m := range web {
		want[m[1]] = true
	}
	got := map[string]bool{}
	for _, m := range desktop {
		got[m[1]] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("platform-desktop.js 缺少导出 %q（platform-web.js 有）", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("platform-desktop.js 多出导出 %q（platform-web.js 没有）", name)
		}
	}
	if len(want) != 6 {
		t.Errorf("platform 导出数 = %d，want 6（api/connect/answerPermission/confirmDialog/promptDialog/caps）", len(want))
	}
}

func webFile(t *testing.T, name string) string {
	t.Helper()
	f, err := FS.Open(name)
	if err != nil {
		t.Fatalf("webui.FS open %s: %v", name, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	if st.IsDir() {
		t.Fatalf("%s 是目录", name)
	}
	data := make([]byte, st.Size())
	if _, err := f.Read(data); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

var apiLiteralRe = regexp.MustCompile(`["'](/api/[^"'?]+)(\?[^"']*)?["']`)

// tableMatched 报告 desktopApi 路由表是否接了该路径（精确 p===/p!==、
// 前缀 p.startsWith / p.slice 均算接线）
func tableMatched(table, path string) bool {
	for _, pat := range []string{
		`p === "` + path + `"`,
		`p !== "` + path + `"`,
		`p.startsWith("` + path + `")`,
		`p.slice("` + path + `"`,
	} {
		if strings.Contains(table, pat) {
			return true
		}
	}
	return false
}

// routeMatched 报告 internal/web 路由表是否注册了该路径（通配参数前的
// 前缀即视为覆盖，如 /api/mcp/servers/ ↔ DELETE /api/mcp/servers/{name}）
func routeMatched(serverSrc, path string) bool {
	routeRe := regexp.MustCompile(`mux\.Handle(?:Func)?\("(?:[A-Z]+) (/[^"]+)"`)
	for _, m := range routeRe.FindAllStringSubmatch(serverSrc, -1) {
		pattern := m[1]
		if i := strings.Index(pattern, "{"); i >= 0 {
			pattern = pattern[:i]
		}
		if pattern == path || strings.HasPrefix(path, pattern) {
			return true
		}
	}
	return false
}

// TestAPILiteralsWired app.js 的每个 /api/ 字面量两端都已接线
func TestAPILiteralsWired(t *testing.T) {
	app := webFile(t, "app.js")
	table := repoFile(t, "desktop/frontend/platform-desktop.js")
	serverSrc := repoFile(t, "internal/web/server.go")

	seen := map[string]bool{}
	for _, m := range apiLiteralRe.FindAllStringSubmatch(app, -1) {
		path := m[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		if !tableMatched(table, path) {
			t.Errorf("app.js 调用 %q 但 desktopApi 路由表未接线（桌面端会 404 not wired）", path)
		}
		if !routeMatched(serverSrc, path) {
			t.Errorf("app.js 调用 %q 但 internal/web 未注册该路由", path)
		}
	}
	if len(seen) < 40 {
		t.Errorf("解析到的 API 字面量仅 %d 个，解析器可能失效", len(seen))
	}
}

// TestSharedIndexContract 共享 HTML 只经 /platform.js 与 /app.js 进 JS，
// 无内联脚本（两端 CSP 保持严格，桌面诊断脚本在 platform-desktop.js 里）
func TestSharedIndexContract(t *testing.T) {
	html := webFile(t, "index.html")
	if !strings.Contains(html, `<script type="module" src="/platform.js">`) {
		t.Errorf("index.html 未加载 /platform.js")
	}
	if !strings.Contains(html, `<script type="module" src="/app.js">`) {
		t.Errorf("index.html 未加载 /app.js")
	}
	if strings.Contains(html, "<script>") || regexp.MustCompile(`<script>[^<]`).MatchString(html) {
		t.Errorf("index.html 含内联脚本（CSP 要求 script-src 'self'）")
	}
	for _, name := range []string{"app.js", "style.css", "platform-web.js"} {
		if _, err := FS.Open(name); err != nil {
			t.Errorf("webui.FS 缺少 %s", name)
		}
	}
}
