package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"doge/internal/ai"
	"doge/internal/coding"
	"doge/internal/coding/permissions"
	"doge/internal/coding/uploads"
	"doge/internal/mcp"
	"doge/pkg/logx"
)

// placeRoot 是 place 目的地的白名单根
type placeRoot struct {
	path  string
	exact bool // 只允许精确写到该文件（配置文件），不允许其子树
}

// placeRoots 当前会话的 place 白名单：会话 cwd 子树、全局 skills 目录、
// 两个可整体替换的配置文件（models.json / mcp.json）。
func placeRoots(cwd string) []placeRoot {
	return []placeRoot{
		{cwd, false},
		{filepath.Join(coding.AgentDir(), "skills"), false},
		{coding.ModelsFile(), true},
		{filepath.Join(coding.AgentDir(), "mcp.json"), true},
	}
}

// handleUploadPlace 把暂存区文件复制到白名单目的地（harness 直写，不经
// permissions；守卫是白名单根 + EvalSymlinks 复核 + web token）。
// dest 相对路径相对当前会话 cwd。
func (s *Server) handleUploadPlace(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	var req struct {
		Dest      string `json:"dest"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	meta, err := s.uploads.Meta(r.PathValue("id"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	cwd := s.backendFor(r).Status().Cwd
	dest, err := resolvePlacePath(req.Dest, cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 目标已存在：symlink 一律拒绝（防写入时跟随逃逸）；非覆盖冲突 409
	if st, err := os.Lstat(dest); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			writeError(w, http.StatusBadRequest, "destination is a symlink")
			return
		}
		if !req.Overwrite {
			writeError(w, http.StatusConflict, "destination already exists")
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 复制走同目录临时文件 + rename 原子替换：中途失败不留半截目标文件；
	// 复制（可达 100MiB）在锁外，锁只串行化最后的替换
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".doge-place-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	src, err := os.Open(meta.Path)
	if err != nil {
		_ = os.Remove(tmp.Name())
		tmp.Close()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.applyMu.Lock()
	err = os.Rename(tmp.Name(), dest)
	s.applyMu.Unlock()
	if err != nil {
		_ = os.Remove(tmp.Name())
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logx.Infof("[web] upload %s placed to %s", meta.ID, dest)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dest})
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

// handleUploadApply 应用暂存区文件为配置（kind: mcp/models/skills-zip/
// permissions）。双段确认：默认 dry-run 只校验+预览；confirm:true 才写盘并
// 触发 reload。auth.json / mesh.json / settings.json 不提供 apply。
// 各 kind 的写盘段由 applyMu 串行化（防并发 confirm 互相覆盖备份与目标），
// 复制/解压与 reload 在锁外。
func (s *Server) handleUploadApply(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads not available")
		return
	}
	var req struct {
		Kind    string `json:"kind"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	meta, err := s.uploads.Meta(r.PathValue("id"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	b := s.backendFor(r)
	switch req.Kind {
	case "mcp":
		s.applyMcpConfig(w, meta, req.Confirm)
	case "models":
		s.applyModelsConfig(w, b, meta, req.Confirm)
	case "skills-zip":
		s.applySkillsZip(w, b, meta, req.Confirm)
	case "permissions":
		s.applyPermissionsConfig(w, b, meta, req.Confirm)
	default:
		writeError(w, http.StatusBadRequest, "unknown kind (want mcp|models|skills-zip|permissions)")
	}
}

// applyMcpConfig 按社区 mcpServers 格式合并进项目 .doge/mcp.json，confirm 后
// 重建 server 注册表并把新工具组分发到活动会话（等价 /api/mcp/servers +
// /api/mcp/reload）。
func (s *Server) applyMcpConfig(w http.ResponseWriter, meta *uploads.Meta, confirm bool) {
	body, err := os.ReadFile(meta.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	servers, err := parseMcpServersBody(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !confirm {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dryRun": true, "diff": mcpServersSummary(servers)})
		return
	}
	if s.mcpView == nil {
		writeError(w, http.StatusNotFound, "mcp not enabled")
		return
	}
	s.applyMu.Lock()
	err = s.mcpView.UpsertServers(servers)
	s.applyMu.Unlock()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.mcpView.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.redistributeMcp()
	logx.Infof("[web] mcp config applied from upload %s (%d servers)", meta.ID, len(servers))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reloaded": true, "applied": len(servers)})
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
// 请求窗口的 ReloadConfig（模型目录全局生效，其他窗口按需自行 reload）。
func (s *Server) applyModelsConfig(w http.ResponseWriter, b Backend, meta *uploads.Meta, confirm bool) {
	data, err := os.ReadFile(meta.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var models []*ai.Model
	if err := json.Unmarshal(data, &models); err != nil {
		writeError(w, http.StatusBadRequest, "models.json must be a JSON array of model definitions: "+err.Error())
		return
	}
	if len(models) == 0 {
		writeError(w, http.StatusBadRequest, "models.json is empty")
		return
	}
	for _, m := range models {
		if m.ID == "" || m.Provider == "" {
			writeError(w, http.StatusBadRequest, "every model needs id and provider")
			return
		}
	}
	specs := make([]string, 0, len(models))
	for _, m := range models {
		specs = append(specs, m.Provider+"/"+m.ID)
	}
	if !confirm {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dryRun": true,
			"diff": fmt.Sprintf("将导入 %d 个模型定义：\n- %s", len(models), strings.Join(specs, "\n- "))})
		return
	}
	target := coding.ModelsFile()
	s.applyMu.Lock()
	backupFile(target)
	werr := writeFileAtomic(target, data, 0o644)
	s.applyMu.Unlock()
	if werr != nil {
		writeError(w, http.StatusInternalServerError, werr.Error())
		return
	}
	reloaded := true
	reloadErr := ""
	if err := b.ReloadConfig(); err != nil {
		reloaded = false
		reloadErr = err.Error()
	}
	logx.Infof("[web] models.json applied from upload %s (%d models)", meta.ID, len(models))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reloaded": reloaded, "applied": len(models), "error": reloadErr})
}

// applySkillsZip 把 zip 里的技能包安装到全局 skills 目录（覆盖同名技能），
// confirm 后触发 ReloadConfig 热刷新（技能工具与系统提示即时重建）。
func (s *Server) applySkillsZip(w http.ResponseWriter, b Backend, meta *uploads.Meta, confirm bool) {
	tmp, err := os.MkdirTemp("", "doge-skill-zip-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(tmp)
	files, err := uploads.ExtractZip(meta.Path, tmp, s.uploads.Config().MaxFileBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	skills := coding.LoadSkills(tmp)
	if len(skills) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("no SKILL.md found in zip (%d files)", len(files)))
		return
	}
	if !confirm {
		var b strings.Builder
		fmt.Fprintf(&b, "将安装 %d 个技能：", len(skills))
		for _, sk := range skills {
			fmt.Fprintf(&b, "\n- %s: %s", sk.Name, sk.Description)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dryRun": true, "diff": b.String(), "files": len(files)})
		return
	}
	// 安装目标固定为全局 skills 目录（与 --skill 自定义目录解耦）
	dest := filepath.Join(coding.AgentDir(), "skills")
	installed := make([]string, 0, len(skills))
	s.applyMu.Lock()
	for _, sk := range skills {
		srcDir := filepath.Dir(sk.FilePath)
		target := filepath.Join(dest, sk.Name)
		// 覆盖安装：先清掉同名旧技能，否则发现时旧版先到先得
		if err := os.RemoveAll(target); err != nil {
			s.applyMu.Unlock()
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.CopyFS(target, os.DirFS(srcDir)); err != nil {
			s.applyMu.Unlock()
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		installed = append(installed, sk.Name)
	}
	s.applyMu.Unlock()
	reloaded := true
	reloadErr := ""
	if err := b.ReloadConfig(); err != nil {
		reloaded = false
		reloadErr = err.Error()
	}
	logx.Infof("[web] skills installed from upload %s: %s", meta.ID, strings.Join(installed, ","))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reloaded": reloaded, "installed": installed, "error": reloadErr})
}

// applyPermissionsConfig 整体替换全局 permissions.json（先备份 .bak），confirm
// 后触发请求窗口的 ReloadConfig。这是本地 token 边界内的面板操作；远端来向
// 永远到不了这里（mesh config-push 显式拒绝 permissions）。
func (s *Server) applyPermissionsConfig(w http.ResponseWriter, b Backend, meta *uploads.Meta, confirm bool) {
	data, err := os.ReadFile(meta.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	policy, err := validatePermissionsData(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !confirm {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dryRun": true, "diff": permissionsSummary(policy)})
		return
	}
	target := filepath.Join(coding.AgentDir(), "permissions.json")
	s.applyMu.Lock()
	backupFile(target)
	werr := writeFileAtomic(target, data, 0o644)
	s.applyMu.Unlock()
	if werr != nil {
		writeError(w, http.StatusInternalServerError, werr.Error())
		return
	}
	reloaded := true
	reloadErr := ""
	if err := b.ReloadConfig(); err != nil {
		reloaded = false
		reloadErr = err.Error()
	}
	logx.Infof("[web] permissions.json applied from upload %s", meta.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reloaded": reloaded, "error": reloadErr})
}

// validatePermissionsData 解析上传的权限策略（临时文件走标准加载路径）
func validatePermissionsData(data []byte) (*permissions.Policy, error) {
	tmp, err := os.CreateTemp("", "doge-perm-*.json")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return permissions.LoadPolicy(path)
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

// backupFile 现文件存在时备份为 <path>.bak
func backupFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 目标还不存在，无需备份
	}
	_ = os.WriteFile(path+".bak", data, 0o600)
}

// writeFileAtomic 原子落盘：写同目录临时文件后 rename，磁盘满/中途失败
// 不留截断的目标文件
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".doge-apply-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
