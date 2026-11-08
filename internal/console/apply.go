package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/internal/coding/uploads"
	"doge/internal/mcp"
	"doge/pkg/logx"
)

// applyMu place/apply 写盘段的全局锁：并发 confirm 会互相覆盖 .bak 与目标
var applyMu sync.Mutex

// LockApply 串行执行一段写盘（place 的替换、mcp 合并等短临界区）
func LockApply(fn func()) {
	applyMu.Lock()
	defer applyMu.Unlock()
	fn()
}

// ApplyDeps apply 的会话上下文：Reload 是配置写盘后的会话热重载（web:
// reloadAfterConfigWrite；desktop: reloadAllSessions）；Mcp 是 kind=mcp 的
// confirm 段（合并/重建注册表/重发工具组各端实现，mcp 未启用的错误类别
// 两端不同——web 404、desktop unavailable——由实现返回）。
type ApplyDeps struct {
	Reload func() error
	Mcp    func(servers map[string]*mcp.Server) *Error
}

// metaOf 取暂存条目（暂存区不可用 → unavailable；未找到 → notfound）
func metaOf(store *uploads.Store, id string) (*uploads.Meta, *Error) {
	if store == nil {
		return nil, Unavailable("uploads not available")
	}
	meta, err := store.Meta(id)
	if err != nil {
		if errors.Is(err, uploads.ErrNotFound) {
			return nil, NotFound("upload not found")
		}
		return nil, Wrap(err)
	}
	return meta, nil
}

// PlaceUpload 把暂存区文件复制到白名单目的地（harness 直写，不经
// permissions；守卫是白名单根 + EvalSymlinks 复核）。dest 相对路径相对会话 cwd。
func PlaceUpload(store *uploads.Store, id, dest, cwd string, overwrite bool) (map[string]any, *Error) {
	meta, e := metaOf(store, id)
	if e != nil {
		return nil, e
	}
	dest, err := resolvePlacePath(dest, cwd)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	// 目标已存在：symlink 一律拒绝（防写入时跟随逃逸）；非覆盖冲突 busy
	if st, err := os.Lstat(dest); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, Invalid("destination is a symlink")
		}
		if !overwrite {
			return nil, Busy("destination already exists")
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, Wrap(err)
	}
	// 复制走同目录临时文件 + rename 原子替换：中途失败不留半截目标文件；
	// 复制（可达 100MiB）在锁外，锁只串行化最后的替换
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".doge-place-*")
	if err != nil {
		return nil, Wrap(err)
	}
	src, err := os.Open(meta.Path)
	if err != nil {
		_ = os.Remove(tmp.Name())
		tmp.Close()
		return nil, Wrap(err)
	}
	_, err = io.Copy(tmp, src)
	src.Close()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, Wrap(err)
	}
	var rerr error
	LockApply(func() { rerr = os.Rename(tmp.Name(), dest) })
	if rerr != nil {
		_ = os.Remove(tmp.Name())
		return nil, Wrap(rerr)
	}
	logx.Infof("[console] upload %s placed to %s", meta.ID, dest)
	return map[string]any{"ok": true, "path": dest}, nil
}

// ApplyUpload 应用暂存区文件为配置（kind: mcp/models/skills-zip/permissions）。
// 双段确认：默认 dry-run 只校验+预览；confirm:true 才写盘并触发 reload。
// auth.json / mesh.json / settings.json 不提供 apply。各 kind 的写盘段由
// applyMu 串行化（防并发 confirm 互相覆盖备份与目标），复制/解压与 reload 在锁外。
func ApplyUpload(store *uploads.Store, id, kind string, confirm bool, d ApplyDeps) (map[string]any, *Error) {
	meta, e := metaOf(store, id)
	if e != nil {
		return nil, e
	}
	switch kind {
	case "mcp":
		return applyUploadMcp(meta, confirm, d)
	case "models":
		return applyModelsConfig(meta, confirm, d)
	case "skills-zip":
		return applySkillsZip(store, meta, confirm, d)
	case "permissions":
		return applyPermissionsConfig(meta, confirm, d)
	default:
		return nil, Invalid("unknown kind (want mcp|models|skills-zip|permissions)")
	}
}

// applyUploadMcp kind=mcp：读暂存文件按社区 mcpServers 格式解析；dry-run 只
// 出预览，confirm 段经 d.Mcp 完成合并与工具组重发（等价 mcp/servers 导入 +
// mcp/reload）。
func applyUploadMcp(meta *uploads.Meta, confirm bool, d ApplyDeps) (map[string]any, *Error) {
	body, err := os.ReadFile(meta.Path)
	if err != nil {
		return nil, Wrap(err)
	}
	servers, e := ParseMcpServersPayload(body)
	if e != nil {
		return nil, e
	}
	if !confirm {
		return map[string]any{"ok": true, "dryRun": true, "diff": mcpServersSummary(servers)}, nil
	}
	if d.Mcp == nil {
		return nil, NotFound("mcp not enabled")
	}
	if e := d.Mcp(servers); e != nil {
		return nil, e
	}
	logx.Infof("[console] mcp config applied from upload %s (%d servers)", meta.ID, len(servers))
	return map[string]any{"ok": true, "reloaded": true, "applied": len(servers)}, nil
}

// mcpServersSummary 生成 dry-run 预览文本
func mcpServersSummary(servers map[string]*mcp.Server) string {
	names := make([]string, 0, len(servers))
	for k := range servers {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "将合并 %d 个 MCP server：", len(names))
	for _, n := range names {
		fmt.Fprintf(&b, "\n- %s (%s)", n, transportOf(servers[n]))
	}
	return b.String()
}

func transportOf(srv *mcp.Server) string {
	if srv == nil {
		return "unknown"
	}
	if srv.URL != "" {
		return "http"
	}
	return "stdio"
}

// applyModelsConfig 整体替换全局 models.json（先备份 .bak），confirm 后触发
// 热重载（模型目录全局生效，其他窗口按需自行 reload）
func applyModelsConfig(meta *uploads.Meta, confirm bool, d ApplyDeps) (map[string]any, *Error) {
	data, err := os.ReadFile(meta.Path)
	if err != nil {
		return nil, Wrap(err)
	}
	var models []*ai.Model
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, Invalid("models.json must be a JSON array of model definitions: " + err.Error())
	}
	if len(models) == 0 {
		return nil, Invalid("models.json is empty")
	}
	for _, m := range models {
		if m.ID == "" || m.Provider == "" {
			return nil, Invalid("every model needs id and provider")
		}
	}
	specs := make([]string, 0, len(models))
	for _, m := range models {
		specs = append(specs, m.Provider+"/"+m.ID)
	}
	if !confirm {
		return map[string]any{"ok": true, "dryRun": true,
			"diff": fmt.Sprintf("将导入 %d 个模型定义：\n- %s", len(models), strings.Join(specs, "\n- "))}, nil
	}
	target := coding.ModelsFile()
	LockApply(func() {
		BackupFile(target)
		err = WriteFileAtomic(target, data, 0o644)
	})
	if err != nil {
		return nil, Wrap(err)
	}
	reloaded, reloadErr := reloadFlags(d.Reload)
	logx.Infof("[console] models.json applied from upload %s (%d models)", meta.ID, len(models))
	return map[string]any{"ok": true, "reloaded": reloaded, "applied": len(models), "error": reloadErr}, nil
}

// applySkillsZip 把 zip 里的技能包安装到全局 skills 目录（覆盖同名技能），
// confirm 后热刷新（技能工具与系统提示即时重建）
func applySkillsZip(store *uploads.Store, meta *uploads.Meta, confirm bool, d ApplyDeps) (map[string]any, *Error) {
	tmp, err := os.MkdirTemp("", "doge-skill-zip-")
	if err != nil {
		return nil, Wrap(err)
	}
	defer os.RemoveAll(tmp)
	files, err := uploads.ExtractZip(meta.Path, tmp, store.Config().MaxFileBytes)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	skills := coding.LoadSkills(tmp)
	if len(skills) == 0 {
		return nil, Invalid(fmt.Sprintf("no SKILL.md found in zip (%d files)", len(files)))
	}
	if !confirm {
		var b strings.Builder
		fmt.Fprintf(&b, "将安装 %d 个技能：", len(skills))
		for _, sk := range skills {
			fmt.Fprintf(&b, "\n- %s: %s", sk.Name, sk.Description)
		}
		return map[string]any{"ok": true, "dryRun": true, "diff": b.String(), "files": len(files)}, nil
	}
	// 安装目标固定为全局 skills 目录（与 --skill 自定义目录解耦）
	dest := filepath.Join(coding.AgentDir(), "skills")
	installed := make([]string, 0, len(skills))
	var installErr error
	LockApply(func() {
		for _, sk := range skills {
			srcDir := filepath.Dir(sk.FilePath)
			target := filepath.Join(dest, sk.Name)
			// 覆盖安装：先清掉同名旧技能，否则发现时旧版先到先得
			if err := os.RemoveAll(target); err != nil {
				installErr = err
				return
			}
			if err := os.CopyFS(target, os.DirFS(srcDir)); err != nil {
				installErr = err
				return
			}
			installed = append(installed, sk.Name)
		}
	})
	if installErr != nil {
		return nil, Wrap(installErr)
	}
	reloaded, reloadErr := reloadFlags(d.Reload)
	logx.Infof("[console] skills installed from upload %s: %s", meta.ID, strings.Join(installed, ","))
	return map[string]any{"ok": true, "reloaded": reloaded, "installed": installed, "error": reloadErr}, nil
}

// applyPermissionsConfig 整体替换全局 permissions.json（先备份 .bak），confirm
// 后触发热重载。这是本地 token 边界内的面板操作；远端来向永远到不了这里
// （mesh config-push 显式拒绝 permissions）。
func applyPermissionsConfig(meta *uploads.Meta, confirm bool, d ApplyDeps) (map[string]any, *Error) {
	data, err := os.ReadFile(meta.Path)
	if err != nil {
		return nil, Wrap(err)
	}
	policy, e := ValidatePermissionsData(data)
	if e != nil {
		return nil, e
	}
	if !confirm {
		return map[string]any{"ok": true, "dryRun": true, "diff": permissionsSummary(policy)}, nil
	}
	target := filepath.Join(coding.AgentDir(), "permissions.json")
	LockApply(func() {
		BackupFile(target)
		err = WriteFileAtomic(target, data, 0o644)
	})
	if err != nil {
		return nil, Wrap(err)
	}
	reloaded, reloadErr := reloadFlags(d.Reload)
	logx.Infof("[console] permissions.json applied from upload %s", meta.ID)
	return map[string]any{"ok": true, "reloaded": reloaded, "error": reloadErr}, nil
}

// reloadFlags 配置写盘后的热重载结果字段（重载失败不影响已写入的文件）
func reloadFlags(reload func() error) (reloaded bool, reloadErr string) {
	reloaded = true
	if reload == nil {
		return
	}
	if err := reload(); err != nil {
		reloaded, reloadErr = false, err.Error()
	}
	return
}

// placeRoot place 目的地的白名单根
type placeRoot struct {
	path  string
	exact bool // 只允许精确写到该文件（配置文件），不允许其子树
}

// placeRoots place 白名单：会话 cwd 子树、全局 skills 目录、两个可整体替换
// 的配置文件（models.json / mcp.json）
func placeRoots(cwd string) []placeRoot {
	return []placeRoot{
		{cwd, false},
		{filepath.Join(coding.AgentDir(), "skills"), false},
		{coding.ModelsFile(), true},
		{filepath.Join(coding.AgentDir(), "mcp.json"), true},
	}
}

// resolvePlacePath 解析并校验 place 目的地：相对路径相对 cwd；拒绝 ".." 段；
// 必须落在白名单根内（对已存在部分做 EvalSymlinks，防 symlink 逃逸）。
func resolvePlacePath(dest, cwd string) (string, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", errors.New("dest required")
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(cwd, dest)
	}
	dest = filepath.Clean(dest)
	for _, seg := range strings.Split(filepath.ToSlash(dest), string(os.PathSeparator)) {
		if seg == ".." {
			return "", errors.New("dest must not contain ..")
		}
	}
	real, err := evalRealPath(dest)
	if err != nil {
		return "", err
	}
	for _, root := range placeRoots(cwd) {
		rootReal, err := evalRealPath(root.path)
		if err != nil {
			continue
		}
		if root.exact {
			if real == rootReal {
				return dest, nil
			}
			continue
		}
		if real == rootReal || strings.HasPrefix(real, rootReal+string(os.PathSeparator)) {
			return dest, nil
		}
	}
	return "", errors.New("destination outside allowed roots")
}

// evalRealPath 解析路径中已存在的最长前缀为真实路径（逐层穿透 symlink），
// 不存在的尾部按字面拼接。
func evalRealPath(p string) (string, error) {
	p = filepath.Clean(p)
	cur := p
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, tail...)...), nil
		}
		dir, base := filepath.Split(cur)
		if dir == cur {
			return p, nil
		}
		tail = append([]string{base}, tail...)
		cur = filepath.Clean(dir)
	}
}

// ValidatePermissionsData 解析上传/提交的权限策略（临时文件走标准加载路径）
func ValidatePermissionsData(data []byte) (*permissions.Policy, *Error) {
	tmp, err := os.CreateTemp("", "doge-perm-*.json")
	if err != nil {
		return nil, Wrap(err)
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, Wrap(err)
	}
	if err := tmp.Close(); err != nil {
		return nil, Wrap(err)
	}
	policy, err := permissions.LoadPolicy(path)
	if err != nil {
		return nil, Invalid(err.Error())
	}
	return policy, nil
}

// permissionsSummary 生成 dry-run 预览文本
func permissionsSummary(p *permissions.Policy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "将替换全局权限策略：\n- 默认：tools=%s bash=%s",
		orUnset(p.DefaultPolicy.Tools), orUnset(p.DefaultPolicy.Bash))
	if p.Special.ExternalDirectory != "" {
		fmt.Fprintf(&b, "\n- 外部目录：%s", p.Special.ExternalDirectory)
	}
	fmt.Fprintf(&b, "\n- 工具规则：%d 条，bash 规则：%d 条", len(p.Tools), len(p.Bash))
	return b.String()
}

func orUnset(s string) string {
	if s == "" {
		return "allow(缺省)"
	}
	return s
}

// BackupFile 现文件存在时备份为 <path>.bak
func BackupFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 目标还不存在，无需备份
	}
	_ = os.WriteFile(path+".bak", data, 0o600)
}
