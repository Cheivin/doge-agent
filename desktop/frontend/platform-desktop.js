/* doge 桌面端传输适配层 + 桌面专属 UI 注入（overlay）。与共享内核
   internal/webui/static/platform-web.js 导出同一 interface——app.js 经
   /platform.js 引用，desktop/main.go 的 union 资产服务把 /platform.js
   解析到本文件。数据面走 typed Service 绑定（desktopApi 路由表），事件流走
   wails Events + Snapshot，弹窗用 desktopModal 替身（WKWebView 未实现
   JS 弹窗委托，原生 confirm 恒返回 false）。 */

import { Call, Events } from "/wails/runtime.js";

/* ---------- 启动诊断：module 加载失败/运行时报错上报 Go 日志（js:err） ----------
   本文件先于 app.js 求值：即使 app.js 加载失败，3s 兜底检查也能上报黑屏原因。 */
window.__booted = false;
function __diag(msg) {
  try {
    const p = Events.Emit("js:err", msg);
    if (p && p.catch) p.catch(() => {});
  } catch (_) {}
  const d = document.createElement("div");
  d.style.cssText = "position:fixed;inset:auto 0 0 0;z-index:99999;background:#7f1d1d;color:#fff;" +
    "font:12px/1.5 ui-monospace,Menlo,monospace;padding:8px 12px;white-space:pre-wrap;";
  d.textContent = msg;
  (document.body || document.documentElement).appendChild(d);
}
window.addEventListener("error", (e) => {
  __diag("JS ERROR: " + e.message + " @" + String(e.filename || "").split("/").pop() + ":" + (e.lineno || 0));
});
window.addEventListener("unhandledrejection", (e) => {
  __diag("JS REJECTION: " + String((e.reason && e.reason.message) || e.reason));
});
setTimeout(() => {
  if (!window.__booted) __diag("app.js module 未启动（import 失败或 MIME 被拒？）");
}, 3000);

/* ---------- 桌面壳外观：标题 / 专属样式 / .desktop 类 / Go 菜单钩子 ---------- */

document.title = "doge";
// desktop.css 插到共享内核样式之前：与旧桌面单文件里的层叠先后关系一致
// （.drow-ctrl select 让位于共享的 select 统一外观规则）
const coreLink = document.querySelector('link[href="/style.css"]');
if (coreLink && !document.querySelector('link[href="/desktop.css"]')) {
  const link = document.createElement("link");
  link.rel = "stylesheet";
  link.href = "/desktop.css";
  document.head.insertBefore(link, coreLink);
}
document.body.classList.add("desktop");

// Go 菜单/托盘经 ExecJS 调用这里，复用页面自身的事件绑定
window.dogeMenu = {
  newTask: () => document.querySelector("#sb-new").click(),
  abort: () => document.querySelector("#abort-btn").click(),
  openSettings: () => document.querySelector("#settings-btn").click(),
  reloadConfig: () => document.querySelector("#nav-reload").click(),
};

// api 桌面端数据层：fetch 调用映射为 typed Service 绑定（响应形状与
// Web API 契约一致），FormData 上传转 base64 服务调用，事件流走 wails Events。
export function api(path, opts) {
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
      if (p === "/api/config") return resOK(await S("ConfigService.Overview"));
      if (p === "/api/config/settings") return resOK(await S("ConfigService.SettingsGet"));
      if (p === "/api/config/auth") return resOK(await S("ConfigService.AuthGet"));
      if (p === "/api/config/providers") return resOK(await S("ConfigService.Providers"));
      if (p === "/api/automations") return resOK(await S("AutomationsService.List"));
      if (p.startsWith("/api/automations/")) {
        const rest = p.slice("/api/automations/".length).split("/");
        const id = decodeURIComponent(rest[0]);
        if (rest.length === 1) return resOK(await S("AutomationsService.Get", id, 5));
        if (rest[1] === "runs") return resOK(await S("AutomationsService.Runs", id, Number(u.searchParams.get("limit")) || 10));
      }
      if (p === "/api/fs/ls") return resOK(await S("ConfigService.FsList", u.searchParams.get("path") || ""));
      if (p === "/api/auth/whoami") return resOK({ ok: true, method: "desktop" });
    } else if (method === "DELETE") {
      if (p.startsWith("/api/mcp/servers/")) { await S("McpService.ServerRemove", decodeURIComponent(p.slice("/api/mcp/servers/".length))); return resOK({ ok: true }); }
      if (p.startsWith("/api/skills/")) return resOK(await S("SkillsService.Delete", decodeURIComponent(p.slice("/api/skills/".length))));
      if (p.startsWith("/api/config/auth/")) {
        const prov = decodeURIComponent(p.slice("/api/config/auth/".length));
        return resOK(await S("ConfigService.AuthDelete", prov));
      }
      if (p.startsWith("/api/config/models/")) {
        const segs = p.slice("/api/config/models/".length).split("/").map(decodeURIComponent);
        if (segs.length === 2) return resOK(await S("ConfigService.ModelsDelete", segs[0], segs[1]));
      }
      if (p.startsWith("/api/mesh/forwards/")) { await S("MeshService.ForwardRemove", decodeURIComponent(p.slice("/api/mesh/forwards/".length))); return resOK({ ok: true }); }
      if (p.startsWith("/api/automations/")) {
        const rest = p.slice("/api/automations/".length).split("/");
        const id = decodeURIComponent(rest[0]);
        if (rest.length === 1) { await S("AutomationsService.Delete", id); return resOK({ ok: true }); }
      }
    } else {
      if (p === "/api/auth/login" || p === "/api/auth/logout") return resOK({ ok: true });
      if (p === "/api/prompt") { await S("SessionService.Prompt", body.text || "", body.attachments || []); return resOK({ ok: true }); }
      if (p === "/api/abort") { await S("SessionService.Abort"); return resOK({ ok: true }); }
      if (p === "/api/compact") { await S("SessionService.Compact"); return resOK({ ok: true }); }
      if (p === "/api/reload") return resOK(await S("ConfigService.Reload"));
      if (p === "/api/config/models") return resOK(await S("ConfigService.ModelsUpsert", body, u.searchParams.get("previous") || ""));
      if (p === "/api/config/auth") return resOK(await S("ConfigService.AuthSet", body.provider || "", body.key || ""));
      if (p === "/api/automations") return resOK(await S("AutomationsService.Create", body));
      if (p.startsWith("/api/automations/") && method === "PATCH") {
        const id = decodeURIComponent(p.slice("/api/automations/".length));
        return resOK(await S("AutomationsService.Update", id, body));
      }
      if (p === "/api/config/settings") {
        await S("ConfigService.SettingsPut", body);
        return resOK({ ok: true, restartRequired: true });
      }
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
      if (p.startsWith("/api/automations/")) {
        const rest = p.slice("/api/automations/".length).split("/");
        const id = decodeURIComponent(rest[0]);
        if (rest.length === 2 && rest[1] === "run") return resOK(await S("AutomationsService.Run", id));
        if (rest.length === 2 && rest[1] === "enable") return resOK(await S("AutomationsService.SetEnabled", id, true));
        if (rest.length === 2 && rest[1] === "disable") return resOK(await S("AutomationsService.SetEnabled", id, false));
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

// connect 建立 console 事件流：桌面端无 WebSocket——事件经 wails Events 注入，
// "连接"即拉快照续接视图；onFrame 收已解析帧（与 web 端真 WebSocket 等效）。
export function connect(onFrame, onConnChange) {
  onConnChange(true);
  (async () => {
    try {
      const snap = await Call.ByName("doge/desktop/services.SessionService.Snapshot");
      onFrame(snap);
    } catch (_) { /* 快照失败：事件流随后补齐 */ }
  })();
  Events.On("console", (ev) => {
    try {
      onFrame(ev.data);
    } catch (e) {
      try { Events.Emit("js:err", "handleFrame: " + String((e && e.stack) || e)); } catch (_) {}
    }
  });
}

// answerPermission 应答权限询问（映射到 SessionService.PermissionAnswer）。
// 发后即忘：与旧 fake socket 行为一致，失败静默（询问已过期/已答由 agent
// 侧 5 分钟超时兜底）。
export function answerPermission(id, resp) {
  Call.ByName(
    "doge/desktop/services.SessionService.PermissionAnswer",
    id, !!resp.allowOnce, !!resp.allowAlways, !!resp.deny
  ).catch(() => {});
  return Promise.resolve({ ok: true });
}

/* ---------- confirm/prompt 替身（WKWebView 未实现 JS 弹窗委托，
   原生 confirm 恒返回 false 导致所有删除/确认静默失效） ---------- */
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
      // 忽略 IME 选词回车（对齐主输入框的 isComposing / keyCode 229 守卫）
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) { e.preventDefault(); ok.click(); }
      if (e.key === "Escape") done(opts.input ? null : false);
    });
    (inp || ok).focus();
  });
}

export const confirmDialog = (msg) => desktopModal({ title: "确认", message: msg });
export const promptDialog = (msg, def) => desktopModal({ title: "输入", message: msg, input: true, defaultValue: def });

// caps 能力开关：桌面端无登录/多窗口仲裁（本机进程单窗口），其余桌面能力全开
export const caps = {
  auth: false,        // 无登录页（无 HTTP 面、无 token）
  multiWindow: false, // 单窗口后端，无写控制仲裁
  paging: true,       // 转录分片加载（SessionService.MessagesBefore）
  permPolicy: true,   // ⛨ 权限策略 tab（ConfigService.PermissionsGet/Put）
  meshControl: true,  // mesh 运行开关/权限配置（MeshService.ConfigGet/Put/...）
  desktop: true,      // 桌面壳专属 UI（.desktop 类、dogeMenu 菜单钩子）
};
