import { Call, Events } from "/wails/runtime.js";
window.__booted = true;
/* doge Web console — vanilla JS SPA, no build step. */
"use strict";

const $ = (sel) => document.querySelector(sel);

// Per-window identity: this tab follows its own session. sessionStorage is
// per-tab and survives reloads, so a refresh keeps the same id (and the same
// per-window session view on the server).
const clientId = (() => {
  let id = sessionStorage.getItem("doge-client");
  if (!id) {
    id =
      (crypto.randomUUID && crypto.randomUUID()) ||
      "w" + Math.random().toString(36).slice(2) + Date.now().toString(36);
    sessionStorage.setItem("doge-client", id);
  }
  return id;
})();

// api 桌面端数据层：fetch 调用映射为 typed Service 绑定（响应形状与
// Web API 契约一致），FormData 上传转 base64 服务调用，事件流走 wails Events。
function api(path, opts) {
  opts = opts || {};
  return desktopApi(path, opts);
}

function resOK(data) {
  return { ok: true, status: 200, json: async () => data };
}
function resErr(status, msg) {
  return { ok: false, status, json: async () => ({ error: msg }) };
}
function kindStatus(kind) {
  return { invalid: 400, notfound: 404, busy: 409, readonly: 403, unavailable: 404 }[kind] || 500;
}
function fileToB64(f) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve({
      name: f.name,
      mimeType: f.type || "application/octet-stream",
      data: String(r.result).split(",", 2)[1] || "",
    });
    r.onerror = () => reject(r.error);
    r.readAsDataURL(f);
  });
}

async function desktopApi(path, opts) {
  const method = (opts.method || "GET").toUpperCase();
  const u = new URL(path, location.href);
  const p = u.pathname;
  if (opts.body instanceof FormData) {
    if (p !== "/api/uploads") return resErr(400, "unsupported multipart target");
    const files = [];
    for (const [, v] of opts.body.entries()) {
      if (typeof v !== "string") files.push(await fileToB64(v));
    }
    if (!files.length) return resErr(400, "no file field");
    try {
      return resOK(await Call.ByName("doge/desktop/services.UploadsService.Upload", files));
    } catch (e) {
      return resErr(kindStatus(e.kind), e.message || String(e));
    }
  }
  let body = {};
  if (opts.body != null) {
    try { body = JSON.parse(opts.body); } catch (_) { return resErr(400, "invalid request body"); }
  }
  const S = (name, ...args) => Call.ByName("doge/desktop/services." + name, ...args);
  try {
    if (method === "GET") {
      if (p === "/api/status") return resOK(await S("SessionService.Status"));
      if (p === "/api/stats") return resOK(await S("SessionService.Stats"));
      if (p === "/api/models") return resOK({ models: await S("SessionService.Models", u.searchParams.get("all") === "1") });
      if (p === "/api/session") return resOK(await S("SessionService.Info"));
      if (p === "/api/session/messages") return resOK(await S("SessionService.MessagesBefore", Number(u.searchParams.get("before")) || 0, Number(u.searchParams.get("limit")) || 0));
      if (p === "/api/sessions") return resOK({ sessions: await S("SessionService.List") });
      if (p === "/api/mesh") return resOK(await S("MeshService.Info"));
      if (p === "/api/mesh/config") return resOK(await S("MeshService.ConfigGet"));
      if (p === "/api/mesh/running") return resOK(await S("MeshService.Running"));
      if (p === "/api/permissions") return resOK(await S("ConfigService.PermissionsGet"));
      if (p === "/api/mesh/forwards") return resOK(await S("MeshService.Forwards"));
      if (p === "/api/mcp") return resOK(await S("McpService.Info"));
      if (p === "/api/session/mcp") return resOK(await S("McpService.SessionState"));
      if (p === "/api/skills") return resOK(await S("SkillsService.List"));
      if (p === "/api/session/skills") return resOK(await S("SkillsService.SessionSkills"));
      if (p === "/api/config/models") return resOK(await S("ConfigService.ModelsGet"));
      if (p === "/api/fs/ls") return resOK(await S("ConfigService.FsList", u.searchParams.get("path") || ""));
      if (p === "/api/auth/whoami") return resOK({ ok: true, method: "desktop" });
    } else if (method === "DELETE") {
      if (p.startsWith("/api/mcp/servers/")) { await S("McpService.ServerRemove", decodeURIComponent(p.slice("/api/mcp/servers/".length))); return resOK({ ok: true }); }
      if (p.startsWith("/api/skills/")) return resOK(await S("SkillsService.Delete", decodeURIComponent(p.slice("/api/skills/".length))));
      if (p.startsWith("/api/config/models/")) {
        const segs = p.slice("/api/config/models/".length).split("/").map(decodeURIComponent);
        if (segs.length === 2) return resOK(await S("ConfigService.ModelsDelete", segs[0], segs[1]));
      }
      if (p.startsWith("/api/mesh/forwards/")) { await S("MeshService.ForwardRemove", decodeURIComponent(p.slice("/api/mesh/forwards/".length))); return resOK({ ok: true }); }
    } else {
      if (p === "/api/auth/login" || p === "/api/auth/logout") return resOK({ ok: true });
      if (p === "/api/prompt") { await S("SessionService.Prompt", body.text || "", body.attachments || []); return resOK({ ok: true }); }
      if (p === "/api/abort") { await S("SessionService.Abort"); return resOK({ ok: true }); }
      if (p === "/api/compact") { await S("SessionService.Compact"); return resOK({ ok: true }); }
      if (p === "/api/reload") return resOK(await S("ConfigService.Reload"));
      if (p === "/api/config/models") return resOK(await S("ConfigService.ModelsUpsert", body));
      if (p === "/api/model") { await S("SessionService.SetModel", body.spec || ""); return resOK({ ok: true }); }
      if (p === "/api/thinking") { await S("SessionService.SetThinking", body.level || ""); return resOK({ ok: true }); }
      if (p === "/api/session/new") { await S("SessionService.New", body.cwd || ""); return resOK({ ok: true }); }
      if (p === "/api/session/switch") { await S("SessionService.Switch", body.id || ""); return resOK({ ok: true }); }
      if (p === "/api/session/delete") { await S("SessionService.Delete", body.id || ""); return resOK({ ok: true }); }
      if (p === "/api/session/claim") return resOK({ owned: true, queued: false });
      if (p === "/api/session/takeover") return resOK({ owned: true, prevOwner: "" });
      if (p === "/api/turn/edit") { await S("SessionService.TurnEdit", body.seq, body.text || ""); return resOK({ ok: true }); }
      if (p === "/api/question/answer") { await S("SessionService.QuestionAnswer", body.id || "", body.text || ""); return resOK({ ok: true }); }
      if (p === "/api/permission/answer") {
        await S("SessionService.PermissionAnswer", body.id || "", !!body.allowOnce, !!body.allowAlways, !!body.deny);
        return resOK({ ok: true });
      }
      if (p === "/api/permissions") return resOK(await S("ConfigService.PermissionsPut", body));
      if (p === "/api/mesh/config") return resOK(await S("MeshService.ConfigPut", body));
      if (p === "/api/mesh/start") { await S("MeshService.Start"); return resOK({ ok: true }); }
      if (p === "/api/mesh/stop") { await S("MeshService.Stop"); return resOK({ ok: true }); }
      if (p === "/api/mesh/forwards") { await S("MeshService.ForwardAdd", body); return resOK({ ok: true }); }
      if (p === "/api/mcp/reconnect") { await S("McpService.Reconnect", body.server || ""); return resOK({ ok: true }); }
      if (p === "/api/mcp/enable") { await S("McpService.Enable", body.server || ""); return resOK({ ok: true }); }
      if (p === "/api/mcp/disable") { await S("McpService.Disable", body.server || ""); return resOK({ ok: true }); }
      if (p === "/api/mcp/reload") { await S("McpService.Reload"); return resOK({ ok: true }); }
      if (p === "/api/mcp/servers") { await S("McpService.UpsertServers", body); return resOK({ ok: true }); }
      if (p === "/api/session/mcp") return resOK(await S("McpService.SessionSet", !!body.enabled));
      if (p === "/api/uploads/x/apply") { /* 占位，真实 id 走前缀分支 */ }
      if (p.startsWith("/api/mcp/servers/")) {
        const segs = p.slice("/api/mcp/servers/".length).split("/");
        if (segs.length === 2 && segs[1] === "direct") { await S("McpService.ServerDirect", decodeURIComponent(segs[0]), !!body.enabled); return resOK({ ok: true }); }
        if (segs.length === 2 && segs[1] === "autostart") { await S("McpService.ServerAutoStart", decodeURIComponent(segs[0]), !!body.enabled); return resOK({ ok: true }); }
      }
      if (p.startsWith("/api/skills/") && p.endsWith("/enabled")) {
        const name = decodeURIComponent(p.slice("/api/skills/".length, -"/enabled".length));
        return resOK(await S("SkillsService.SetEnabled", name, !!body.enabled));
      }
      if (p.startsWith("/api/uploads/") && p.endsWith("/apply")) {
        const id = decodeURIComponent(p.slice("/api/uploads/".length, -"/apply".length));
        return resOK(await S("UploadsService.Apply", id, body.kind || "", !!body.confirm));
      }
      if (p.startsWith("/api/mesh/forwards/")) {
        const rest = p.slice("/api/mesh/forwards/".length).split("/");
        if (rest.length === 2 && (rest[1] === "enable" || rest[1] === "disable")) {
          await S("MeshService.ForwardSetEnabled", decodeURIComponent(rest[0]), rest[1] === "enable");
          return resOK({ ok: true });
        }
      }
    }
    console.warn("[desktop] 未接线的 API 调用：", method, path);
    return resErr(404, "not wired: " + method + " " + p);
  } catch (e) {
    return resErr(kindStatus(e.kind), e.message || String(e));
  }
}

const state = {
  connected: false,
  busy: false,
  messages: [],       // committed transcript (from /api/session + message_end)
  streaming: null,    // partial assistant message
  streamingEl: null,
  toolChips: new Map(),
  permAsk: null,
  question: null,
  readOnly: false,
  retry: 0,
  ws: null,
  sessionId: "",
  cwd: "",
  globalCwd: "",     // 服务进程启动目录（新会话不选项目时的落点）
  models: [],         // /api/models 列表（模型选择浮层数据源）
  modelSpec: "",      // 当前模型 spec（provider/id）
  thinking: "",       // 当前思考级别（low/high/xhigh）
  // sessionKind is "" for a user session, "remote" for a slave's remote-task
  // session; sessionSourceName labels the node that dispatched it (the UIs
  // show it on the user message cards instead of "user").
  sessionKind: "",
  sessionSourceName: "",
  sessionSourcePeer: "",
  // Input history: submitted messages recalled with ArrowUp/Down (newest
  // first); historyIdx -1 = not browsing (showing the draft).
  history: [],
  historyIdx: -1,
  historyDraft: "",
  // msgEls maps committed message objects to their DOM node, so a tool
  // result can re-render the assistant card it belongs to.
  msgEls: new Map(),
  // pendingUploads are staged uploads (POST /api/uploads) waiting to be sent
  // with the next message: { id, name, size, mimeType }.
  pendingUploads: [],
};

/* ---------------- auth ---------------- */
function init() {
  // The login form must be wired before any login is attempted: bindEvents
  // only runs after a successful login, so without this the manual login
  // page's submit would just reload the page.
  $("#login-form").addEventListener("submit", (e) => {
    e.preventDefault();
    doLogin($("#token-input").value.trim());
  });

  const tok = new URLSearchParams(location.search).get("token");
  if (tok) {
    history.replaceState(null, "", location.pathname);
    doLogin(tok.trim());
    return;
  }
  api("/api/status", { credentials: "same-origin" })
    .then((r) => (r.ok ? enterApp() : showLogin()))
    .catch(showLogin);
}

function showLogin() {
  $("#login").classList.remove("hidden");
  $("#token-input").focus();
}

function doLogin(token) {
  const btn = $("#login-btn");
  btn.disabled = true;
  api("/api/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ token }),
  })
    .then((r) => r.json())
    .then((j) => {
      if (j.ok) {
        enterApp();
      } else {
        throw new Error(j.error || "登录失败");
      }
    })
    .catch((e) => {
      $("#login-error").textContent = String(e.message || e);
      $("#login-error").classList.remove("hidden");
    })
    .finally(() => (btn.disabled = false));
}

/* ---------------- entry ---------------- */
function enterApp() {
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  // 窄屏默认收起覆盖式侧栏，避免一进来就盖住对话区
  if (isNarrow()) $("#app").classList.add("sb-collapsed");
  bindEvents();
  loadModels();
  loadSession();
  populateSessions();
  connectWS();
  fetchStatus(); // 进场立即拉一次状态（cwd/模型/思考级别）
  // 周期刷新状态：提供商卡住/流自愈结束这类没有事件驱动的变化也能及时反映到 UI，
  // 避免看起来"假死"。
  setInterval(fetchStatus, 5000);
}

function bindEvents() {

  const input = $("#input");
  input.addEventListener("keydown", (e) => {
    // Ignore Enter used to confirm an IME candidate (Chinese input): e.isComposing
    // is the modern signal, keyCode 229 the legacy one.
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      sendMessage();
      return;
    }
    if (e.key === "ArrowUp" || e.key === "ArrowDown") {
      // In a multi-line input the arrows move the cursor between lines; only a
      // single-line input uses them for history recall (matches the TUI).
      if (input.value.includes("\n")) return;
      e.preventDefault();
      recallHistory(e.key === "ArrowUp" ? -1 : 1);
    }
  });
  input.addEventListener("input", () => resizeInput(input));
  $("#send-btn").addEventListener("click", sendMessage);
  // 附件：📎 选择、输入框粘贴、拖拽到输入区
  // ＋菜单：附件 / 技能（对齐桌面端左下角 + 唤起的添加面板）；粘贴附件图片
  // 走输入框的 paste 监听，不在此列。
  $("#attach-btn").addEventListener("click", (e) => {
    e.stopPropagation();
    openAddMenu(e.currentTarget);
  });
  $("#file-input").addEventListener("change", (e) => {
    uploadFiles(e.target.files);
    e.target.value = ""; // 允许连续选择同一文件
  });
  // Skills 面板的 zip 安装入口（input 挂在 composer 下，全局绑定一次）
  $("#skill-zip-input").addEventListener("change", (e) => {
    installSkillZip(e.target.files[0]);
    e.target.value = "";
  });
  input.addEventListener("paste", (e) => {
    if (e.clipboardData && e.clipboardData.files.length) {
      e.preventDefault();
      uploadFiles(e.clipboardData.files);
    }
  });
  const composer = $("#composer");
  composer.addEventListener("dragover", (e) => {
    e.preventDefault();
    composer.classList.add("drag-over");
  });
  composer.addEventListener("dragleave", () => composer.classList.remove("drag-over"));
  composer.addEventListener("drop", (e) => {
    e.preventDefault();
    composer.classList.remove("drag-over");
    if (e.dataTransfer && e.dataTransfer.files.length) uploadFiles(e.dataTransfer.files);
  });
  $("#abort-btn").addEventListener("click", () => {
    api("/api/abort", { method: "POST" })
      .then((r) => {
        if (r.ok) {
          $("#abort-btn").classList.add("hidden");
          setStatusText("已发送中断");
        } else {
          // 只读（403）或运行已结束（409）：刷新状态让按钮状态恢复正确。
          setStatusText(r.status === 403 ? "无法中断：会话只读，请先接管" : "中断失败：运行可能已结束");
          fetchStatus();
        }
      })
      .catch(() => {});
  });
  // 上下文容量 hover 面板（悬停显示，移开即收）
  const ctxBtn = $("#ctx-btn");
  ctxBtn.addEventListener("mouseenter", () => {
    clearTimeout(state.ctxHideTimer);
    showCtxPanel();
  });
  ctxBtn.addEventListener("mouseleave", () => {
    state.ctxHideTimer = setTimeout(hideCtxPanel, 220);
  });

  // 设置中心：模型 / MCP / 技能 三个 tab（MCP 面板的按钮走事件委托）
  $("#settings-btn").addEventListener("click", () => openSettings());
  $("#settings-close").addEventListener("click", () => $("#settings").classList.add("hidden"));
  for (const tab of document.querySelectorAll(".settings-tab")) {
    tab.addEventListener("click", () => openSettings(tab.dataset.tab));
  }
  $("#settings-body").addEventListener("click", onMcpAction);
  // 设置导航底部：Mesh / 重载 / 登出
  $("#nav-reload").addEventListener("click", () => {
    api("/api/reload", { method: "POST" })
      .then((r) => setStatusText(r.ok ? "已重载 models/auth/permission 配置" : "重载失败"))
      .catch(() => setStatusText("重载配置失败"));
  });
  $("#nav-logout").addEventListener("click", async () => {
    try { await api("/api/auth/logout", { method: "POST" }); } catch (_) {}
    location.reload();
  });
  // 添加/编辑模型弹窗
  $("#model-form-close").addEventListener("click", () => $("#model-form").classList.add("hidden"));
  $("#mf-cancel").addEventListener("click", () => $("#model-form").classList.add("hidden"));
  $("#model-form").addEventListener("click", (e) => {
    if (e.target === $("#model-form")) $("#model-form").classList.add("hidden");
  });
  // Composer 底行：思考级别 / 模型选择浮层；顶行：项目目录
  $("#thinking-btn").addEventListener("click", (e) => {
    e.stopPropagation();
    openThinkingMenu(e.currentTarget);
  });
  $("#model-btn").addEventListener("click", (e) => {
    e.stopPropagation();
    openModelMenu(e.currentTarget);
  });
  // 点击浮层外任意处关闭
  document.addEventListener("click", closePopovers);
  // Session sidebar: collapse/expand, new session, item open/delete.
  $("#sidebar-toggle").addEventListener("click", toggleSidebar);
  $("#sb-collapse").addEventListener("click", toggleSidebar);
  // 移动端：点击遮罩收起覆盖式侧栏（桌面端遮罩 display:none，不会命中）
  $("#sb-backdrop").addEventListener("click", toggleSidebar);
  // 新建会话：独立窗口，选项目目录 + 首条消息
  $("#sb-new").addEventListener("click", () => {
    closeSidebar();
    openNewTask();
  });
  // Delegate clicks on session items (open) and their delete buttons.
  $("#sess-list").addEventListener("click", async (e) => {
    const del = e.target.closest(".si-del");
    if (del) {
      const id = del.dataset.id;
      if (!(await desktopConfirm("确定删除该会话？此操作不可恢复。"))) return;
      api("/api/session/delete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id }),
      })
        .then((r) => {
          if (!r.ok) {
            return r.json().then((j) => {
              throw new Error(j.error || "删除会话失败");
            });
          }
          setStatusText("已删除会话");
          populateSessions();
        })
        .catch((e) => setStatusText(String(e.message || e)));
      return;
    }
    const item = e.target.closest(".sess-item[data-id]");
    if (!item || item.classList.contains("current") || state.switching) return;
    closeSidebar();
    state.switching = true;
    item.classList.add("loading");
    setStatusText("正在切换会话…");
    api("/api/session/switch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id: item.dataset.id }),
    })
      .then((r) => {
        if (!r.ok) {
          return r.json().then((j) => {
            throw new Error(j.error || "切换会话失败");
          });
        }
        // Success path: the loading state clears on the session_reset
        // broadcast (resetWebConversation), which fires once the server has
        // actually opened the target session.
      })
      .catch((e) => {
        clearSwitching();
        setStatusText(String(e.message || e));
      });
  });

  // Permission modal buttons.
  $("#perm-allow-once").addEventListener("click", () => permResponse({ allowOnce: true }));
  $("#perm-allow-always").addEventListener("click", () => permResponse({ allowAlways: true }));
  $("#perm-deny").addEventListener("click", () => permResponse({ deny: true }));

  // Remote-task question modal.
  $("#question-submit").addEventListener("click", submitQuestion);
  $("#question-input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      submitQuestion();
    }
  });

  // Force takeover of the current session's write control.
  $("#takeover-btn").addEventListener("click", takeoverControl);
}

function toggleSidebar() {
  $("#app").classList.toggle("sb-collapsed");
}

// 窄屏（移动端）侧栏是覆盖层：进入时默认收起，切换/新建会话后也自动收起，
// 避免盖住对话区；桌面端 docked 侧栏不受影响。
function isNarrow() {
  return window.matchMedia("(max-width: 720px)").matches;
}
function closeSidebar() {
  if (isNarrow()) $("#app").classList.add("sb-collapsed");
}

/* ---------------- session sidebar ---------------- */
function populateSessions() {
  api("/api/sessions", { credentials: "same-origin" })
    .then((r) => r.json())
    .then((j) => {
      const list = $("#sess-list");
      list.textContent = "";
      const sessions = (j.sessions || []).slice(0, 50); // newest first
      // Keep the user's own sessions and the slave's remote-task forks apart:
      // remote sessions go into a collapsible section so they never clutter the
      // normal list, but stay reachable for inspection.
      const userSess = sessions.filter((s) => s.kind !== "remote");
      const remoteSess = sessions.filter((s) => s.kind === "remote");

      const makeItem = (s) => {
        const item = document.createElement("div");
        item.className = "sess-item" + (s.id === state.sessionId ? " current" : "");
        item.dataset.id = s.id;
        item.title = "#" + s.id.slice(0, 8) + " · " + (s.cwd || "");

        const preview = document.createElement("div");
        preview.className = "si-preview" + (s.firstMessage ? "" : " muted");
        preview.textContent = s.firstMessage || "（无消息）";
        item.appendChild(preview);

        const meta = document.createElement("div");
        meta.className = "si-meta";
        const idSpan = document.createElement("span");
        idSpan.textContent = "#" + s.id.slice(0, 8);
        const cnt = document.createElement("span");
        cnt.textContent = s.messageCount + " 条";
        const when = document.createElement("span");
        when.textContent = fmtTime(s.timestamp);
        meta.appendChild(idSpan);
        meta.appendChild(cnt);
        meta.appendChild(when);
        const cwdSpan = document.createElement("span");
        cwdSpan.className = "si-cwd";
        cwdSpan.textContent = shortCwd(s.cwd);
        cwdSpan.title = s.cwd || "";
        meta.appendChild(cwdSpan);
        item.appendChild(meta);

        const del = document.createElement("button");
        del.className = "si-del";
        del.textContent = "✕";
        del.title = "删除会话";
        del.dataset.id = s.id;
        if (s.id === state.sessionId) {
          del.title = "不能删除当前会话";
          del.disabled = true;
        }
        item.appendChild(del);
        return item;
      };

      if (!userSess.length && !remoteSess.length) {
        const empty = document.createElement("div");
        empty.className = "muted";
        empty.textContent = "暂无历史会话";
        list.appendChild(empty);
        return;
      }

      // 按项目（会话 cwd）分组，对齐桌面端侧栏；组内保持时间倒序，
      // 当前会话所在项目组默认展开并置顶。
      const groups = new Map();
      for (const s of userSess) {
        const key = s.cwd || "";
        if (!groups.has(key)) groups.set(key, []);
        groups.get(key).push(s);
      }
      const entries = [...groups.entries()].sort((a, b) => {
        const ca = a[0] === state.cwd, cb = b[0] === state.cwd;
        if (ca !== cb) return ca ? -1 : 1;
        return b[1][0].timestamp - a[1][0].timestamp;
      });
      const folderSvg = '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z"/></svg>';
      const plusSvg = '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>';
      for (const [cwd, items] of entries) {
        const proj = document.createElement("details");
        proj.className = "proj-group" + (cwd === state.cwd ? " current" : "");
        proj.open = cwd === state.cwd || entries.length === 1;
        const sum = document.createElement("summary");
        sum.title = cwd || "";
        const icon = document.createElement("span");
        icon.className = "proj-icon";
        icon.innerHTML = folderSvg;
        sum.appendChild(icon);
        const nm = document.createElement("span");
        nm.className = "proj-name";
        nm.textContent = cwd ? cwd.split("/").filter(Boolean).pop() : "未知目录";
        sum.appendChild(nm);
        const actions = document.createElement("span");
        actions.className = "proj-actions";
        const add = document.createElement("button");
        add.className = "proj-add";
        add.title = "新建任务";
        add.innerHTML = plusSvg;
        add.addEventListener("click", (e) => {
          e.preventDefault(); // 不收起分组
          e.stopPropagation();
          closeSidebar();
          newSessionIn(cwd);
        });
        actions.appendChild(add);
        sum.appendChild(actions);
        proj.appendChild(sum);
        for (const s of items) proj.appendChild(makeItem(s));
        list.appendChild(proj);
      }

      if (remoteSess.length) {
        const head = document.createElement("div");
        head.className = "sess-remote-head";
        head.textContent = "远端任务会话 (" + remoteSess.length + ")";
        head.title = "点击展开/收起由远端任务创建的会话";
        const box = document.createElement("div");
        box.className = "sess-remote hidden";
        for (const s of remoteSess) box.appendChild(makeItem(s));
        head.addEventListener("click", () => box.classList.toggle("hidden"));
        list.appendChild(head);
        list.appendChild(box);
      }
    })
    .catch(() => {});
}

function fmtTime(ts) {
  if (!ts) return "";
  const d = new Date(ts);
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  const hm = String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  if (sameDay) return hm;
  return `${d.getMonth() + 1}-${String(d.getDate()).padStart(2, "0")} ${hm}`;
}

function shortCwd(cwd) {
  if (!cwd) return "";
  const parts = cwd.split("/").filter(Boolean);
  return "…" + parts.slice(-1)[0];
}

// resetWebConversation clears the local transcript after a session switch or
// /new broadcast, so every connected tab reloads the active session.
function resetWebConversation() {
  clearSwitching();
  state.messages = [];
  clearStreaming();
  clearToolChips();
  loadSession();
  fetchStatus();
  populateSessions();
}

// clearSwitching drops the session-switch affordances: the item spinner and
// the guard that blocks double-submits while a switch is in flight.
function clearSwitching() {
  state.switching = false;
  for (const el of document.querySelectorAll(".sess-item.loading")) {
    el.classList.remove("loading");
  }
}

function permResponse(response) {
  const ask = state.permAsk;
  state.permAsk = null;
  $("#perm").classList.add("hidden");
  if (ask && state.ws && state.ws.readyState === WebSocket.OPEN) {
    state.ws.send(JSON.stringify({ type: "permission_response", id: ask.id, response }));
  }
}

// showQuestion surfaces a pending remote-task question (the master's bridge
// is blocking on the answer). The reply is posted to /api/question/answer and
// routed to the waiting bridge out-of-band.
function showQuestion(m) {
  const q = m.question || {};
  state.question = q;
  const el = $("#question-detail");
  el.textContent = "";
  const pre = document.createElement("pre");
  pre.textContent =
    (q.prompt || "") + "\n\n" +
    (q.from ? "来源节点: " + q.from + "\n" : "") +
    (q.task ? "任务: " + q.task + "\n" : "");
  el.appendChild(pre);
  $("#question-input").value = "";
  $("#question").classList.remove("hidden");
  $("#question-input").focus();
}

function submitQuestion() {
  const q = state.question;
  const text = $("#question-input").value.trim();
  if (!q || !text) return;
  state.question = null;
  $("#question").classList.add("hidden");
  api("/api/question/answer", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ id: q.id, text }),
  }).catch((e) => setStatusText(String(e.message || e)));
}

/* ---------------- turn editing ---------------- */
// userSeqOf returns the ordinal (0-based) of msg among user messages in the
// current transcript, matching the backend's message view.
function userSeqOf(msg) {
  let seq = 0;
  for (const m of state.messages) {
    if (m === msg) return seq;
    if (m.role === "user") seq++;
  }
  return -1;
}

// startEditUserMessage swaps a user message body for an inline editor.
function startEditUserMessage(wrap, msg) {
  const seq = userSeqOf(msg);
  if (seq < 0) return;
  const body = wrap.querySelector(".body");
  body.textContent = "";
  const box = document.createElement("div");
  box.className = "msg-editor";
  const ta = document.createElement("textarea");
  ta.value = plainText(normalizeContent(msg.content));
  ta.rows = 3;
  const btns = document.createElement("div");
  btns.className = "msg-editor-btns";
  const save = document.createElement("button");
  save.textContent = "保存并重新执行";
  const cancel = document.createElement("button");
  cancel.textContent = "取消";
  btns.appendChild(save);
  btns.appendChild(cancel);
  box.appendChild(ta);
  box.appendChild(btns);
  body.appendChild(box);
  ta.focus();
  save.addEventListener("click", () => {
    const text = ta.value.trim();
    if (!text) {
      setStatusText("消息不能为空");
      return;
    }
    saveEdit(seq, text);
  });
  cancel.addEventListener("click", () => renderAll());
}

// saveEdit rewinds to the seq-th user message (dropping it and everything
// after) and asks the server to re-run with the edited text. The truncation
// is applied optimistically so the incoming stream appends to the rewritten
// transcript; a failed request reloads the server truth.
function saveEdit(seq, text) {
  let idx = -1;
  let seen = 0;
  for (let i = 0; i < state.messages.length; i++) {
    if (state.messages[i].role === "user") {
      if (seen === seq) {
        idx = i;
        break;
      }
      seen++;
    }
  }
  if (idx < 0) {
    setStatusText("消息位置无效");
    return;
  }
  state.messages = state.messages.slice(0, idx);
  clearStreaming();
  clearToolChips();
  renderAll();
  api("/api/turn/edit", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ seq, text }),
  })
    .then((r) => {
      if (!r.ok) {
        return r.json().then((j) => {
          throw new Error(j.error || "编辑失败");
        });
      }
    })
    .catch((e) => {
      setStatusText(String(e.message || e));
      loadSession(); // restore server truth on failure
    });
}

/* ---------------- session stats ---------------- */
// 上下文容量 hover 面板：悬停按钮弹出、移开即收（对齐桌面端交互）
function showCtxPanel() {
  let panel = document.getElementById("ctx-panel");
  if (!panel) {
    panel = document.createElement("div");
    panel.id = "ctx-panel";
    panel.className = "ctx-panel";
    panel.addEventListener("mouseenter", () => clearTimeout(state.ctxHideTimer));
    panel.addEventListener("mouseleave", () => {
      state.ctxHideTimer = setTimeout(hideCtxPanel, 220);
    });
    document.body.appendChild(panel);
  }
  panel.classList.remove("hidden");
  const btn = $("#ctx-btn");
  const r = btn.getBoundingClientRect();
  panel.style.left = Math.max(8, r.right - 360) + "px";
  panel.style.top = Math.max(8, r.top - panel.offsetHeight - 8) + "px";
  panel.textContent = "加载中…";
  api("/api/stats", { credentials: "same-origin" })
    .then((r2) => r2.json())
    .then((st) => {
      renderCtxPanel(panel, st);
      const rect = btn.getBoundingClientRect();
      panel.style.left = Math.max(8, rect.right - 360) + "px";
      panel.style.top = Math.max(8, rect.top - panel.offsetHeight - 8) + "px";
    })
    .catch(() => {
      panel.textContent = "加载统计失败";
    });
}

function hideCtxPanel() {
  const panel = document.getElementById("ctx-panel");
  if (panel) panel.classList.add("hidden");
}

function renderCtxPanel(panel, st) {
  panel.textContent = "";
  // 上下文用量：优先后端字段，回退本地最后一条 usage
  let ctxTokens = st.contextTokens || 0;
  let ctxWindow = st.contextWindow || 0;
  if (!ctxTokens) {
    for (let i = state.messages.length - 1; i >= 0; i--) {
      const u = state.messages[i] && state.messages[i].usage;
      if (u && (u.input || u.cacheRead)) {
        ctxTokens = (u.input || 0) + (u.cacheRead || 0);
        break;
      }
    }
  }
  if (!ctxWindow) {
    const cur = (state.models || []).find((m) => m.spec === state.modelSpec);
    ctxWindow = cur ? cur.contextWindow : 0;
  }
  const pct = ctxWindow ? (ctxTokens / ctxWindow) * 100 : 0;

  const head = document.createElement("div");
  head.className = "ctx-head";
  const title = document.createElement("span");
  title.className = "ctx-title";
  title.textContent = "上下文容量";
  head.appendChild(title);
  const num = document.createElement("span");
  num.className = "ctx-num";
  num.textContent = ctxWindow
    ? `${fmtNum(ctxTokens)} / ${fmtNum(ctxWindow)}（${pct.toFixed(1)}%）`
    : `${fmtNum(ctxTokens)} tokens`;
  head.appendChild(num);
  panel.appendChild(head);

  const bar = document.createElement("div");
  bar.className = "ctx-bar";
  const fill = document.createElement("div");
  fill.className = "ctx-fill";
  fill.style.width = Math.min(100, pct).toFixed(1) + "%";
  bar.appendChild(fill);
  panel.appendChild(bar);

  const row = (k, v) => {
    const r = document.createElement("div");
    r.className = "ctx-row";
    const kk = document.createElement("span");
    kk.textContent = k;
    const vv = document.createElement("span");
    vv.textContent = v;
    r.appendChild(kk);
    r.appendChild(vv);
    return r;
  };
  panel.appendChild(row("消息", `${st.messages}（用户 ${st.userMessages} / 助手 ${st.assistantMessages} / 工具 ${st.toolMessages}）`));
  panel.appendChild(row("平均缓存命中率", (st.cacheHitPct || 0).toFixed(1) + "%"));
  panel.appendChild(row("输入", fmtNum(st.inputTokens)));
  panel.appendChild(row("输出", fmtNum(st.outputTokens)));
  panel.appendChild(row("累计花费", (st.currency || "$") + st.cost.toFixed(4)));
  if (st.cwd) panel.appendChild(row("目录", st.cwd));

  const foot = document.createElement("div");
  foot.className = "ctx-foot";
  const compact = document.createElement("button");
  compact.textContent = "▦ 压缩上下文";
  compact.title = "压缩历史，降低上下文占用";
  compact.addEventListener("click", () => {
    compact.disabled = true;
    api("/api/compact", { method: "POST" })
      .then(() => setStatusText("已发起上下文压缩"))
      .catch(() => setStatusText("压缩失败"))
      .finally(() => (compact.disabled = false));
  });
  foot.appendChild(compact);
  panel.appendChild(foot);
}

/* ---------------- mesh network ---------------- */
// peerNames caches peer id → node name from the latest /api/mesh fetch, so
// remote-task cards can show "nas-code" instead of a bare peer id.
const peerNames = new Map();

function openMesh() {
  const body = $("#settings-body");
  body.textContent = "";
  const loading = document.createElement("div");
  loading.className = "mesh-muted";
  loading.textContent = "加载中…";
  body.appendChild(loading);
  api("/api/mesh", { credentials: "same-origin" })
    .then((r) =>
      r.status === 404
        ? Promise.reject(new Error("mesh 未启用（需 -tags mesh 构建并 --mesh 启动）"))
        : r.json()
    )
    .then((m) => {
      peerNames.clear();
      for (const p of m.peers || []) {
        if (p && p.id && p.profile && p.profile.name) peerNames.set(p.id, p.profile.name);
      }
      if (state.settingsTab !== "mesh") return;
      renderMeshModal(body, m);
    })
    .catch((e) => {
      body.textContent = String(e.message || e);
    });
}

// sessionPeerName resolves a node name for a peer id ("" when unknown).
function sessionPeerName(peer) {
  return peerNames.get(peer) || "";
}

function renderMeshModal(body, m) {
  body.textContent = "";
  body.appendChild(meshNodeCard(m.self || {}, true, null, m.addrs || []));

  const peers = m.peers || [];
  const sec = document.createElement("div");
  sec.className = "mesh-section";
  const sh = document.createElement("div");
  sh.className = "mesh-section-title";
  sh.textContent = peers.length ? "节点 " + peers.length + " 个" : "无已连接节点";
  sec.appendChild(sh);
  for (const p of peers) sec.appendChild(meshNodeCard(p.profile || {}, false, p, p.addrs || []));
  body.appendChild(sec);

  const tasks = m.tasks || [];
  const tsec = document.createElement("div");
  tsec.className = "mesh-section";
  const th = document.createElement("div");
  th.className = "mesh-section-title";
  th.textContent = tasks.length ? "在途远端任务 " + tasks.length + " 个" : "无在途远端任务";
  tsec.appendChild(th);
  for (const t of tasks) tsec.appendChild(meshTaskCard(t));
  body.appendChild(tsec);
}

function meshNodeCard(profile, isSelf, peer, addrs) {
  const card = document.createElement("div");
  card.className = "mesh-node" + (isSelf ? " self" : "");
  const head = document.createElement("div");
  head.className = "mesh-node-head";
  const name = document.createElement("span");
  name.className = "mesh-node-name";
  name.textContent = (profile.name || "(unnamed)") + (isSelf ? "  · 本节点" : "");
  head.appendChild(name);
  if (profile.peerId) {
    const id = document.createElement("span");
    id.className = "mesh-node-id";
    id.textContent = profile.peerId.slice(0, 12);
    id.title = profile.peerId;
    head.appendChild(id);
  }
  card.appendChild(head);

  // 徽标行：任务接受态 / 负载 / 延迟 / 版本 / 平台
  const meta = document.createElement("div");
  meta.className = "mesh-node-meta";
  const acc = document.createElement("span");
  acc.className = "mesh-badge " + (profile.accepting ? "ok" : "no");
  acc.textContent = profile.accepting ? "接受任务" : "不接受任务";
  meta.appendChild(acc);
  if (profile.maxLoad || profile.load) {
    meta.appendChild(meshMetaSpan("负载 " + (profile.load || 0) + "/" + (profile.maxLoad || 0)));
  }
  if (peer && peer.latency) {
    meta.appendChild(meshMetaSpan("延迟 " + meshFmtMs(peer.latency)));
  }
  if (profile.version) {
    meta.appendChild(meshMetaSpan("v" + profile.version));
  }
  if (profile.os || profile.arch) {
    meta.appendChild(meshMetaSpan([profile.os, profile.arch].filter(Boolean).join("/")));
  }
  card.appendChild(meta);

  // 角色与能力
  const roles = profile.roles || [];
  if (roles.length) {
    const chips = document.createElement("div");
    chips.className = "mesh-chip-row";
    chips.textContent = "roles: " + roles.join(", ");
    card.appendChild(chips);
  }
  if (peer && peer.caps && peer.caps.length) {
    const chips = document.createElement("div");
    chips.className = "mesh-chip-row";
    chips.textContent = "caps: " + peer.caps.join(", ");
    card.appendChild(chips);
  }

  // 明细行：模型 / 可用模型 / 工具 / 技能 / MCP / 工作目录
  const info = document.createElement("div");
  info.className = "mesh-node-info";
  const addInfo = (k, v, title) => {
    const r = document.createElement("div");
    r.className = "mesh-info-row";
    const kk = document.createElement("span");
    kk.className = "mesh-info-k";
    kk.textContent = k;
    const vv = document.createElement("span");
    vv.className = "mesh-info-v";
    vv.textContent = v;
    if (title) vv.title = title;
    r.appendChild(kk);
    r.appendChild(vv);
    info.appendChild(r);
  };
  if (profile.model) addInfo("当前模型", profile.model);
  if (profile.models && profile.models.length) {
    addInfo("可用模型", profile.models.length + " 个: " + profile.models.join(", "), profile.models.join("\n"));
  }
  if (profile.tools && profile.tools.length) addInfo("工具", profile.tools.length + " 个: " + profile.tools.join(", "), profile.tools.join("\n"));
  if (profile.skills && profile.skills.length) addInfo("技能", profile.skills.length + " 个: " + profile.skills.join(", "), profile.skills.join("\n"));
  if (profile.mcp && profile.mcp.length) addInfo("MCP", profile.mcp.length + " 个端点");
  if (profile.cwds && profile.cwds.length) addInfo("工作目录", profile.cwds.join(", "));
  if (addrs && addrs.length) {
    addInfo(isSelf ? "监听地址" : "地址", addrs[0] + (addrs.length > 1 ? `（共 ${addrs.length} 个）` : ""), addrs.join("\n"));
  }
  if (info.childNodes.length) card.appendChild(info);

  return card;
}

// meshMetaSpan 徽标行里的单个补充信息
function meshMetaSpan(text) {
  const el = document.createElement("span");
  el.className = "mesh-muted";
  el.textContent = text;
  return el;
}

// meshFmtMs 把 ns 时长格式化为可读延迟
function meshFmtMs(ns) {
  const ms = ns / 1e6;
  return ms >= 100 ? Math.round(ms) + "ms" : ms.toFixed(1) + "ms";
}

function meshTaskCard(t) {
  const card = document.createElement("div");
  card.className = "mesh-task";
  // 头部一行：任务号 + 状态徽标 + 节点；progress 单独成块并钳制高度，
  // 避免长结果把面板撑爆。
  const head = document.createElement("div");
  head.className = "mesh-task-head";
  if (t.taskId) {
    const id = document.createElement("span");
    id.className = "mesh-task-id";
    id.textContent = t.taskId.slice(0, 12);
    head.appendChild(id);
  }
  const st = document.createElement("span");
  st.className = "mesh-badge " + remoteTaskBadgeClass(t.status);
  st.textContent = remoteTaskStatusLabels[t.status] || t.status || "";
  head.appendChild(st);
  if (t.peer) {
    const peer = document.createElement("span");
    peer.className = "mesh-muted";
    peer.textContent = sessionPeerName(t.peer) || t.peer.slice(0, 12);
    peer.title = t.peer;
    head.appendChild(peer);
  }
  card.appendChild(head);
  if (t.progress) {
    const prog = document.createElement("div");
    prog.className = "mesh-task-progress";
    prog.textContent = t.progress;
    prog.title = "点击展开/收起";
    prog.addEventListener("click", () => prog.classList.toggle("expanded"));
    card.appendChild(prog);
  }
  return card;
}

function fmtNum(n) {
  if (!n) return "0";
  if (n < 1000) return String(n);
  if (n < 10000) return (n / 1000).toFixed(1) + "k";
  if (n < 1000000) return Math.round(n / 1000) + "k";
  return (n / 1e6).toFixed(1) + "M";
}

function openMcp() {
  const body = $("#settings-body");
  body.textContent = "";
  const loading = document.createElement("div");
  loading.className = "mesh-muted";
  loading.textContent = "加载中…";
  body.appendChild(loading);
  Promise.all([
    api("/api/mcp", { credentials: "same-origin" }),
    api("/api/session/mcp", { credentials: "same-origin" }),
  ])
    .then(([srvRes, sessRes]) =>
      Promise.all([
        srvRes.status === 404 ? null : srvRes.json(),
        sessRes.ok ? sessRes.json() : null,
      ])
    )
    .then(([srv, sess]) => {
      if (state.settingsTab !== "mcp") return;
      renderMcpModal(body, srv, sess);
    })
    .catch((e) => {
      if (state.settingsTab !== "mcp") return;
      body.textContent = String(e.message || e);
    });
}

// onMcpAction is the single delegated click handler for every button rendered
// into the MCP panel (data-mcp-action + data-* parameters).
async function onMcpAction(e) {
  const btn = e.target.closest("[data-mcp-action]");
  if (!btn) return;
  const action = btn.dataset.mcpAction;
  btn.disabled = true;
  try {
    switch (action) {
      case "session-toggle": {
        const on = btn.dataset.value === "on";
        const r = await api("/api/session/mcp", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ enabled: on }),
        });
        if (r.status === 403) throw new Error((await r.json()).error || "远端任务会话不支持单独配置 MCP");
        if (r.status === 404) throw new Error("本进程没有可用的 MCP 工具组");
        if (!r.ok) throw new Error("设置失败");
        setStatusText(on ? "本会话 MCP 将在下次「重载」后启用" : "本会话 MCP 将在下次「重载」后停用");
        break;
      }
      case "session-apply":
        await applySessionMcp();
        break;
      case "server-reconnect": {
        const r = await api("/api/mcp/reconnect", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ server: btn.dataset.server || "" }),
        });
        if (!r.ok) throw new Error(((await r.json()) || {}).error || "重连失败");
        break;
      }
      case "server-enable":
      case "server-disable": {
        const r = await api(action === "server-enable" ? "/api/mcp/enable" : "/api/mcp/disable", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ server: btn.dataset.server || "" }),
        });
        if (!r.ok) throw new Error(((await r.json()) || {}).error || "操作失败");
        break;
      }
      case "server-direct-on":
      case "server-direct-off":
      case "server-autostart-on":
      case "server-autostart-off": {
        const kind = action.includes("direct") ? "direct" : "autostart";
        const on = action.endsWith("-on");
        const r = await api(`/api/mcp/servers/${encodeURIComponent(btn.dataset.server)}/${kind}`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ enabled: on }),
        });
        if (!r.ok) throw new Error(((await r.json()) || {}).error || "设置失败");
        setStatusText(kind === "direct"
          ? (on ? "已提升为一等工具并重发到各会话" : "已改回代理模式")
          : (on ? "已开启自启（立即连接）" : "已关闭自启"));
        break;
      }
      case "server-remove": {
        if (!(await desktopConfirm("从项目 .doge/mcp.json 删除服务器 " + btn.dataset.server + "？（全局/共享配置里的定义不受影响）"))) {
          btn.disabled = false;
          return;
        }
        const r = await api("/api/mcp/servers/" + encodeURIComponent(btn.dataset.server), { method: "DELETE" });
        if (!r.ok) throw new Error(((await r.json()) || {}).error || "删除失败");
        // 立即重建注册表：面板刷新后不再显示已删项（与导入后自动 reload 一致）
        const rl = await api("/api/mcp/reload", { method: "POST" });
        if (!rl.ok) {
          const e = ((await rl.json()) || {}).error;
          setStatusText("已删除 " + btn.dataset.server + "，但重新加载失败：" + (e || "未知错误"));
          btn.disabled = false;
          return;
        }
        setStatusText("已删除并重新加载：" + btn.dataset.server);
        break;
      }
      case "config-reload": {
        const r = await api("/api/mcp/reload", { method: "POST" });
        if (!r.ok) throw new Error(((await r.json()) || {}).error || "重新加载失败");
        setStatusText("已重新加载 MCP 配置");
        break;
      }
      case "config-import": {
        const ta = $("#mcp-import-json");
        // 逐级放宽的解析：严格 JSON → 清理富文本污染（BOM/零宽/智能引号）→ 允许尾逗号
        const strict = (ta.value || "").trim();
        const cleaned = strict
          .replace(/^[\uFEFF]/, "")
          .replace(/[\u200B-\u200D]/g, "")
          .replace(/[\u2018\u2019]/g, "'")
          .replace(/[\u201C\u201D]/g, '"');
        const lenient = cleaned.replace(/,\s*([}\]])/g, "$1");
        let parsed = null;
        let parseErr = null;
        for (const candidate of [strict, cleaned, lenient]) {
          try {
            parsed = JSON.parse(candidate);
            break;
          } catch (err) {
            parseErr = err;
          }
        }
        if (parsed === null) {
          const preview = cleaned.slice(0, 80).replace(/\n/g, " ");
          throw new Error("JSON 解析失败：" + (parseErr ? parseErr.message : "") + "；请检查引号/逗号。收到内容开头：" + preview);
        }
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed) || !Object.keys(parsed).length) {
          throw new Error("需要一个非空的 mcpServers 对象，如 {\"playwright\":{\"command\":\"npx\",\"args\":[]}}");
        }
        const r = await api("/api/mcp/servers", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(parsed),
        });
        if (!r.ok) throw new Error(((await r.json()) || {}).error || "导入失败");
        // 导入后立即重新加载配置并刷新面板（新 server 马上可见、可连）
        const rl = await api("/api/mcp/reload", { method: "POST" });
        if (!rl.ok) {
          const e = ((await rl.json()) || {}).error;
          setStatusText("已写入项目 .doge/mcp.json，但重新加载失败：" + (e || "未知错误"));
          btn.disabled = false;
          return;
        }
        setStatusText("已导入并重新加载 MCP 配置");
        break;
      }
      default:
        return;
    }
  } catch (err) {
    setStatusText("✗ " + String(err.message || err));
    btn.disabled = false;
    return;
  }
  openMcp(); // refresh the whole panel after any successful action
}

// applySessionMcp lands a pending per-session toggle via POST /api/reload
// (the same manual reload as models/auth/permissions).
async function applySessionMcp() {
  const r = await api("/api/reload", { method: "POST" });
  if (!r.ok) throw new Error("重载失败");
  const res = await r.json().catch(() => ({}));
  setStatusText(res.mcpApplied ? "已重载，本会话 MCP 状态已生效" : "已重载配置");
}

function renderMcpModal(body, srv, sess) {
  body.textContent = "";

  // 本会话：per-session enable/disable（remote 会话不可单独配置）。
  const sessSec = document.createElement("div");
  sessSec.className = "mesh-section";
  const sessTitle = document.createElement("div");
  sessTitle.className = "mesh-section-title";
  sessTitle.textContent = "本会话";
  sessSec.appendChild(sessTitle);

  if (!sess || !sess.available) {
    const p = document.createElement("div");
    p.className = "mesh-muted";
    p.textContent = "本进程没有注入 MCP 工具组（未配置 mcpServers 或 --no-mcp）。";
    sessSec.appendChild(p);
  } else if (sess.kind === "remote") {
    const p = document.createElement("div");
    p.className = "mesh-muted";
    p.textContent = "远端派发任务的会话不单独配置 MCP（由派发节点的策略决定）。";
    sessSec.appendChild(p);
  } else {
    const row = document.createElement("div");
    row.className = "mcp-session-row";
    const label = document.createElement("span");
    label.textContent = "MCP 工具（mcp 代理 + direct 提升工具）";
    row.appendChild(label);
    const badge = document.createElement("span");
    badge.className = "mesh-badge " + (sess.enabled ? "ok" : "no");
    badge.textContent = sess.enabled ? "启用" : "停用";
    row.appendChild(badge);
    if (sess.pending) {
      const pendingBadge = document.createElement("span");
      pendingBadge.className = "mesh-badge warn";
      pendingBadge.textContent = "待重载生效";
      row.appendChild(pendingBadge);
      const apply = document.createElement("button");
      apply.textContent = "应用（重载）";
      apply.title = "立即重载配置使本次启停生效";
      apply.dataset.mcpAction = "session-apply";
      row.appendChild(apply);
    }
    const toggle = document.createElement("button");
    toggle.textContent = sess.enabled ? "停用" : "启用";
    toggle.dataset.mcpAction = "session-toggle";
    toggle.dataset.value = sess.enabled ? "off" : "on";
    row.appendChild(toggle);
    sessSec.appendChild(row);
  }
  body.appendChild(sessSec);

  // 全局操作行。
  const ops = document.createElement("div");
  ops.className = "mcp-ops";
  const reloadBtn = document.createElement("button");
  reloadBtn.textContent = "↻ 重新加载 MCP 配置";
  reloadBtn.title = "重读 mcp.json/.mcp.json 并重建连接（配置修改后点这里生效）";
  reloadBtn.dataset.mcpAction = "config-reload";
  ops.appendChild(reloadBtn);
  body.appendChild(ops);

  // 服务器列表。
  const servers = (srv && srv.servers) || [];
  const sec = document.createElement("div");
  sec.className = "mesh-section";
  const sh = document.createElement("div");
  sh.className = "mesh-section-title";
  sh.textContent = servers.length ? "服务器 " + servers.length + " 个" : "未配置任何 MCP 服务器";
  sec.appendChild(sh);
  for (const s of servers) sec.appendChild(mcpServerCard(s));
  body.appendChild(sec);

  // 添加服务器表单（写入项目 .doge/mcp.json；重新加载后生效）。
  body.appendChild(mcpAddForm());
}

function mcpServerCard(s) {
  const card = document.createElement("div");
  card.className = "mesh-node";
  const head = document.createElement("div");
  head.className = "mesh-node-head";
  const name = document.createElement("span");
  name.className = "mesh-node-name";
  name.textContent = s.name;
  head.appendChild(name);
  const transport = document.createElement("span");
  transport.className = "mesh-muted";
  transport.textContent = s.transport === "http" ? "http" : "stdio";
  head.appendChild(transport);
  card.appendChild(head);

  const meta = document.createElement("div");
  meta.className = "mesh-node-meta";
  const st = document.createElement("span");
  if (!s.enabled) {
    st.className = "mesh-badge no";
    st.textContent = "已禁用";
  } else if (s.connected) {
    st.className = "mesh-badge ok";
    st.textContent = "已连接";
  } else {
    st.className = "mesh-badge";
    st.textContent = s.cache ? "未连接（缓存）" : "未连接";
  }
  meta.appendChild(st);
  const tools = s.tools || [];
  const count = document.createElement("span");
  count.className = "mesh-muted";
  count.textContent = "工具 " + tools.length;
  meta.appendChild(count);
  if (s.direct) {
    const d = document.createElement("span");
    d.className = "mesh-badge run";
    d.title = "工具提升为一等工具（server__tool 直接出现在模型工具列表）";
    d.textContent = "一等";
    meta.appendChild(d);
  }
  if (s.autoStart) {
    const a = document.createElement("span");
    a.className = "mesh-badge run";
    a.title = "发现/重载后自动启动并连接";
    a.textContent = "自启";
    meta.appendChild(a);
  }
  if (s.error) {
    const err = document.createElement("span");
    err.className = "mesh-muted";
    err.title = s.error;
    err.textContent = "错误: " + (s.error.length > 40 ? s.error.slice(0, 40) + "…" : s.error);
    meta.appendChild(err);
  }
  card.appendChild(meta);

  // 工具清单默认折叠，点击头部展开/收起。
  if (tools.length) {
    const list = document.createElement("div");
    list.className = "mcp-tools hidden";
    for (const t of tools) {
      const line = document.createElement("div");
      line.className = "mcp-tool-line";
      line.textContent = (s.name + "__" + t.name) + (t.description ? "  — " + t.description : "");
      list.appendChild(line);
    }
    head.style.cursor = "pointer";
    head.addEventListener("click", () => list.classList.toggle("hidden"));
    card.appendChild(list);
  }

  // 操作按钮。
  const actions = document.createElement("div");
  actions.className = "mcp-actions";
  const mkBtn = (text, act, title, extra) => {
    const b = document.createElement("button");
    b.textContent = text;
    b.dataset.mcpAction = act;
    b.dataset.server = s.name;
    if (title) b.title = title;
    Object.assign(b, extra || {});
    return b;
  };
  actions.appendChild(mkBtn("重连", "server-reconnect", "断开并重连该服务器，刷新工具缓存"));
  // 一等提升 / 自启切换：写项目 .doge/mcp.json，即时生效
  if (s.direct) {
    actions.appendChild(mkBtn("改回代理", "server-direct-off", "取消一等提升：工具改由 mcp 代理按需 search→call"));
  } else {
    actions.appendChild(mkBtn("设为一等", "server-direct-on", "把全部工具提升为一等工具（server__tool），注意会增加上下文占用"));
  }
  if (s.autoStart) {
    actions.appendChild(mkBtn("关闭自启", "server-autostart-off", "不再随发现/重载自动连接（改为懒连接）"));
  } else {
    actions.appendChild(mkBtn("自启", "server-autostart-on", "发现/重载后自动启动并连接"));
  }
  if (s.enabled) {
    actions.appendChild(mkBtn("禁用", "server-disable", "写 disabled 到项目 .doge/mcp.json（重新加载 MCP 配置后生效）"));
  } else {
    actions.appendChild(mkBtn("启用", "server-enable", "清除 disabled 标记（重新加载 MCP 配置后生效）"));
  }
  actions.appendChild(mkBtn("删除", "server-remove", "从项目 .doge/mcp.json 移除该定义"));
  card.appendChild(actions);
  return card;
}

function mcpAddForm() {
  const form = document.createElement("div");
  form.className = "mesh-section mcp-add";
  const title = document.createElement("div");
  title.className = "mesh-section-title";
  title.textContent = "导入配置（写入项目 .doge/mcp.json，重新加载后生效）";
  form.appendChild(title);

  const ta = document.createElement("textarea");
  ta.id = "mcp-import-json";
  ta.rows = 7;
  ta.spellcheck = false;
  // 预填社区标准格式示例，直接改名字/命令即可用
  ta.value = JSON.stringify(
    {
      "my-mcp-server": {
        type: "stdio",
        command: "npx",
        args: ["-y", "some-mcp-server"],
      },
    },
    null,
    2
  );
  form.appendChild(ta);

  const importBtn = document.createElement("button");
  importBtn.className = "primary";
  importBtn.textContent = "导入";
  importBtn.title = "同名 server 做字段合并；支持 stdio（type+command+args）与 http（type+url）";
  importBtn.dataset.mcpAction = "config-import";
  form.appendChild(importBtn);
  return form;
}

/* ---------------- composer 浮层：模型选择 / 思考级别 ---------------- */
// 浮层为 body 直挂的绝对定位菜单，点击别处即关（document click 统一收口）
function closePopovers() {
  document.querySelectorAll(".menu-pop").forEach((el) => el.remove());
}

// anchorPop 在锚点元素上方弹出菜单（composer 贴底，向上展开）。
// 内容填充后必须再调 positionPop，否则按空容器高度定位会被视口底部裁掉。
function anchorPop(anchor, className) {
  closePopovers();
  const pop = document.createElement("div");
  pop.className = "menu-pop" + (className ? " " + className : "");
  document.body.appendChild(pop);
  positionPop(pop, anchor);
  return pop;
}

// positionPop 依据浮层实际尺寸重新定位：优先锚点上方，放不下落下方
function positionPop(pop, anchor) {
  const r = anchor.getBoundingClientRect();
  const h = pop.offsetHeight, w = pop.offsetWidth;
  let top = r.top - h - 8;
  if (top < 8) top = Math.min(r.bottom + 8, window.innerHeight - h - 8);
  pop.style.top = Math.max(8, top) + "px";
  pop.style.left = Math.min(Math.max(8, r.right - w), window.innerWidth - w - 8) + "px";
}

// thinkingLabel 把后端级别映射为中文档位（minimal/low→低，medium→中…）
function thinkingLabel(level) {
  return { minimal: "低", low: "低", medium: "中", high: "高", xhigh: "最高", max: "最高" }[level] || level || "最高";
}

function updateThinkingBtn() {
  $("#thinking-btn").textContent = "⊕ " + thinkingLabel(state.thinking) + " ▾";
}

function updateModelBtn() {
  const spec = state.modelSpec || "";
  $("#model-btn").textContent = "◇ " + (spec ? spec.split("/").pop() : "模型") + " ▾";
}

function openThinkingMenu(anchor) {
  // max 与 xhigh 同为"最高"档：settings 里默认级别可能是 max
  const curRaw = state.thinking || "xhigh";
  const cur = curRaw === "max" ? "xhigh" : curRaw;
  const pop = anchorPop(anchor);
  for (const [lv, label] of [["low", "低"], ["medium", "中"], ["high", "高"], ["xhigh", "最高"]]) {
    const item = document.createElement("button");
    item.className = "menu-item" + (lv === cur ? " current" : "");
    const name = document.createElement("span");
    name.textContent = label;
    item.appendChild(name);
    if (lv === cur) {
      const check = document.createElement("span");
      check.className = "check";
      check.textContent = "✓";
      item.appendChild(check);
    }
    item.addEventListener("click", (e) => {
      e.stopPropagation();
      closePopovers();
      setThinkingLevel(lv);
    });
    pop.appendChild(item);
  }
  positionPop(pop, anchor);
}

function setThinkingLevel(level) {
  api("/api/thinking", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ level }),
  })
    .then((r) => {
      if (!r.ok) throw new Error("set thinking level failed");
      state.thinking = level;
      updateThinkingBtn();
      setStatusText("思考级别：" + thinkingLabel(level));
    })
    .catch(() => setStatusText("思考级别设置失败"));
}

function openModelMenu(anchor) {
  const pop = anchorPop(anchor, "model-menu");
  // 按 provider 分组，组头小字；自定义模型与内置模型自然分区
  const groups = new Map();
  for (const m of state.models) {
    if (!groups.has(m.provider)) groups.set(m.provider, []);
    groups.get(m.provider).push(m);
  }
  if (!groups.size) {
    const empty = document.createElement("div");
    empty.className = "menu-note";
    empty.textContent = "未加载模型列表";
    pop.appendChild(empty);
  }
  for (const [provider, models] of groups) {
    const head = document.createElement("div");
    head.className = "menu-head";
    head.textContent = provider;
    pop.appendChild(head);
    for (const m of models) {
      const item = document.createElement("button");
      item.className = "menu-item" + (m.spec === state.modelSpec ? " current" : "");
      const name = document.createElement("span");
      name.textContent = m.id;
      item.appendChild(name);
      const badges = document.createElement("span");
      badges.className = "menu-badges";
      if ((m.input || []).includes("image")) badges.appendChild(menuBadge("视觉"));
      if (m.reasoning) badges.appendChild(menuBadge("推理"));
      if (m.contextWindow >= 1e6) badges.appendChild(menuBadge("1M"));
      if (badges.childNodes.length) item.appendChild(badges);
      if (m.spec === state.modelSpec) {
        const check = document.createElement("span");
        check.className = "check";
        check.textContent = "✓";
        item.appendChild(check);
      }
      item.addEventListener("click", (e) => {
        e.stopPropagation();
        closePopovers();
        switchModel(m.spec);
      });
      pop.appendChild(item);
    }
  }
  positionPop(pop, anchor);
}

function menuBadge(text) {
  const b = document.createElement("span");
  b.className = "menu-badge";
  b.textContent = text;
  return b;
}

function switchModel(spec) {
  api("/api/model", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ spec }),
  })
    .then((r) => {
      if (!r.ok) throw new Error("switch model failed");
      state.modelSpec = spec;
      updateModelBtn();
      setStatusText("模型：" + spec);
    })
    .catch(() => setStatusText("模型切换失败"));
}

/* ---------------- ＋菜单（附件/技能）与项目目录 picker ---------------- */
function openAddMenu(anchor) {
  const pop = anchorPop(anchor);
  const attach = document.createElement("button");
  attach.className = "menu-item";
  attach.textContent = "📎 附件";
  attach.title = "选择文件或图片（也可直接拖拽/粘贴到输入框）";
  attach.addEventListener("click", () => {
    closePopovers();
    $("#file-input").click();
  });
  pop.appendChild(attach);

  positionPop(pop, anchor);
  const head = document.createElement("div");
  head.className = "menu-head";
  head.textContent = "技能";
  pop.appendChild(head);
  const box = document.createElement("div");
  box.className = "menu-note";
  box.textContent = "加载中…";
  pop.appendChild(box);
  api("/api/skills", { credentials: "same-origin" })
    .then((r) => r.json())
    .then((j) => {
      box.remove();
      const skills = (j.skills || []).filter((s) => !s.disabled);
      if (!skills.length) {
        const none = document.createElement("div");
        none.className = "menu-note";
        none.textContent = "暂无可用技能";
        pop.appendChild(none);
        return;
      }
      for (const sk of skills) {
        const item = document.createElement("button");
        item.className = "menu-item";
        item.title = sk.description || sk.name;
        const name = document.createElement("span");
        name.className = "t-name";
        name.textContent = "✦ " + sk.name;
        item.appendChild(name);
        const desc = document.createElement("span");
        desc.className = "t-arg";
        desc.textContent = sk.description || "";
        item.appendChild(desc);
        item.addEventListener("click", () => {
          closePopovers();
          const input = $("#input");
          input.value = (input.value ? input.value + " " : "") + "/skill:" + sk.name + " ";
          input.focus();
        });
        pop.appendChild(item);
      }
    })
    .then(() => positionPop(pop, anchor))
    .catch(() => {
      box.textContent = "技能加载失败";
    });
}

function newSessionIn(cwd) {
  api("/api/session/new", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ cwd }),
  })
    .then((r) => {
      if (!r.ok) throw new Error((r.status === 409 ? "运行中，请先中断" : "新建会话失败"));
      setStatusText("已在新目录打开会话：" + cwd);
    })
    .catch((e) => setStatusText(String(e.message || e)));
}

/* ---------------- 新建会话窗口 ---------------- */
function greeting() {
  const h = new Date().getHours();
  if (h < 6) return "夜深了，还在奋斗呀";
  if (h < 12) return "早上好呀";
  if (h < 14) return "中午好呀";
  if (h < 18) return "下午好呀";
  return "晚上好呀，今天辛苦啦";
}

function projectLabel(cwd) {
  return cwd ? cwd.split("/").filter(Boolean).pop() : "全局目录";
}

function openNewTask() {
  state.ntCwd = state.cwd || state.globalCwd || "";
  $("#newtask-greet").textContent = greeting();
  $("#nt-project-name").textContent = projectLabel(state.ntCwd);
  $("#nt-input").value = "";
  $("#newtask").classList.remove("hidden");
  bindNewTask();
  $("#nt-input").focus();
}

let ntBound = false;
function bindNewTask() {
  if (ntBound) return;
  ntBound = true;
  $("#nt-project").addEventListener("click", (e) => {
    e.stopPropagation();
    openProjectMenu(e.currentTarget);
  });
  $("#nt-model").addEventListener("click", (e) => {
    e.stopPropagation();
    openModelMenu(e.currentTarget);
  });
  $("#nt-thinking").addEventListener("click", (e) => {
    e.stopPropagation();
    openThinkingMenu(e.currentTarget);
  });
  $("#nt-send").addEventListener("click", submitNewTask);
  $("#nt-input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      submitNewTask();
    }
    if (e.key === "Escape") $("#newtask").classList.add("hidden");
  });
  $("#newtask").addEventListener("click", (e) => {
    if (e.target === $("#newtask")) $("#newtask").classList.add("hidden");
  });
}

// openProjectMenu 新建窗口里的项目下拉：当前/全局/最近项目
function openProjectMenu(anchor) {
  const pop = anchorPop(anchor);
  const set = (cwd) => {
    state.ntCwd = cwd;
    $("#nt-project-name").textContent = projectLabel(cwd);
    closePopovers();
  };
  const opts = [];
  if (state.cwd) opts.push({ cwd: state.cwd, label: "当前项目 · " + projectLabel(state.cwd) });
  if (state.globalCwd && state.globalCwd !== state.cwd) {
    opts.push({ cwd: state.globalCwd, label: "全局目录 · " + projectLabel(state.globalCwd) });
  }
  const seen = new Set(opts.map((o) => o.cwd));
  for (const o of opts) {
    const item = document.createElement("button");
    item.className = "menu-item" + (o.cwd === state.ntCwd ? " current" : "");
    const name = document.createElement("span");
    name.className = "t-name";
    name.textContent = "📁 " + o.label;
    item.appendChild(name);
    const arg = document.createElement("span");
    arg.className = "t-arg";
    arg.textContent = o.cwd;
    item.appendChild(arg);
    item.addEventListener("click", (e) => {
      e.stopPropagation();
      set(o.cwd);
    });
    pop.appendChild(item);
  }
  // 目录浏览器：由 /api/fs/ls 提供数据，逐级进入后"选择此目录"
  const browseHead = document.createElement("div");
  browseHead.className = "menu-head";
  browseHead.textContent = "浏览目录";
  pop.appendChild(browseHead);
  const browse = document.createElement("div");
  browse.className = "menu-browse";
  browse.textContent = "加载中…";
  pop.appendChild(browse);
  const loadBrowse = (p) => {
    browse.textContent = "加载中…";
    api("/api/fs/ls?path=" + encodeURIComponent(p || ""), { credentials: "same-origin" })
      .then((r) => r.json())
      .then((j) => {
        browse.textContent = "";
        const cur = document.createElement("button");
        cur.className = "menu-item browse-pick";
        const cn = document.createElement("span");
        cn.className = "t-name";
        cn.textContent = "✓ 选择此目录";
        cur.appendChild(cn);
        const cp = document.createElement("span");
        cp.className = "t-arg";
        cp.textContent = j.path;
        cur.appendChild(cp);
        cur.addEventListener("click", (e) => {
          e.stopPropagation();
          set(j.path);
        });
        browse.appendChild(cur);
        if (j.parent) {
          const up = document.createElement("button");
          up.className = "menu-item";
          const un = document.createElement("span");
          un.className = "t-name";
          un.textContent = "⌃ 上一级";
          up.appendChild(un);
          up.addEventListener("click", (e) => {
            e.stopPropagation();
            loadBrowse(j.parent);
          });
          browse.appendChild(up);
        }
        for (const d of j.dirs || []) {
          const item = document.createElement("button");
          item.className = "menu-item";
          const nm = document.createElement("span");
          nm.className = "t-name";
          nm.textContent = d.name;
          item.appendChild(nm);
          item.addEventListener("click", (e) => {
            e.stopPropagation();
            loadBrowse(d.path);
          });
          browse.appendChild(item);
        }
        browse.scrollTop = 0;
      })
      .catch(() => {
        browse.textContent = "目录加载失败";
      });
  };
  loadBrowse(state.globalCwd || state.cwd || "");

  api("/api/sessions", { credentials: "same-origin" })
    .then((r) => r.json())
    .then((j) => {
      for (const s of j.sessions || []) {
        const cwd = s.cwd || "";
        if (!cwd || seen.has(cwd)) continue;
        seen.add(cwd);
        const item = document.createElement("button");
        item.className = "menu-item";
        const name = document.createElement("span");
        name.className = "t-name";
        name.textContent = "📁 " + projectLabel(cwd);
        item.appendChild(name);
        const arg = document.createElement("span");
        arg.className = "t-arg";
        arg.textContent = cwd;
        item.appendChild(arg);
        item.addEventListener("click", (e) => {
          e.stopPropagation();
          set(cwd);
        });
        pop.appendChild(item);
      }
      positionPop(pop, anchor);
    })
    .catch(() => {});
  positionPop(pop, anchor);
}

// newtask 关闭

function submitNewTask() {
  const text = $("#nt-input").value.trim();
  if (!text) return;
  const cwd = state.ntCwd || state.globalCwd || "";
  $("#nt-send").disabled = true;
  api("/api/session/new", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ cwd }),
  })
    .then((r) => {
      if (!r.ok) throw new Error("新建会话失败");
      return api("/api/prompt", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text }),
      });
    })
    .then((r) => {
      if (!r.ok) throw new Error("发送失败");
      $("#newtask").classList.add("hidden");
      $("#nt-send").disabled = false;
    })
    .catch((e) => {
      setStatusText(String(e.message || e));
      $("#nt-send").disabled = false;
    });
}

/* ---------------- REST loads ---------------- */
function loadModels() {
  api("/api/models", { credentials: "same-origin" })
    .then((r) => r.json())
    .then((j) => {
      state.models = j.models || [];
      updateModelBtn();
    })
    .catch(() => {});
}

function loadSession() {
  // Loading affordance while the transcript is fetched and rendered: big
  // sessions (remote-task forks especially) take a moment to transfer.
  $("#messages").classList.add("pane-loading");
  setStatusText("正在加载会话…");
  return api("/api/session", { credentials: "same-origin" })
    .then((r) => r.json())
    .then((info) => {
      state.messages = info.messages || [];
      state.msgOffset = info.earlierCount || 0;
      state.sessionKind = info.kind || "";
      state.sessionSourceName = info.sourceName || "";
      state.sessionSourcePeer = info.sourcePeerId || "";
      renderAll();
    })
    .catch((e) => setStatusText("加载会话失败：" + String((e && e.message) || e)))
    .finally(() => {
      $("#messages").classList.remove("pane-loading");
      setStatusText("");
    });
}

function fetchStatus() {
  api("/api/status", { credentials: "same-origin" })
    .then((r) => {
      if (r.status === 401) location.reload(); // 凭据失效：回登录页
      return r;
    })
    .then((r) => r.json())
    .then((st) => {
      state.sessionId = st.sessionId || "";
      state.cwd = st.cwd || "";
      state.globalCwd = st.globalCwd || "";
      if (st.model) {
        state.modelSpec = st.model;
        updateModelBtn();
      }
      if (st.thinking) {
        state.thinking = st.thinking;
        updateThinkingBtn();
      }
      setBusy(st.streaming);
      setReadOnly(st.writable === false);
      if (st.writable === false) {
        setStatusText(st.owner ? "会话正被「" + st.owner + "」使用（只读）" : "会话正被其他窗口使用（只读）");
      } else {
        const want = st.streaming ? "运行中…" : (st.errorMessage ? "错误: " + st.errorMessage : "就绪");
        // 常态「就绪」在操作反馈保留期内不覆盖；错误/运行中是状态变化，立即显示
        if (want === "就绪" && Date.now() < sbHoldUntil) return;
        $("#sb-status-text").textContent = want;
        sbHoldUntil = 0;
      }
    })
    .catch(() => {});
}

/* ---------------- websocket ---------------- */
function connectWS() {
  // 桌面端：无 WebSocket——事件经 wails Events 注入，"连接"即拉快照续接视图
  const sock = {
    readyState: 1,
    onopen: null,
    onmessage: null,
    onclose: null,
    send(data) {
      try {
        const msg = JSON.parse(data);
        if (msg.type === "permission_response" && msg.id) {
          const r = msg.response || {};
          Call.ByName(
            "doge/desktop/services.SessionService.PermissionAnswer",
            msg.id, !!r.allowOnce, !!r.allowAlways, !!r.deny
          ).catch(() => {});
        }
      } catch (_) { /* 忽略非法帧 */ }
    },
  };
  state.ws = sock;
  // 处理函数与 webui 的真 WebSocket 完全一致（此前漏挂导致事件帧无人处理）
  sock.onopen = () => {
    state.connected = true;
    state.retry = 0;
    $("#conn-dot").className = "status-dot on";
  };
  sock.onmessage = (e) => {
    let msg;
    try { msg = JSON.parse(e.data); } catch (_) { return; }
    handleEvent(msg);
  };
  sock.onclose = () => {
    state.connected = false;
    $("#conn-dot").className = "status-dot off";
  };
  (async () => {
    if (sock.onopen) sock.onopen();
    try {
      const snap = await Call.ByName("doge/desktop/services.SessionService.Snapshot");
      if (sock.onmessage) sock.onmessage({ data: JSON.stringify(snap) });
    } catch (_) { /* 快照失败：事件流随后补齐 */ }
  })();
  Events.On("console", (ev) => {
    try {
      if (sock.onmessage) sock.onmessage({ data: JSON.stringify(ev.data) });
    } catch (e) {
      try { Events.Emit("js:err", "handleFrame: " + String((e && e.stack) || e)); } catch (_) {}
    }
  });
}

/* ---------------- event handling ---------------- */
let streamTimer = null;
function scheduleStreamRender() {
  if (streamTimer) return;
  streamTimer = requestAnimationFrame(() => {
    streamTimer = null;
    renderStreaming();
  });
}

function handleEvent(m) {
  switch (m.type) {
    case "session_info": {
      state.messages = m.messages || [];
      state.msgOffset = m.earlierCount || 0;
      state.streaming = m.streaming ? (m.streamingMessage || { role: "assistant", content: [] }) : null;
      if (m.sessionId) state.sessionId = m.sessionId;
      setBusy(!!m.streaming);
      renderAll();
      if (m.model) {
        state.modelSpec = m.model;
        updateModelBtn();
      }
      break;
    }
    case "agent_start":
    case "turn_start":
      if (m.type === "agent_start") clearToolChips();
      setBusy(true);
      setStatusText("运行中…");
      break;
    case "message_start":
      state.streaming = { role: m.message.role, content: m.message.content || [] };
      renderStreaming();
      break;
    case "message_update":
      state.streaming = m.message || state.streaming;
      scheduleStreamRender();
      break;
    case "message_end": {
      const msg = m.message;
      if (msg) {
        if (msg.role === "assistant" || msg.role === "user" || msg.role === "toolResult") {
          state.messages.push(msg);
          appendMessageDOM(msg);
        }
        // The committed tool result replaces its live execution card in-flow.
        if (msg.role === "toolResult" && msg.toolCallId) {
          removeChip(msg.toolCallId);
        }
      }
      clearStreaming();
      break;
    }
    case "tool_execution_start":
      addToolChip(m.toolCallId, m.toolName, m.args || {}, "running");
      break;
    case "tool_execution_update":
      updateToolChip(m.toolCallId, m.partialResult, "running");
      break;
    case "tool_execution_end":
      updateToolChip(m.toolCallId, m.result, m.isError ? "error" : "done");
      break;
    case "turn_end":
      break;
    case "agent_end":
      // The committed transcript now carries the tool calls and results;
      // the transient live chips are no longer needed.
      clearToolChips();
      setBusy(false);
      setStatusText("完成");
      fetchStatus();
      // A lazily created session file materializes with the first message;
      // refresh the sidebar so it shows up.
      populateSessions();
      break;
    case "permission_ask":
      showPermAsk(m);
      break;
    case "question":
      showQuestion(m);
      break;
    case "mesh_progress":
      if (m.meshProgress && m.meshProgress.message) {
        setStatusText("远端任务 " + (m.meshProgress.taskId || "") + ": " + m.meshProgress.message);
      }
      break;
    case "session_reset":
      // A session /new or switch happened (here or in another tab); reload.
      resetWebConversation();
      break;
    case "session_list_changed":
      // A session was deleted somewhere; refresh the sidebar.
      populateSessions();
      break;
    case "session_control":
      // Ownership changed (someone released or force-took the session);
      // re-fetch status so this window's read-only state stays current.
      fetchStatus();
      break;
    default:
      break;
  }
}

function clearStreaming() {
  if (state.streamingEl) {
    state.streamingEl.remove();
    state.streamingEl = null;
  }
  state.streaming = null;
}

function setBusy(busy) {
  state.busy = busy;
  $("#input").disabled = busy;
  $("#send-btn").disabled = busy;
  $("#attach-btn").disabled = busy;
  $("#abort-btn").classList.toggle("hidden", !busy);
}

let sbHoldUntil = 0; // 操作反馈保留截止（见 setStatusText）
function setStatusText(t) {
  $("#sb-status-text").textContent = t;
  // 操作反馈保留 4s：5s 轮询的「就绪」不覆盖刚设置的提示（fetchStatus 里只豁免常态文案）
  sbHoldUntil = Date.now() + 4000;
}

/* ---------------- rendering ---------------- */

// The live region (#live) holds the streaming message and the live tool
// activity strip, and is always kept AFTER all committed messages. Without
// it, committed messages appended to #messages end up below the tool strip,
// making in-progress content render mid-conversation and then "jump" to the
// bottom once committed.
function ensureLive() {
  const host = $("#messages");
  let live = $("#live");
  if (!live) {
    live = document.createElement("div");
    live.id = "live";
    host.appendChild(live);
  }
  return { live };
}

// buildTurns 把平铺消息切成回合：一条 user 消息开启一个回合，其后所有
// 非用户消息（思考/工具/中间输出/用量）都归属该回合，直到下一条 user。
function buildTurns(messages) {
  const turns = [];
  let cur = null;
  for (const m of messages) {
    if (m.role === "user" || !cur) {
      cur = { user: m.role === "user" ? m : null, rest: m.role === "user" ? [] : [m] };
      turns.push(cur);
    } else {
      cur.rest.push(m);
    }
  }
  return turns;
}

// turnDuration 回合耗时文案：最后一条消息与用户消息的时间差
function turnDuration(turn) {
  const start = turn.user ? turn.user.timestamp || 0 : 0;
  let end = start;
  for (const m of turn.rest) {
    if (m.timestamp && m.timestamp > end) end = m.timestamp;
  }
  const sec = Math.max(0, Math.round((end - start) / 1000));
  if (sec < 60) return sec + " 秒";
  return Math.floor(sec / 60) + " 分 " + (sec % 60) + " 秒";
}

// 回合的最后一条带正文的助手消息：折叠时唯一留在折叠区外的"最终输出"
function finalAssistantIdx(rest) {
  for (let i = rest.length - 1; i >= 0; i--) {
    const m = rest[i];
    if (m.role !== "assistant") continue;
    const blocks = normalizeContent(m.content);
    if (blocks.some((b) => b.type === "text" && (b.text || "").trim())) return i;
  }
  return -1;
}

// renderTurn 渲染一个回合：完成的回合把中间过程折叠进"已工作 N"行
// （token 用量只在展开后可见），最终输出留在外面；流式中的回合全程展开。
// 注意最后一条助手消息常是"思考+工具+正文"混合体：按块拆分，正文留在外面，
// 过程块（含该条消息的用量）进折叠。
function renderTurn(turn, live) {
  const frag = document.createDocumentFragment();
  if (turn.user) {
    const un = renderMessage(turn.user, state.messages);
    frag.appendChild(un);
    state.msgEls.set(turn.user, un);
  }
  const fi = finalAssistantIdx(turn.rest);
  const intermediates = [];
  let final = null;
  turn.rest.forEach((m, i) => {
    if (i === fi && m.role === "assistant") {
      const blocks = normalizeContent(m.content);
      const textBlocks = blocks.filter((b) => b.type === "text" && (b.text || "").trim());
      const procBlocks = blocks.filter((b) => !(b.type === "text" && (b.text || "").trim()));
      if (procBlocks.length) intermediates.push({ ...m, content: procBlocks });
      final = { ...m, content: textBlocks };
    } else {
      intermediates.push(m);
    }
  });
  const collapsed = !!final && !live;

  const renderOne = (m) => {
    if (m.role === "toolResult") return null;
    const node = renderMessage(m, state.messages);
    state.msgEls.set(m, node);
    return node;
  };

  if (collapsed) {
    const fold = document.createElement("details");
    fold.className = "fold turn-fold";
    const sum = document.createElement("summary");
    const name = document.createElement("span");
    name.className = "t-name";
    name.textContent = "已工作 " + turnDuration(turn);
    sum.appendChild(name);
    fold.appendChild(sum);
    const box = document.createElement("div");
    box.className = "turn-box";
    for (const m of intermediates) {
      const node = renderOne(m);
      if (node) box.appendChild(node);
    }
    fold.appendChild(box);
    frag.appendChild(fold);
  } else {
    for (const m of intermediates) {
      const node = renderOne(m);
      if (node) frag.appendChild(node);
    }
  }
  if (final) {
    const node = renderMessage(final, state.messages, true);
    state.msgEls.set(final, node);
    frag.appendChild(node);
  }
  return frag;
}

function renderAll() {
  const host = $("#messages");
  // Preserve the reading position across a rebuild: stick to the bottom if
  // the user was there, otherwise keep the same distance from the bottom.
  const stick = nearBottom(host);
  const dist = host.scrollHeight - host.scrollTop - host.clientHeight;
  host.textContent = "";
  state.msgEls = new Map();
  if (state.msgOffset > 0) {
    const earlierBtn = document.createElement("button");
    earlierBtn.className = "earlier-btn";
    earlierBtn.textContent = "↑ 加载更早（" + state.msgOffset + " 条）";
    earlierBtn.addEventListener("click", loadEarlier);
    host.appendChild(earlierBtn);
  }
  const turns = buildTurns(state.messages);
  turns.forEach((turn, i) => {
    const isLast = i === turns.length - 1;
    host.appendChild(renderTurn(turn, isLast && state.busy));
  });
  ensureLive();
  if (state.streaming) renderStreaming();
  host.scrollTop = stick
    ? host.scrollHeight
    : Math.max(0, host.scrollHeight - dist - host.clientHeight);
}

function appendMessageDOM(msg) {
  // 回合折叠结构下增量定位太脆弱（新消息可能落进折叠区/改变最终输出），
  // 统一走全量重渲；renderAll 自带贴底/保位逻辑。
  renderAll();
}

// resultForToolCall finds the committed tool result for a tool call id.
function resultForToolCall(toolCallId, allMsgs) {
  if (!toolCallId || !allMsgs) return null;
  for (const m of allMsgs) {
    if (m.role === "toolResult" && m.toolCallId === toolCallId) return m;
  }
  return null;
}

// rerenderToolParent rebuilds the assistant card that contains a given tool
// call so its fold gains the just-arrived result.
function rerenderToolParent(toolResultMsg) {
  if (!toolResultMsg || !toolResultMsg.toolCallId) return;
  const parent = state.messages.find((m) =>
    m.role === "assistant" &&
    normalizeContent(m.content).some((b) => b.type === "toolCall" && b.id === toolResultMsg.toolCallId));
  if (!parent) return;
  const node = renderMessage(parent, state.messages);
  const old = state.msgEls.get(parent);
  if (old && old.parentNode) old.replaceWith(node);
  state.msgEls.set(parent, node);
}

// userRoleLabel returns the label shown on user message cards: for a
// remote-task session it is the dispatching node ("node-a (12D3Ko)") instead
// of "user".
function userRoleLabel() {
  if (state.sessionKind === "remote" && state.sessionSourceName) {
    let label = state.sessionSourceName;
    if (state.sessionSourcePeer) {
      label += " (" + state.sessionSourcePeer.slice(0, 6) + ")";
    }
    return label;
  }
  return "user";
}

/* Render committed messages from the server's ai.Message JSON shape. */
// remoteTaskMeta recognizes a remote-task result message: the structured
// marker (details.kind) first, then the legacy "[远端任务 …]" text prefix so
// sessions persisted before the marker existed still render as task cards.
function remoteTaskMeta(msg) {
  const d = msg.details;
  if (d && typeof d === "object" && d.kind === "remote-task") return d;
  const text = plainText(normalizeContent(msg.content || []));
  const m = /^\[远端任务 ([^\]]+) 完成\] peer=(\S+) status=(\S+)(?: session=(\S+))?/.exec(text);
  if (m) return { taskId: m[1], peer: m[2], status: m[3], session: m[4] || "" };
  return null;
}

// stripRemoteTaskHeader removes the "[远端任务 …]" first line from the body
// (the card header already shows task/peer/status).
function stripRemoteTaskHeader(text) {
  return text.replace(/^\[远端任务[^\n]*\]\n?/, "");
}

// remoteTaskStatusBadge maps a task status onto the mesh badge classes.
function remoteTaskBadgeClass(status) {
  if (status === "completed") return "ok";
  if (status === "running") return "run";
  return "no";
}

const remoteTaskStatusLabels = {
  completed: "完成", error: "错误", aborted: "已中断", timeout: "超时", recovered: "已恢复", running: "运行中",
};

function renderMessage(msg, allMsgs, noUsage) {
  if (msg.role === "toolResult") return null; // handled by the paired fold
  const wrap = document.createElement("div");
  wrap.className = "msg " + msg.role;

  // 远端任务结果：独立折叠卡片（任务号/节点/状态 + 输出），默认收起省空间，
  // 点击头部展开完整输出。
  const rt = msg.role === "user" ? remoteTaskMeta(msg) : null;
  if (rt) {
    const wrap = document.createElement("div");
    wrap.className = "msg remote-task";
    const card = document.createElement("details");
    card.className = "remote-task-card";
    const sum = document.createElement("summary");
    sum.className = "remote-task-head";
    const role = document.createElement("span");
    role.className = "role";
    role.textContent = "远端任务";
    sum.appendChild(role);
    const badge = document.createElement("span");
    badge.className = "mesh-badge " + remoteTaskBadgeClass(rt.status);
    badge.textContent = remoteTaskStatusLabels[rt.status] || rt.status || "完成";
    sum.appendChild(badge);
    const peer = document.createElement("span");
    peer.className = "remote-task-meta";
    peer.textContent = (sessionPeerName(rt.peer) || String(rt.peer || "").slice(0, 12)) + (rt.taskId ? " · " + String(rt.taskId).slice(0, 8) : "");
    peer.title = "peer=" + (rt.peer || "") + (rt.taskId ? " task=" + rt.taskId : "") + (rt.session ? " session=" + rt.session : "");
    sum.appendChild(peer);
    card.appendChild(sum);

    const out = stripRemoteTaskHeader(plainText(normalizeContent(msg.content || [])));
    if (out) {
      const pre = document.createElement("pre");
      pre.className = "remote-task-out" + (rt.status === "error" || rt.status === "timeout" ? " error" : "");
      pre.textContent = out;
      card.appendChild(pre);
    }
    wrap.appendChild(card);
    return wrap;
  }

  const role = document.createElement("div");
  role.className = "role";
  role.textContent = msg.role === "user" ? userRoleLabel() : msg.role;
  // 远端任务会话的用户消息（即派发进来的任务）显示来源节点，其余消息角色标签隐藏
  if (msg.role === "user" && state.sessionKind === "remote") {
    wrap.classList.add("remote-in");
  }
  wrap.appendChild(role);

  const body = document.createElement("div");
  body.className = "body";

  const blocks = normalizeContent(msg.content);
  if (msg.role === "user") {
    // 用户消息：文本气泡 + 附件（图片直显、引用行渲染为可下载文件卡片）
    const texts = [];
    for (const b of blocks) {
      if (b.type === "image" && b.data) {
        const img = document.createElement("img");
        img.src = "data:" + (b.mimeType || "image/png") + ";base64," + b.data;
        img.alt = "image";
        body.appendChild(img);
      } else if (b.type === "text" && /^\[附件 /.test(b.text || "")) {
        const card = attachmentFileCard(b.text);
        if (card) body.appendChild(card);
        else texts.push(b.text || "");
      } else if (b.type === "text") {
        texts.push(b.text || "");
      }
    }
    const text = texts.join("\n");
    if (text) {
      const p = document.createElement("div");
      p.className = "md";
      p.innerHTML = userTextHtml(text);
      body.appendChild(p);
    }
    // Edit affordance: rewrite this turn and re-run from here.
    const editBtn = document.createElement("button");
    editBtn.className = "msg-edit";
    editBtn.textContent = "✎";
    editBtn.title = "编辑此消息并重新执行";
    editBtn.addEventListener("click", () => startEditUserMessage(wrap, msg));
    wrap.appendChild(editBtn);
  } else {
    // Assistant: markdown text + collapsible thinking + tool calls. Each
    // tool call is a single card whose fold also carries its committed
    // result, so the call and the result stay paired.
    for (const b of blocks) {
      if (b.type === "text") {
        const div = document.createElement("div");
        div.className = "md";
        div.innerHTML = mdToHtml(b.text || "");
        body.appendChild(div);
      } else if (b.type === "thinking") {
        body.appendChild(makeFold(thinkingTitle(b.thinking), b.thinking || "(redacted)", false, true));
      } else if (b.type === "toolCall") {
        body.appendChild(makeToolCard(b, resultForToolCall(b.id, allMsgs)));
      } else if (b.type === "image" && b.data) {
        const img = document.createElement("img");
        img.src = "data:" + (b.mimeType || "image/png") + ";base64," + b.data;
        img.alt = "image";
        body.appendChild(img);
      }
    }
    if (msg.errorMessage) {
      const err = document.createElement("div");
      err.className = "error";
      err.textContent = "错误: " + msg.errorMessage;
      body.appendChild(err);
    }
    if (!noUsage && msg.usage && (msg.usage.input || msg.usage.output)) {
      body.appendChild(makeUsageFold(msg.usage));
    }
  }
  wrap.appendChild(body);
  return wrap;
}

// thinkingTitle 思考折叠行标题：带字数（对齐 ZCode 的"思考 · 持续了N秒"形态）
function thinkingTitle(text) {
  const n = (text || "").length;
  return "思考 · " + (n >= 1000 ? (n / 1000).toFixed(1) + "k 字" : n + " 字");
}

// makeUsageFold token 用量折叠行：摘要行显示进出 tokens，展开看缓存明细
function makeUsageFold(u) {
  const d = document.createElement("details");
  d.className = "fold usage";
  const s = document.createElement("summary");
  const name = document.createElement("span");
  name.className = "t-name";
  name.textContent = "用量";
  s.appendChild(name);
  const arg = document.createElement("span");
  arg.className = "t-arg";
  arg.textContent = "in " + fmtNum(u.input ?? 0) + " / out " + fmtNum(u.output ?? 0) + " tokens";
  s.appendChild(arg);
  d.appendChild(s);
  const c = document.createElement("div");
  c.className = "fold-content";
  c.textContent =
    "input " + (u.input ?? 0) + " · output " + (u.output ?? 0) + " · total " +
    (u.totalTokens ?? ((u.input ?? 0) + (u.output ?? 0))) +
    "\ncache read " + (u.cacheRead ?? 0) + " · cache write " + (u.cacheWrite ?? 0);
  d.appendChild(c);
  return d;
}

function makeFold(title, text, open, md) {
  // Native <details> toggling only; no CSS class overrides the open state.
  const d = document.createElement("details");
  d.className = "fold";
  if (open) d.open = true;
  const s = document.createElement("summary");
  s.textContent = title;
  const c = document.createElement("div");
  c.className = "fold-content" + (md ? " md" : "");
  if (md) c.innerHTML = mdToHtml(text || "");
  else c.textContent = text;
  d.appendChild(s);
  d.appendChild(c);
  return d;
}

// toolArgPreview 提取工具参数的单行预览：bash 取首行命令，其余取首参值
function toolArgPreview(name, args) {
  if (!args) return "";
  if ((name === "bash" || name === "sh") && typeof args.command === "string") {
    const line = args.command.trim().split("\n")[0];
    return "$ " + line;
  }
  for (const k of Object.keys(args)) {
    const v = args[k];
    if (typeof v === "string" && v.trim()) return v.trim().split("\n")[0];
    if (typeof v === "number" || typeof v === "boolean") return String(v);
  }
  return "";
}

// makeToolCard renders one tool call plus its committed result as a foldable
// card: bash shows the command as a $ line, other params as a key/value list,
// and the result (if any) under a labeled block.
function makeToolCard(call, res) {
  const d = document.createElement("details");
  d.className = "fold tool-card";
  const s = document.createElement("summary");
  const dot = document.createElement("span");
  dot.className = "tool-status " + (res ? (res.isError ? "error" : "done") : "done");
  s.appendChild(dot);
  const name = document.createElement("span");
  name.className = "t-name";
  name.textContent = call.name || "tool";
  s.appendChild(name);
  const preview = toolArgPreview(call.name, call.arguments);
  if (preview) {
    const arg = document.createElement("span");
    arg.className = "t-arg";
    arg.textContent = preview;
    s.appendChild(arg);
  }
  d.appendChild(s);

  const body = document.createElement("div");
  body.className = "fold-content tool-body";

  const args = call.arguments || {};
  if ((call.name === "bash" || call.name === "sh") && typeof args.command === "string") {
    const cmd = document.createElement("pre");
    cmd.className = "tool-cmd";
    cmd.textContent = "$ " + args.command;
    body.appendChild(cmd);
    const extra = Object.assign({}, args);
    delete extra.command;
    if (Object.keys(extra).length) appendToolParams(body, extra);
  } else {
    appendToolParams(body, args);
  }

  if (res) {
    const label = document.createElement("div");
    label.className = "tool-result-label" + (res.isError ? " error" : "");
    label.textContent = res.isError ? "错误" : "结果";
    body.appendChild(label);
    const pre = document.createElement("pre");
    pre.className = "tool-out" + (res.isError ? " error" : "");
    const out = stripAnsi(plainText(normalizeContent(res.content)));
    pre.textContent = out || "(空输出)";
    body.appendChild(pre);
  }
  d.appendChild(body);
  return d;
}

// appendToolParams renders a params object as compact key/value rows.
function appendToolParams(container, args) {
  for (const [k, v] of Object.entries(args)) {
    if (v === undefined || v === null || v === "") continue;
    const row = document.createElement("div");
    row.className = "tool-param";
    const key = document.createElement("span");
    key.className = "tp-key";
    key.textContent = k;
    const val = document.createElement("code");
    val.className = "tp-val";
    val.textContent = typeof v === "string" ? v : JSON.stringify(v);
    row.appendChild(key);
    row.appendChild(val);
    container.appendChild(row);
  }
}

function normalizeContent(content) {
  if (!content) return [];
  if (typeof content === "string") return [{ type: "text", text: content }];
  return content;
}

function plainText(blocks) {
  let out = "";
  for (const b of blocks) {
    if (b.type === "text") out += b.text || "";
    else if (b.type === "thinking") out += b.thinking || "";
  }
  return out;
}

/* Live streaming assistant card, throttled to one frame. */
function renderStreaming() {
  const pane = $("#messages");
  const stick = nearBottom(pane); // sample before any DOM growth
  const msg = state.streaming;
  if (!msg) {
    clearStreaming();
    return;
  }
  let el = state.streamingEl;
  if (!el) {
    el = document.createElement("div");
    el.className = "msg assistant streaming";
    const role = document.createElement("div");
    role.className = "role";
    role.textContent = "assistant";
    el.appendChild(role);
    el.appendChild(document.createElement("div"));
    el.lastChild.className = "body";
    const { live } = ensureLive();
    live.appendChild(el); // streaming tail stays at the very bottom
    state.streamingEl = el;
  }
  const body = el.querySelector(".body");
  body.textContent = "";
  const blocks = normalizeContent(msg.content);
  let text = "";
  for (const b of blocks) {
    // Tool call blocks are intentionally skipped while streaming: they only
    // become final when the message commits, and rendering them here would
    // put a tool card at the bottom that then "jumps" into place. Committed
    // messages and the in-flow execution chips carry the tool cards instead.
    if (b.type === "text") text += b.text || "";
    else if (b.type === "thinking") {
      body.appendChild(makeFold(thinkingTitle(b.thinking), b.thinking || "", false, true));
    }
  }
  const div = document.createElement("div");
  div.className = "md";
  div.innerHTML = mdToHtml(text);
  const cursor = document.createElement("span");
  cursor.className = "streaming-cursor";
  div.appendChild(cursor);
  body.appendChild(div);
  if (stick) pane.scrollTop = pane.scrollHeight;
}

/* Live tool execution cards. Each is inserted into the message flow (just
   before the live region) in the order it runs, and is replaced by its
   committed tool-result message once that arrives. */
function addToolChip(id, name, args, status) {
  if (state.toolChips.has(id)) return;
  const { live } = ensureLive();
  // A real <details> so the card folds natively (like the committed folds).
  const chip = document.createElement("details");
  chip.className = "fold live-chip";
  const sum = document.createElement("summary");
  const dot = document.createElement("span");
  dot.className = "tool-status " + status;
  const label = document.createElement("span");
  label.className = "t-name";
  label.textContent = name || "tool";
  sum.appendChild(dot);
  sum.appendChild(label);
  const preview = toolArgPreview(name, args);
  if (preview) {
    const arg = document.createElement("span");
    arg.className = "t-arg";
    arg.textContent = preview;
    sum.appendChild(arg);
  }
  chip.appendChild(sum);
  if (args && Object.keys(args).length) {
    const c = document.createElement("div");
    c.className = "fold-content";
    c.textContent = JSON.stringify(args, null, 2);
    chip.appendChild(c);
  }
  state.toolChips.set(id, { chip, dot, out: null });
  followScroll(() => live.before(chip)); // sit after the committed messages
}

// removeChip drops a live card (e.g. once its committed result is in place).
function removeChip(id) {
  const rec = state.toolChips.get(id);
  if (!rec) return;
  rec.chip.remove();
  state.toolChips.delete(id);
}

// clearToolChips removes any remaining live cards for a new run.
function clearToolChips() {
  for (const rec of state.toolChips.values()) {
    rec.chip.remove();
  }
  state.toolChips = new Map();
}

// updateToolChip id: tool_execution_end passes m.result, tool_execution_update
// passes m.partialResult; both are {content:[blocks...]} (or a plain string).
function toolResultText(result) {
  if (result == null) return "";
  if (typeof result === "string") return result;
  if (Array.isArray(result.content)) {
    return plainText(normalizeContent(result.content));
  }
  return "";
}

function updateToolChip(id, result, status) {
  const rec = state.toolChips.get(id);
  if (!rec) return;
  rec.dot.className = "tool-status " + status;
  const text = stripAnsi(toolResultText(result));
  if (text) {
    followScroll(() => {
      if (!rec.out) {
        rec.out = document.createElement("div");
        rec.out.className = "fold-content";
        rec.chip.appendChild(rec.out);
      }
      rec.out.textContent = text;
    });
  }
}

function showPermAsk(m) {
  state.permAsk = m;
  const req = m.request || {};
  const el = $("#perm-detail");
  el.textContent = "";
  const pre = document.createElement("pre");
  pre.textContent =
    "命令: " + (req.command || "") + "\n" +
    "工具: " + (req.toolName || "") + "\n" +
    "路径: " + (req.path || "") + "\n" +
    "说明: " + (req.reason || "") + "\n" +
    "目录: " + (req.cwd || "");
  el.appendChild(pre);
  $("#perm").classList.remove("hidden");
}

/* ---------------- attachments ---------------- */

// humanSize renders a byte count for chips and cards.
function humanSize(n) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + "GiB";
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + "MiB";
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + "KiB";
  return n + "B";
}

// uploadFiles stages files via POST /api/uploads; each success lands in
// state.pendingUploads and shows as a chip, to be sent with the next message.
function uploadFiles(files) {
  if (!files || !files.length) return;
  const fd = new FormData();
  for (const f of files) fd.append("file", f, f.name);
  setStatusText("上传中…");
  api("/api/uploads", { method: "POST", body: fd })
    .then((r) => r.json())
    .then((j) => {
      if (j.error) throw new Error(j.error);
      for (const u of j.uploads || []) {
        if (u.error) {
          setStatusText("「" + u.name + "」上传失败: " + u.error);
          continue;
        }
        state.pendingUploads.push({ id: u.id, name: u.name, size: u.size, mimeType: u.mimeType });
      }
      renderAttachTray();
      setStatusText("");
    })
    .catch((e) => setStatusText("上传失败: " + String(e.message || e)));
}

// renderAttachTray redraws the staged-attachment chips above the input.
function renderAttachTray() {
  const tray = $("#attach-tray");
  tray.innerHTML = "";
  for (const u of state.pendingUploads) {
    const chip = document.createElement("span");
    chip.className = "attach-chip";
    const name = document.createElement("span");
    name.className = "name";
    name.textContent = u.name;
    name.title = u.name + " (" + u.mimeType + ")";
    const size = document.createElement("span");
    size.className = "size";
    size.textContent = humanSize(u.size);
    const rm = document.createElement("button");
    rm.textContent = "✕";
    rm.title = "移除附件";
    rm.addEventListener("click", () => {
      state.pendingUploads = state.pendingUploads.filter((x) => x.id !== u.id);
      renderAttachTray();
    });
    chip.append(name, size, rm);
    tray.appendChild(chip);
  }
  tray.classList.toggle("hidden", state.pendingUploads.length === 0);
}

// attachmentFileCard builds the chip shown in a sent user message for a
// reference block (server-side uploads path: /uploads/<id>/<name>).
function attachmentFileCard(text) {
  const firstLine = text.split("\n")[0] || "";
  const m = firstLine.match(/^\[附件 [^\]]+\] (.+)$/);
  if (!m) return null;
  const path = m[1];
  const parts = path.split("/");
  const id = parts[parts.length - 2] || "";
  const name = parts[parts.length - 1] || "file";
  const sizeMatch = text.match(/size=(\d+)/);
  const a = document.createElement("a");
  a.className = "attachment-file";
  a.href = "/api/uploads/" + encodeURIComponent(id) + "/content";
  a.target = "_blank";
  a.rel = "noopener";
  a.textContent = "📄 " + name;
  const meta = document.createElement("span");
  meta.className = "meta";
  meta.textContent = sizeMatch ? humanSize(Number(sizeMatch[1])) : "";
  a.appendChild(meta);
  return a;
}

/* ---------------- composer ---------------- */
function resizeInput(input) {
  input.style.height = "auto";
  input.style.height = Math.min(input.scrollHeight, 160) + "px";
}

// recallHistory browses submitted messages with ArrowUp (dir -1, older) and
// ArrowDown (dir +1, newer). It starts from the most recent submission and
// returns to the draft when reaching the bottom — useful to re-edit a
// message that was interrupted.
function recallHistory(dir) {
  const input = $("#input");
  const n = state.history.length;
  if (n === 0) return;
  if (dir < 0) {
    if (state.historyIdx === -1) {
      state.historyDraft = input.value;
      state.historyIdx = 0;
    } else if (state.historyIdx < n - 1) {
      state.historyIdx++;
    } else {
      return;
    }
  } else {
    if (state.historyIdx > 0) {
      state.historyIdx--;
    } else if (state.historyIdx === 0) {
      state.historyIdx = -1;
    } else {
      return;
    }
  }
  input.value = state.historyIdx === -1 ? state.historyDraft : state.history[state.historyIdx];
  const len = input.value.length;
  input.setSelectionRange(len, len);
  resizeInput(input);
}

function sendMessage() {
  const input = $("#input");
  const text = input.value.trim();
  const attachments = state.pendingUploads.map((u) => u.id);
  if ((!text && !attachments.length) || state.busy) return;
  input.disabled = true;
  state.history.unshift(text);
  if (state.history.length > 50) state.history.pop();
  state.historyIdx = -1;
  api("/api/prompt", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify(attachments.length ? { text, attachments } : { text }),
  })
    .then((r) => {
      if (r.status === 403) {
        // Session is read-only: another window / the TUI is using it. Queue
        // for control and surface the explicit force-takeover affordance.
        setReadOnly(true);
        setStatusText("会话正被其他窗口使用（只读），正在请求接管…可点「强制夺取」立即接管");
        claimControl();
        throw null; // already surfaced by claimControl
      }
      if (r.status === 409) throw new Error("agent 忙，请稍候");
      if (!r.ok) return r.json().then((j) => { throw new Error(j.error || ("发送失败 (" + r.status + ")")); });
      // 乐观置忙：202 到首个 agent 事件之间输入保持禁用，避免短暂可再次提交
      setBusy(true);
      input.value = "";
      resizeInput(input);
      state.pendingUploads = [];
      renderAttachTray();
    })
    .catch((e) => { if (e) setStatusText(String(e.message || e)); })
    .finally(() => { input.disabled = state.busy; $("#send-btn").disabled = state.busy; $("#attach-btn").disabled = state.busy; });
}

// claimControl requests write control of the current session: owned
// immediately when uncontended, otherwise queued (FIFO) until the current
// owner switches away or disconnects.
function claimControl() {
  api("/api/session/claim", { method: "POST" })
    .then((r) => r.json())
    .then((j) => {
      if (j.owned) {
        setReadOnly(false);
        setStatusText("已获得会话控制权，可以继续操作");
      } else if (j.queued) {
        setReadOnly(true);
        setStatusText("已排队：等待当前使用窗口让出该会话后自动接管，或点「强制夺取」立即接管");
      } else {
        setStatusText("会话控制权请求失败");
      }
    })
    .catch((e) => setStatusText("接管失败: " + String(e.message || e)));
}

// setReadOnly toggles the window's read-only affordance: when another window /
// the TUI holds the session, the 强制夺取 button appears so the user can
// deliberately take over instead of waiting forever for the old owner.
function setReadOnly(readOnly) {
  state.readOnly = readOnly;
  $("#takeover-btn").classList.toggle("hidden", !readOnly);
}

// takeoverControl explicitly, deliberately forces write control of the current
// session to this window, releasing whoever owns it now (强夺). This exists so
// a window that is blocked/queued does not have to hunt down which window
// holds the lock.
async function takeoverControl() {
  if (!(await desktopConfirm("强制夺取将立即释放当前持有窗口对本会话的控制权，由本窗口接管。确定？"))) {
    return;
  }
  api("/api/session/takeover", { method: "POST" })
    .then((r) => r.json())
    .then((j) => {
      if (j.owned) {
        setReadOnly(false);
        setStatusText(j.prevOwner ? "已强制夺取控制权（原持有者: " + j.prevOwner + "）" : "已获得会话控制权");
        fetchStatus();
      } else {
        setStatusText("强制夺取失败");
      }
    })
    .catch((e) => setStatusText("强制夺取失败: " + String(e.message || e)));
}

/* ---------------- Skills panel ---------------- */

// installSkillZip 上传技能 zip → dry-run 预览 → 用户确认 → 安装并热刷新。
async function installSkillZip(file) {
  if (!file) return;
  try {
    setStatusText("上传技能包…");
    const fd = new FormData();
    fd.append("file", file, file.name);
    const up = await api("/api/uploads", { method: "POST", body: fd }).then((r) => r.json());
    if (up.error) throw new Error(up.error);
    const u = (up.uploads || []).find((x) => !x.error);
    if (!u) throw new Error("上传失败");
    const applyURL = "/api/uploads/" + u.id + "/apply";
    const dry = await api(applyURL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ kind: "skills-zip" }),
    }).then((r) => r.json());
    if (!dry.ok) throw new Error(dry.error || "解析失败");
    if (!(await desktopConfirm(dry.diff + "\n\n安装到全局技能目录并热刷新？"))) return;
    const done = await api(applyURL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ kind: "skills-zip", confirm: true }),
    }).then((r) => r.json());
    if (!done.ok) throw new Error(done.error || "安装失败");
    setStatusText(done.error ? "已安装但刷新失败: " + done.error : "技能已安装: " + (done.installed || []).join(", "));
    openSkills(); // 刷新面板
  } catch (e) {
    setStatusText("安装失败: " + String(e.message || e));
  }
}

function openSkills() {
  const body = $("#settings-body");
  body.textContent = "";
  const loading = document.createElement("div");
  loading.className = "mesh-muted";
  loading.textContent = "加载中…";
  body.appendChild(loading);
  Promise.all([
    api("/api/skills", { credentials: "same-origin" }),
    api("/api/session/skills", { credentials: "same-origin" }),
  ])
    .then(([listRes, sessRes]) =>
      Promise.all([listRes.ok ? listRes.json() : null, sessRes.ok ? sessRes.json() : null])
    )
    .then(([list, sess]) => {
      if (state.settingsTab !== "skills") return;
      renderSkillsModal(body, list, sess);
    })
    .catch((e) => {
      if (state.settingsTab !== "skills") return;
      body.textContent = String(e.message || e);
    });
}

function renderSkillsModal(body, list, sess) {
  body.textContent = "";

  // 安装技能包：上传 zip → dry-run 预览 → 确认后安装并热刷新。
  const installSec = document.createElement("div");
  installSec.className = "mesh-section";
  const installTitle = document.createElement("div");
  installTitle.className = "mesh-section-title";
  installTitle.textContent = "安装技能包";
  installSec.appendChild(installTitle);
  const installBtn = document.createElement("button");
  installBtn.textContent = "⇪ 从 zip 安装";
  installBtn.title = "上传技能 zip 包（SKILL.md + 资源），安装到全局技能目录并热刷新";
  installBtn.addEventListener("click", () => $("#skill-zip-input").click());
  installSec.appendChild(installBtn);
  body.appendChild(installSec);

  // 本会话：当前 AgentSession 实际加载的技能（skill 工具背后的集合）。
  const sessSec = document.createElement("div");
  sessSec.className = "mesh-section";
  const sessTitle = document.createElement("div");
  sessTitle.className = "mesh-section-title";
  sessTitle.textContent = "本会话";
  sessSec.appendChild(sessTitle);
  const sessionSkills = (sess && sess.skills) || [];
  if (!sessionSkills.length) {
    const p = document.createElement("div");
    p.className = "mesh-muted";
    p.textContent = "本会话未加载任何技能（技能目录为空）。模型可用的技能见下方「已安装」列表。";
    sessSec.appendChild(p);
  } else {
    const p = document.createElement("div");
    p.className = "mesh-muted";
    p.textContent = "这些技能已注入本会话：模型可通过 skill 工具加载其正文，或用 /skill:name 直接触发。";
    sessSec.appendChild(p);
    for (const sk of sessionSkills) sessSec.appendChild(skillRow(sk, false));
  }
  body.appendChild(sessSec);

  // 已安装技能（文件系统发现结果）。
  const installed = (list && list.skills) || [];
  const sec = document.createElement("div");
  sec.className = "mesh-section";
  const sh = document.createElement("div");
  sh.className = "mesh-section-title";
  sh.textContent = installed.length ? "已安装 " + installed.length + " 个" : "未发现已安装技能";
  sec.appendChild(sh);
  for (const sk of installed) sec.appendChild(skillRow(sk, true));
  body.appendChild(sec);

  // 发现目录。
  if (list && list.dirs && list.dirs.length) {
    const dirs = document.createElement("div");
    dirs.className = "mcp-tools";
    dirs.textContent = "发现目录:\n" + list.dirs.map((d) => "  " + d).join("\n");
    body.appendChild(dirs);
  }
}

// skillRow renders one skill line: name + one-line-clamped description +
// invocation badge. Installed skills additionally get a global enable/disable
// toggle and a delete button (session rows are read-only).
function skillRow(sk, installed) {
  const row = document.createElement("div");
  row.className = "skill-sess-row";
  const name = document.createElement("span");
  name.className = "skill-sess-name";
  name.textContent = sk.name;
  name.title = sk.location;
  row.appendChild(name);
  const desc = document.createElement("span");
  desc.className = "skill-sess-desc";
  desc.textContent = sk.description || "";
  desc.title = sk.description || "";
  row.appendChild(desc);
  const badge = document.createElement("span");
  badge.className = "mesh-badge " + (sk.disabled ? "no" : sk.disableModelInvocation ? "warn" : "ok");
  badge.textContent = sk.disabled ? "已全局停用" : sk.disableModelInvocation ? "仅手动" : "可模型调用";
  row.appendChild(badge);
  if (installed) {
    const toggle = document.createElement("button");
    toggle.textContent = sk.disabled ? "启用" : "停用";
    toggle.title = sk.disabled ? "从全局禁用列表移除，恢复模型调用" : "全局停用该技能（写入 settings）";
    toggle.addEventListener("click", async () => {
      toggle.disabled = true;
      try {
        const r = await api("/api/skills/" + encodeURIComponent(sk.name) + "/enabled", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ enabled: !!sk.disabled }),
        });
        if (!r.ok) throw new Error((await r.json()).error || "设置失败");
        openSkills();
      } catch (e) {
        setStatusText("技能开关失败：" + (e.message || e));
        toggle.disabled = false;
      }
    });
    row.appendChild(toggle);
    const del = document.createElement("button");
    del.textContent = "✕";
    del.title = "删除该技能目录（不可恢复）";
    del.addEventListener("click", async () => {
      if (!(await desktopConfirm("删除技能 " + sk.name + "？其目录将被移除，不可恢复。"))) return;
      del.disabled = true;
      try {
        const r = await api("/api/skills/" + encodeURIComponent(sk.name), { method: "DELETE" });
        if (!r.ok) throw new Error((await r.json()).error || "删除失败");
        openSkills();
      } catch (e) {
        setStatusText("技能删除失败：" + (e.message || e));
        del.disabled = false;
      }
    });
    row.appendChild(del);
  }
  return row;
}

/* ---------------- 设置中心：模型 / MCP / 技能 ---------------- */
const settingsTabs = { models: "模型", mcp: "MCP 服务器", skills: "技能", mesh: "Mesh 网络", perm: "权限策略" };
let settingsModelProvider = ""; // 模型 tab 当前选中的 provider

function openSettings(tab) {
  tab = tab || "models";
  if (!(tab in settingsTabs)) tab = "models";
  state.settingsTab = tab; // 异步渲染前校验，防止慢响应覆盖后点开的 tab
  $("#settings").classList.remove("hidden");
  for (const el of document.querySelectorAll(".settings-tab")) {
    el.classList.toggle("current", el.dataset.tab === tab);
  }
  $("#settings-title").textContent = settingsTabs[tab];
  if (tab === "models") loadSettingsModels();
  else if (tab === "mcp") openMcp();
  else if (tab === "mesh") openMeshDesktop();
  else if (tab === "perm") loadSettingsPerm();
  else openSkills();
}

// 模型 tab：左列 provider 分组，右列该 provider 的模型行（对照桌面端模型设置页）
function loadSettingsModels() {
  const body = $("#settings-body");
  body.textContent = "";
  const loading = document.createElement("div");
  loading.className = "mesh-muted";
  loading.textContent = "加载中…";
  body.appendChild(loading);
  // 只展示 models.json 里配置过的供应商；内置未配置的不出现
  const tab = state.settingsTab;
  api("/api/config/models", { credentials: "same-origin" })
    .then((r) => (r.ok ? r.json() : []))
    .then((custom) => {
      if (state.settingsTab !== tab) return;
      renderSettingsModels(Array.isArray(custom) ? custom : []);
    })
    .catch((e) => {
      if (state.settingsTab !== tab) return;
      body.textContent = "加载模型失败：" + (e.message || e);
    });
}

function renderSettingsModels(custom) {
  const body = $("#settings-body");
  body.textContent = "";
  const customMap = new Map(custom.map((m) => [m.provider + "/" + m.id, m]));
  const providers = [...new Set(custom.map((m) => m.provider))];
  if (!settingsModelProvider || !providers.includes(settingsModelProvider)) {
    settingsModelProvider = providers[0] || "";
  }

  const wrap = document.createElement("div");
  wrap.className = "model-split";
  const nav = document.createElement("div");
  nav.className = "model-providers";
  for (const p of providers) {
    const b = document.createElement("button");
    b.className = "model-provider" + (p === settingsModelProvider ? " current" : "");
    b.textContent = p;
    b.addEventListener("click", () => {
      settingsModelProvider = p;
      renderSettingsModels(custom);
    });
    nav.appendChild(b);
  }
  // 单独的"添加供应商"入口：命名后直接进添加模型表单
  const addProv = document.createElement("button");
  addProv.className = "model-provider add";
  addProv.textContent = "＋ 添加供应商";
  addProv.title = "新增一个自定义供应商，随第一个模型一起保存";
  addProv.addEventListener("click", async () => {
    const name = await desktopPrompt("新供应商名称（provider 标识）");
    if (!name || !name.trim()) return;
    settingsModelProvider = name.trim();
    openModelForm(null, customMap, name.trim());
  });
  nav.appendChild(addProv);

  const list = document.createElement("div");
  list.className = "model-list-panel";
  const head = document.createElement("div");
  head.className = "model-list-head";
  const title = document.createElement("div");
  title.className = "mesh-section-title";
  const count = custom.filter((m) => m.provider === settingsModelProvider).length;
  title.textContent = settingsModelProvider
    ? settingsModelProvider + " · " + count + " 个模型"
    : "尚未配置任何自定义模型";
  head.appendChild(title);
  const add = document.createElement("button");
  add.className = "primary";
  add.textContent = "＋ 添加模型";
  add.addEventListener("click", () => openModelForm(null, customMap));
  head.appendChild(add);
  list.appendChild(head);

  for (const m of custom) {
    if (m.provider !== settingsModelProvider) continue;
    list.appendChild(modelRow(m, customMap.get(m.provider + "/" + m.id) || null));
  }
  wrap.appendChild(nav);
  wrap.appendChild(list);
  body.appendChild(wrap);
}

function modelRow(m, custom) {
  const row = document.createElement("div");
  row.className = "model-row";
  const info = document.createElement("div");
  info.className = "model-row-info";
  const nameLine = document.createElement("div");
  nameLine.className = "model-row-name";
  const name = document.createElement("span");
  name.textContent = m.id;
  nameLine.appendChild(name);
  const badges = document.createElement("span");
  badges.className = "menu-badges";
  if ((m.input || []).includes("image")) badges.appendChild(menuBadge("视觉"));
  if (m.reasoning) badges.appendChild(menuBadge("推理"));
  if (m.contextWindow >= 1e6) badges.appendChild(menuBadge("1M"));
  if (badges.childNodes.length) nameLine.appendChild(badges);
  info.appendChild(nameLine);
  const sub = document.createElement("div");
  sub.className = "model-row-sub";
  sub.textContent = (m.api || "") + (m.contextWindow ? " · " + fmtNum(m.contextWindow) + " ctx" : "");
  info.appendChild(sub);
  row.appendChild(info);

  const ops = document.createElement("div");
  ops.className = "model-row-ops";
  const edit = document.createElement("button");
  edit.textContent = "编辑";
  if (custom) {
    edit.addEventListener("click", () => openModelForm(custom, new Map([[m.provider + "/" + m.id, custom]])));
  } else {
    edit.disabled = true;
    edit.title = "内置模型不可编辑";
  }
  ops.appendChild(edit);
  const del = document.createElement("button");
  del.textContent = "删除";
  if (custom) {
    del.addEventListener("click", async () => {
      if (!(await desktopConfirm("删除模型 " + m.provider + "/" + m.id + "？"))) return;
      del.disabled = true;
      api("/api/config/models/" + encodeURIComponent(m.provider) + "/" + encodeURIComponent(m.id), { method: "DELETE" })
        .then(async (r) => {
          if (!r.ok) throw new Error((await r.json()).error || "删除失败");
          setStatusText("已删除 " + m.provider + "/" + m.id);
          loadSettingsModels();
        })
        .catch((e) => {
          setStatusText(String(e.message || e));
          del.disabled = false;
        });
    });
  } else {
    del.disabled = true;
    del.title = "内置模型不可删除";
  }
  ops.appendChild(del);
  row.appendChild(ops);
  return row;
}

// 添加/编辑模型弹窗：编辑时用自定义模型的完整字段预填（含 baseUrl）
function openModelForm(m, customMap, presetProvider) {
  $("#model-form-title").textContent = m ? "编辑模型" : "添加模型";
  $("#mf-provider").value = m ? m.provider : (presetProvider || settingsModelProvider || "");
  $("#mf-id").value = m ? m.id : "";
  $("#mf-baseurl").value = m && m.baseUrl ? m.baseUrl : "";
  $("#mf-api").value = (m && m.api) || "openai-completions";
  $("#mf-ctx").value = m && m.contextWindow ? m.contextWindow : "";
  $("#mf-max").value = m && m.maxTokens ? m.maxTokens : "";
  $("#mf-in-image").checked = !!(m && (m.input || []).includes("image"));
  $("#mf-reasoning").checked = !!(m && m.reasoning);
  $("#model-form").classList.remove("hidden");
  // 保存按钮按本轮打开的模型重绑，避免闭包串味
  const save = $("#mf-save");
  const clone = save.cloneNode(true);
  save.replaceWith(clone);
  clone.addEventListener("click", () => saveModelForm(m));
}

function saveModelForm(orig) {
  const provider = $("#mf-provider").value.trim();
  const id = $("#mf-id").value.trim();
  if (!provider || !id) {
    setStatusText("provider 和模型 ID 不能为空");
    return;
  }
  const input = [];
  if ($("#mf-in-text").checked) input.push("text");
  if ($("#mf-in-image").checked) input.push("image");
  const def = {
    id: id,
    name: id,
    api: $("#mf-api").value,
    provider: provider,
    baseUrl: $("#mf-baseurl").value.trim(),
    contextWindow: parseInt($("#mf-ctx").value, 10) || 0,
    maxTokens: parseInt($("#mf-max").value, 10) || 0,
    input: input,
    reasoning: $("#mf-reasoning").checked,
  };
  if (orig && orig.cost) def.cost = orig.cost; // 编辑保留原计费配置
  api("/api/config/models", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(def),
  })
    .then(async (r) => {
      if (!r.ok) throw new Error((await r.json()).error || "保存失败");
      $("#model-form").classList.add("hidden");
      setStatusText("模型已保存：" + provider + "/" + id);
      loadSettingsModels();
      loadModels();
    })
    .catch((e) => setStatusText(String(e.message || e)));
}

/* ---------------- markdown-lite ---------------- */
function escapeHtml(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

// ANSI_RE matches terminal escape sequences (SGR colors, cursor moves, OSC
// title, etc.). Bash output is captured with raw escapes; they must be
// stripped before display or they render as garbled control characters.
const ANSI_RE =
  /[\u001B\u009B][[\]()#;?]*(?:(?:(?:[a-zA-Z\d]*(?:;[-\da-zA-Z\d/#&.:=?%@~_]*)*)?\u0007)|(?:(?:\d{1,4}(?:;\d{0,4})*)?[\dA-PR-TZcf-nq-uy=><~]))/g;

function stripAnsi(s) {
  return String(s || "").replace(ANSI_RE, "");
}

function safeUrl(u) {
  const t = String(u).trim();
  return /^(https?:|#|\/)/i.test(t) ? t : "";
}

/* userTextHtml renders a plain user message: escaped, inline formatting,
   line breaks preserved. */
function userTextHtml(text) {
  const escaped = escapeHtml(String(text || "").replace(/\r\n/g, "\n"));
  return inline(escaped).replace(/\n/g, "<br>");
}

/* Minimal markdown renderer. All input is escaped before transformation, so
   the injected tags above are the only HTML produced. */
function mdToHtml(src, block = true) {
  const esc = escapeHtml(src);

  // Extract fenced code blocks first.
  const fences = [];
  let s = esc.replace(/```([^\n`]*)\n?([\s\S]*?)```/g, (_m, lang, code) => {
    const idx = fences.length;
    fences.push(lang + ":" + code);
    return "\u0001F" + idx + "\u0001";
  });

  const lines = s.split("\n");
  let html = "";
  let list = null; // "ul" | "ol" | null
  let para = "";

  const flushPara = () => {
    if (para) { html += "<p>" + inline(para.trim()) + "</p>"; para = ""; }
  };
  const closeList = () => {
    if (list) { html += "</" + list + ">"; list = null; }
  };
  const flushLine = (line) => {
    flushPara();
    closeList();
    if (line) html += line + "\n";
  };

  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i];
    if (raw.startsWith("\u0001F")) {
      flushPara(); closeList();
      const idx = parseInt(raw.slice(2, -1), 10);
      const parts = fences[idx].split(":");
      const lang = parts[0], code = parts.slice(1).join(":");
      html += "<pre><code" + (lang ? ' data-lang="' + lang + '"' : "") + ">" + (code.replace(/^\n+|\n+$/g, "") || " ") + "</code></pre>\n";
      continue;
    }
    // GFM table: a row followed by a separator row starts a table block.
    if (isTableRow(raw) && i + 1 < lines.length && isTableSeparator(lines[i + 1])) {
      flushPara(); closeList();
      const t = renderTable(lines, i);
      html += t.html + "\n";
      i = t.nextIdx - 1;
      continue;
    }
    if (/^\s*$/.test(raw)) { flushPara(); closeList(); continue; }
    if (/^#{1,3}\s/.test(raw)) {
      flushLine("<h" + Math.min(3, raw.match(/^#+/)[0].length) + ">" +
        inline(raw.replace(/^#+\s*/, "")) + "</h" + Math.min(3, raw.match(/^#+/)[0].length) + ">");
      continue;
    }
    // Thematic break: a line of 3+ -, * or _ (GFM horizontal rule).
    if (/^\s{0,3}(?:-{3,}|\*{3,}|_{3,})\s*$/.test(raw)) {
      flushPara(); closeList();
      html += "<hr>\n";
      continue;
    }
    if (/^>\s?/.test(raw)) {
      flushPara(); closeList();
      html += "<blockquote>" + inline(raw.replace(/^>\s?/, "")) + "</blockquote>\n";
      continue;
    }
    let m = raw.match(/^(\s*)([-*+])\s+(.*)$/);
    if (m && /^\s*$/.test(m[1])) {
      flushPara();
      if (list !== "ul") { closeList(); html += "<ul>\n"; list = "ul"; }
      html += "<li>" + inline(m[3]) + "</li>\n";
      continue;
    }
    m = raw.match(/^(\s*)(\d+)[.)]\s+(.*)$/);
    if (m && /^\s*$/.test(m[1])) {
      flushPara();
      if (list !== "ol") { closeList(); html += "<ol>\n"; list = "ol"; }
      html += "<li>" + inline(m[3]) + "</li>\n";
      continue;
    }
    para += (para ? " " : "") + raw;
  }
  flushPara();
  closeList();

  return block ? html : html.replace(/^<\/?p>\s*/g, "");
}

// isTableRow reports whether a line looks like a GFM table row (has |).
function isTableRow(line) {
  return line.includes("|");
}

// isTableSeparator reports whether every cell is a dash/colon alignment row.
function isTableSeparator(line) {
  const cells = tableCells(line);
  if (!cells.length) return false;
  return cells.every((c) => /^:?-+:?$/.test(c));
}

// tableCells splits a row into trimmed cells, tolerating missing edge pipes.
function tableCells(line) {
  let s = String(line).trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|")) s = s.slice(0, -1);
  return s.split("|").map((c) => c.trim());
}

// renderTable consumes a GFM table starting at lines[i] (header row) and
// returns its HTML plus the index just past the last body row.
function renderTable(lines, i) {
  const align = (sepCell) => {
    if (sepCell.startsWith(":") && sepCell.endsWith(":")) return "center";
    if (sepCell.endsWith(":")) return "right";
    if (sepCell.startsWith(":")) return "left";
    return "";
  };
  const aligns = tableCells(lines[i + 1]).map(align);
  const cellTag = (tag, cell, a) => {
    const style = a ? ' style="text-align:' + a + '"' : "";
    return "<" + tag + style + ">" + inline(cell) + "</" + tag + ">";
  };

  const head = tableCells(lines[i]);
  let out = "<table>\n<thead>\n<tr>";
  head.forEach((c, j) => { out += cellTag("th", c, aligns[j]); });
  out += "</tr>\n</thead>\n<tbody>\n";

  let k = i + 2;
  for (; k < lines.length; k++) {
    const row = lines[k];
    if (!isTableRow(row) || /^\s*$/.test(row)) break;
    const cells = tableCells(row);
    out += "<tr>";
    for (let j = 0; j < cells.length; j++) {
      out += cellTag("td", cells[j], aligns[j]);
    }
    out += "</tr>\n";
  }
  out += "</tbody>\n</table>";
  return { html: out, nextIdx: k };
}

/* Inline formatting, applied to already-escaped text. */
function inline(s) {
  return s
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[^*])\*([^*]+)\*(?!\*)/g, "$1<em>$2</em>")
    .replace(/!\[([^\]]*)\]\(([^)]+)\)/g, (_m, alt, url) => {
      const u = safeUrl(url);
      return u ? `<img alt="${escapeHtml(alt)}" src="${u}">` : escapeHtml(alt);
    })
    .replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_m, label, url) => {
      const u = safeUrl(url);
      const L = escapeHtml(label);
      return u ? `<a href="${u}" target="_blank" rel="noopener">${L}</a>` : L;
    });
}

// Stick-to-bottom scrolling: output follows the bottom only while the user
// is already there; scrolling up pauses following, returning to the bottom
// resumes it. The bottom test must run BEFORE the DOM mutation — checking
// afterwards would misread freshly appended content taller than the
// threshold as a deliberate scroll-up and silently stop following.
function nearBottom(el) {
  return el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}

function followScroll(mut) {
  const m = $("#messages");
  const stick = nearBottom(m);
  mut();
  if (stick) m.scrollTop = m.scrollHeight;
}

document.addEventListener("DOMContentLoaded", init);

/* ============================ desktop 定制 ============================ */

document.body.classList.add("desktop");

// Go 菜单/托盘经 ExecJS 调用这里，复用页面自身的事件绑定
window.dogeMenu = {
  newTask: () => $("#sb-new").click(),
  abort: () => $("#abort-btn").click(),
  openSettings: () => $("#settings-btn").click(),
  reloadConfig: () => $("#nav-reload").click(),
};

/* ============ desktop：转录分片加载（大转录防冻结） ============ */
state.msgOffset = 0;
state.loadingEarlier = false;

// 上滑到顶部自动加载更早消息（无需手动点按钮）
$("#messages").addEventListener("scroll", () => {
  const m = $("#messages");
  if (m.scrollTop < 120 && state.msgOffset > 0 && !state.loadingEarlier) loadEarlier();
});

function loadEarlier() {
  if (state.msgOffset <= 0 || state.loadingEarlier) return;
  state.loadingEarlier = true;
  setStatusText("正在加载更早消息…");
  const m = $("#messages");
  const prevHeight = m.scrollHeight;
  const prevTop = m.scrollTop;
  api("/api/session/messages?before=" + state.msgOffset + "&limit=30")
    .then((r) => r.json())
    .then((j) => {
      const got = j.messages || [];
      state.messages = got.concat(state.messages);
      state.msgOffset = typeof j.offset === "number" ? j.offset : state.msgOffset - got.length;
      renderAll();
      // 顶部补页：锚定用户当前阅读位置，不跳底不跳动
      m.scrollTop = m.scrollHeight - prevHeight + prevTop;
    })
    .catch((e) => setStatusText(String((e && e.message) || e)))
    .finally(() => {
      state.loadingEarlier = false;
      setStatusText("");
      // 锚定后仍贴着顶部（一批不够铺满一屏）则继续补
      if (m.scrollTop < 120 && state.msgOffset > 0) loadEarlier();
    });
}

/* ===== desktop：confirm/prompt 替身（WKWebView 未实现 JS 弹窗委托，
   原生 confirm 恒返回 false 导致所有删除/确认静默失效） ===== */
function desktopModal(opts) {
  // opts: { title, message, input, defaultValue } → Promise<bool | string | null>
  return new Promise((resolve) => {
    const box = document.createElement("div");
    box.className = "modal";
    const inner = document.createElement("div");
    inner.className = "modal-box";
    const h = document.createElement("h3");
    h.textContent = opts.title || "确认";
    inner.appendChild(h);
    if (opts.message) {
      const msg = document.createElement("div");
      msg.className = "desktop-modal-msg";
      msg.textContent = opts.message;
      inner.appendChild(msg);
    }
    let inp = null;
    if (opts.input) {
      inp = document.createElement("input");
      inp.value = opts.defaultValue || "";
      inp.spellcheck = false;
      inner.appendChild(inp);
    }
    const actions = document.createElement("div");
    actions.className = "modal-actions";
    const cancel = document.createElement("button");
    cancel.textContent = "取消";
    const ok = document.createElement("button");
    ok.className = "primary";
    ok.textContent = "确定";
    actions.append(cancel, ok);
    inner.appendChild(actions);
    box.appendChild(inner);
    document.body.appendChild(box);
    const done = (v) => { box.remove(); resolve(v); };
    cancel.onclick = () => done(opts.input ? null : false);
    ok.onclick = () => done(opts.input ? inp.value : true);
    box.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); ok.click(); }
      if (e.key === "Escape") done(opts.input ? null : false);
    });
    (inp || ok).focus();
  });
}
const desktopConfirm = (msg) => desktopModal({ title: "确认", message: msg });
const desktopPrompt = (msg, def) => desktopModal({ title: "输入", message: msg, input: true, defaultValue: def });


/* ==================== ⛨ 权限策略 tab + Mesh 开关/权限（桌面专属） ==================== */

/* ---------- 卡片式表单构件 ---------- */

function dsec(title, sub) {
  const s = document.createElement("div");
  s.className = "dsec";
  const head = document.createElement("div");
  head.className = "dsec-head";
  const t = document.createElement("div");
  t.className = "dsec-title";
  t.textContent = title;
  head.appendChild(t);
  if (sub) {
    const u = document.createElement("div");
    u.className = "dsec-sub";
    u.textContent = sub;
    head.appendChild(u);
  }
  s.appendChild(head);
  return s;
}

function drow(label, sub, ctrl) {
  const r = document.createElement("div");
  r.className = "drow";
  const l = document.createElement("div");
  l.className = "drow-label";
  l.textContent = label;
  if (sub) {
    const u = document.createElement("div");
    u.className = "dsub";
    u.textContent = sub;
    l.appendChild(u);
  }
  const c = document.createElement("div");
  c.className = "drow-ctrl";
  c.appendChild(ctrl);
  r.append(l, c);
  return r;
}

function dswitch(checked) {
  const w = document.createElement("label");
  w.className = "dswitch";
  const inp = document.createElement("input");
  inp.type = "checkbox";
  inp.checked = !!checked;
  const knob = document.createElement("i");
  w.append(inp, knob);
  return { el: w, inp };
}

const permDecisionOpts = [
  ["", "缺省（allow）"],
  ["allow", "allow 放行"],
  ["ask", "ask 询问"],
  ["deny", "deny 拒绝"],
];
const autoDecisionOpts = [
  ["", "缺省（deny）"],
  ["allow", "allow"],
  ["deny", "deny"],
  ["llm", "llm（LLM 审批）"],
];

function permSelect(cur, opts) {
  const sel = document.createElement("select");
  for (const [v, label] of opts || permDecisionOpts) {
    const o = document.createElement("option");
    o.value = v;
    o.textContent = label;
    if ((cur || "") === v) o.selected = true;
    sel.appendChild(o);
  }
  return sel;
}

function permRuleRows(list, isBash) {
  const box = document.createElement("div");
  box.className = "drules";
  const rows = [];
  const addRow = (pattern, decision) => {
    const row = document.createElement("div");
    row.className = "drule-row";
    const pat = document.createElement("input");
    pat.value = pattern || "";
    pat.placeholder = isBash ? "命令 glob，如 git push*" : "tool 或 tool:path/glob";
    const dec = permSelect(decision);
    const del = document.createElement("button");
    del.textContent = "✕";
    del.title = "删除该规则";
    del.addEventListener("click", () => {
      rows.splice(rows.indexOf(row), 1);
      row.remove();
    });
    row.append(pat, dec, del);
    rows.push(row);
    box.insertBefore(row, add);
  };
  (list || []).forEach((r) => addRow(r.pattern, r.decision));
  const add = document.createElement("button");
  add.className = "drule-add";
  add.textContent = "＋ 添加规则";
  add.addEventListener("click", () => addRow("", "ask"));
  box.appendChild(add);
  return {
    el: box,
    collect() {
      const out = [];
      for (const row of rows) {
        const [pat, dec] = row.querySelectorAll("input, select");
        if (pat.value.trim()) out.push({ pattern: pat.value.trim(), decision: dec.value || "" });
      }
      return out;
    },
  };
}

/* ---------- ⛨ 权限策略 ---------- */

async function loadSettingsPerm() {
  const body = $("#settings-body");
  body.textContent = "";
  const tab = state.settingsTab;
  let resp, models = [];
  try {
    [resp, models] = await Promise.all([
      api("/api/permissions").then((r) => r.json()),
      api("/api/models").then((r) => r.json()).then((j) => j.models || []),
    ]);
  } catch (e) {
    body.textContent = "加载权限策略失败：" + String(e.message || e);
    return;
  }
  if (state.settingsTab !== tab) return;
  const pol = resp.policy || {};
  const dp = pol.defaultPolicy || {};
  const sp = pol.special || {};
  const ad = pol.autoDecide || {};
  const llm = pol.llmApproval || {};

  body.appendChild(Object.assign(document.createElement("p"), {
    className: "dsec-note",
    textContent: "策略文件：" + (resp.path || "") + " · 保存即校验、备份并热重载，无需重启",
  }));

  // 默认策略 + 目录守卫
  const sec1 = dsec("默认策略", "无规则命中时的兜底决策；留空视为 allow");
  const selTools = permSelect(dp.tools);
  const selBash = permSelect(dp.bash);
  const selExt = permSelect(sp.externalDirectory);
  sec1.append(
    drow("工具", "所有工具调用的兜底", selTools),
    drow("bash", "所有 bash 命令的兜底", selBash),
    drow("工作目录之外的读写", "read/write/edit 目标在会话目录之外时", selExt),
  );
  body.appendChild(sec1);

  // 无人值守
  const sec2 = dsec("无人值守自动决策", "无人在场时按风险等级自动决策：allow / deny / llm");
  const adKeys = [
    ["readOnly", "只读工具"],
    ["projectLocal", "项目内写"],
    ["system", "系统操作"],
    ["network", "网络操作"],
    ["unknown", "未知风险"],
  ];
  const adSels = {};
  for (const [k, label] of adKeys) {
    adSels[k] = permSelect(ad[k], autoDecisionOpts);
    sec2.appendChild(drow(label, null, adSels[k]));
  }
  const llmSw = dswitch(llm.enabled);
  sec2.appendChild(drow("LLM 审批", "无人值守时用 LLM 审批 ask 决策", llmSw.el));
  const llmModelSel = document.createElement("select");
  llmModelSel.append(new Option("会话模型（默认）", ""));
  let llmCurInList = llm.model === "" || llm.model == null;
  for (const m of models) {
    const label = m.name && m.name !== m.id ? m.name + " · " + m.spec : m.spec;
    const o = new Option(label, m.spec);
    if (llm.model === m.spec) { o.selected = true; llmCurInList = true; }
    llmModelSel.append(o);
  }
  if (!llmCurInList && llm.model) llmModelSel.append(new Option(llm.model, llm.model, true, true));
  sec2.appendChild(drow("审批模型", "无人值守 LLM 审批所用模型", llmModelSel));
  const humanOnly = document.createElement("textarea");
  humanOnly.className = "dtextarea";
  humanOnly.placeholder = "人工必问的命令模式，一行一个（如 rm -rf*）";
  humanOnly.value = (pol.humanOnly || []).join("\n");
  sec2.appendChild(drow("人工必问命令", "命中即拒绝 LLM 审批", humanOnly));
  body.appendChild(sec2);

  // 规则
  const sec3 = dsec("工具规则", "键为工具名或 tool:path/glob，值为决策；命中即生效");
  const toolRows = permRuleRows(
    Object.entries(pol.tools || {}).map(([pattern, decision]) => ({ pattern, decision })), false);
  sec3.appendChild(toolRows.el);
  body.appendChild(sec3);

  const sec4 = dsec("bash 规则", "命令 glob → 决策，长模式优先、短模式兜底");
  const bashRows = permRuleRows(pol.bash || [], true);
  sec4.appendChild(bashRows.el);
  body.appendChild(sec4);

  // 保存
  const save = document.createElement("button");
  save.className = "primary";
  save.textContent = "保存权限策略";
  save.addEventListener("click", async () => {
    const policy = { defaultPolicy: {}, special: {}, autoDecide: {}, llmApproval: {} };
    if (selTools.value) policy.defaultPolicy.tools = selTools.value;
    if (selBash.value) policy.defaultPolicy.bash = selBash.value;
    if (selExt.value) policy.special.externalDirectory = selExt.value;
    for (const [k] of adKeys) {
      if (adSels[k].value) policy.autoDecide[k] = adSels[k].value;
    }
    if (llmSw.inp.checked) policy.llmApproval.enabled = true;
    if (llmModelSel.value) policy.llmApproval.model = llmModelSel.value;
    const ho = humanOnly.value.split("\n").map((s) => s.trim()).filter(Boolean);
    if (ho.length) policy.humanOnly = ho;
    const tools = {};
    for (const r of toolRows.collect()) tools[r.pattern] = r.decision;
    if (Object.keys(tools).length) policy.tools = tools;
    const bash = bashRows.collect();
    if (bash.length) policy.bash = bash;
    save.disabled = true;
    try {
      const j = await api("/api/permissions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(policy),
      }).then((r) => r.json());
      if (j.ok === false) throw new Error(j.error || "保存失败");
      setStatusText(j.reloaded === false ? "已保存（热重载失败：" + (j.error || "") + "）" : "已保存并热重载权限策略");
    } catch (e) {
      setStatusText("保存失败：" + String(e.message || e));
    }
    save.disabled = false;
  });
  body.appendChild(save);
}

/* ---------- ◈ Mesh 运行开关 + 权限 ---------- */

async function openMeshDesktop() {
  const body = $("#settings-body");
  body.textContent = "";
  const tab = state.settingsTab;
  let running, cfgResp;
  try {
    [running, cfgResp] = await Promise.all([
      api("/api/mesh/running").then((r) => r.json()),
      api("/api/mesh/config").then((r) => r.json()),
    ]);
  } catch (e) {
    body.textContent = "加载 mesh 配置失败：" + String(e.message || e);
    return;
  }
  if (state.settingsTab !== tab) return;
  const cfg = cfgResp.config || {};

  // 运行开关
  const sec1 = dsec("运行开关", "mesh.json：" + (cfgResp.path || "") + " · 须先有 token（`doge mesh new` 生成）；停用后已注入工具保留到会话重建");
  const sw = dswitch(running.running);
  sw.inp.addEventListener("change", async () => {
    const on = sw.inp.checked;
    sw.inp.disabled = true;
    try {
      const r = await api(on ? "/api/mesh/start" : "/api/mesh/stop", { method: "POST" });
      if (!r.ok) throw new Error(((await r.json()) || {}).error || (on ? "启动失败" : "停止失败"));
      setStatusText(on ? "mesh 节点已启动" : "mesh 节点已停止");
    } catch (e) {
      sw.inp.checked = running.running; // 启停失败回弹到真实状态
      setStatusText("✗ " + String((e && e.message) || e));
    }
    sw.inp.disabled = false;
    openMeshDesktop(); // 重渲染反映运行态（节点卡/任务列表）
  });
  const togRow = drow(running.running ? "运行中" : "已停用", running.running ? "点击开关停用节点" : "点击开关启动节点（加入 mesh.json 网络）", sw.el);
  sec1.appendChild(togRow);
  body.appendChild(sec1);

  // 权限配置
  const sec2 = dsec("Mesh 权限", "远端节点在本节点上能做什么；保存后运行中的节点自动重启生效（token 留空保留现值）");
  const levelSel = permSelect(cfg.remoteTaskLevel, [
    ["", "缺省"],
    ["read", "read 只读"],
    ["write", "write 可写"],
    ["full", "full 完全"],
  ]);
  const swTasks = dswitch(cfg.allowRemoteTasks);
  const swTools = dswitch(cfg.allowRemoteTools);
  const swFiles = dswitch(cfg.allowRemoteFiles);
  const swPriv = dswitch(cfg.privateNetwork);
  const swRelay = dswitch(cfg.relay);
  const numTasks = document.createElement("input");
  numTasks.type = "number";
  numTasks.value = cfg.maxConcurrentTasks || 0;
  const numFile = document.createElement("input");
  numFile.type = "number";
  numFile.value = cfg.fileMaxBytes || 0;
  const numPort = document.createElement("input");
  numPort.type = "number";
  numPort.value = cfg.listenPort || 0;
  sec2.append(
    drow("接受远端任务派发", "allowRemoteTasks", swTasks.el),
    drow("远端任务级别", "remoteTaskLevel", levelSel),
    drow("允许 peer 直调本节点工具", "allowRemoteTools（mesh_forward 等）", swTools.el),
    drow("允许 trusted peer 收发文件", "allowRemoteFiles（进上传暂存区）", swFiles.el),
    drow("最大并发任务", "maxConcurrentTasks", numTasks),
    drow("单文件上限（字节）", "fileMaxBytes（0 = 256MiB 默认）", numFile),
    drow("固定监听端口", "listenPort（0 = 随机；种子节点建议固定）", numPort),
    drow("私有网络强制 PSK", "privateNetwork", swPriv.el),
    drow("充当中继节点", "relay（公网/固定 IP 的种子节点）", swRelay.el),
  );
  const tp = document.createElement("textarea");
  tp.className = "dtextarea";
  tp.placeholder = "trustedPeers：受信 peerID 一行一个（空 = 同 token 全部节点受信）";
  tp.value = (cfg.trustedPeers || []).join("\n");
  sec2.appendChild(drow("受信节点白名单", "trustedPeers", tp));
  // Token 输入：恒发空串（ConfigPut 空则保留磁盘现值）——回传快照会把手改的
  // mesh.json 覆盖回旧值；填新 token 即换网络，保存后节点重启生效
  const tokIn = document.createElement("input");
  tokIn.type = "password";
  tokIn.placeholder = "留空保留现值（粘贴新 token 即换网络）";
  tokIn.autocomplete = "off";
  sec2.appendChild(drow("网络 Token", "mesh.json 里的 token；所有节点同 token 才能互通", tokIn));
  body.appendChild(sec2);

  // 保存
  const save = document.createElement("button");
  save.className = "primary";
  save.textContent = "保存 Mesh 权限";
  save.addEventListener("click", async () => {
    const out = {
      token: tokIn.value.trim(),
      name: cfg.name || "",
      roles: cfg.roles || [],
      bootstrapPeers: cfg.bootstrapPeers || [],
      cwds: cfg.cwds || {},
      forwards: cfg.forwards || [],
      allowRemoteTasks: swTasks.inp.checked,
      remoteTaskLevel: levelSel.value,
      allowRemoteTools: swTools.inp.checked,
      allowRemoteFiles: swFiles.inp.checked,
      privateNetwork: swPriv.inp.checked,
      relay: swRelay.inp.checked,
      maxConcurrentTasks: Number(numTasks.value) || 0,
      fileMaxBytes: Number(numFile.value) || 0,
      listenPort: Number(numPort.value) || 0,
      trustedPeers: tp.value.split("\n").map((s) => s.trim()).filter(Boolean),
    };
    save.disabled = true;
    try {
      const j = await api("/api/mesh/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(out),
      }).then((r) => r.json());
      if (j.ok === false) throw new Error(j.error || "保存失败");
      setStatusText(j.restarted ? "已保存并重启 mesh 节点（权限即时生效）" : "已保存（节点未运行，启动时生效）");
      setTimeout(() => {}, 0);
    } catch (e) {
      setStatusText("保存失败：" + String(e.message || e));
    }
    save.disabled = false;
  });
  body.appendChild(save);

  // 运行时信息（沿用 webui 渲染；未运行时静默跳过）。渲染进独立容器：
  // renderMeshModal 会清空容器，直接传 body 会把上面的开关/权限表单一起抹掉，
  // 节点运行时就没法停节点、改配置了。
  try {
    const r = await api("/api/mesh");
    if (state.settingsTab !== tab) return;
    if (r.ok) {
      const m = await r.json();
      peerNames.clear();
      for (const p of m.peers || []) {
        if (p && p.id && p.profile && p.profile.name) peerNames.set(p.id, p.profile.name);
      }
      const runtime = document.createElement("div");
      const rtHead = document.createElement("div");
      rtHead.className = "mesh-section-title";
      rtHead.textContent = "运行时信息";
      runtime.appendChild(rtHead);
      body.appendChild(runtime);
      renderMeshModal(runtime, m);
    }
  } catch { /* 未运行：不追加运行时信息 */ }
}
