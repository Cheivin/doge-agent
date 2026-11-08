/* doge Web 控制台传输适配层：fetch（携带窗口身份）+ WebSocket（断线指数退避
   重连）。与 desktop/frontend/platform-desktop.js 导出同一 interface——共享
   UI 内核 app.js 经 /platform.js 引用本文件（internal/web 静态服务把
   /platform.js 改写到此处）。 */

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

// 当前连接（answerPermission 判断在线用）
let sock = null;

// api performs a fetch carrying this window's client id so the server routes
// to this window's own session.
export function api(path, opts) {
  opts = opts || {};
  opts.credentials = opts.credentials || "same-origin";
  opts.headers = Object.assign({ "X-Doge-Client": clientId }, opts.headers || {});
  return fetch(path, opts);
}

// connect 建立 WS 事件流：onFrame 收已解析的 console 帧，onConnChange 报告
// 连接建立/断开；断线按指数退避自动重连（15s 封顶），重连后重挂同一组回调。
export function connect(onFrame, onConnChange) {
  let retry = 0;
  const dial = () => {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    sock = new WebSocket(proto + "//" + location.host + "/ws?client=" + encodeURIComponent(clientId));
    sock.onopen = () => {
      retry = 0;
      onConnChange(true);
    };
    sock.onmessage = (e) => {
      let frame;
      try { frame = JSON.parse(e.data); } catch (_) { return; }
      onFrame(frame);
    };
    sock.onclose = () => {
      onConnChange(false);
      // 未应答的权限/提问跨断线保留：agent 还在阻塞等结果，弹窗留着，
      // 重连后重挂；应答走 answerPermission 的 REST 兜底，过期则报 404/409。
      const delay = Math.min(1000 * 2 ** retry, 15000);
      retry++;
      setTimeout(dial, delay);
    };
  };
  dial();
}

// answerPermission 应答权限询问：socket 在线优先走 WS 帧（与 REST 等效），
// 断线落 REST 兜底；404/409 = 询问已过期（expired 标记供 UI 区分文案）。
export async function answerPermission(id, resp) {
  if (sock && sock.readyState === WebSocket.OPEN) {
    sock.send(JSON.stringify({ type: "permission_response", id, response: resp }));
    return { ok: true };
  }
  try {
    const r = await api("/api/permission/answer", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(Object.assign({ id }, resp)),
    });
    if (r.ok) return { ok: true };
    return { ok: false, expired: r.status === 404 || r.status === 409 };
  } catch (e) {
    return { ok: false, error: String((e && e.message) || e) };
  }
}

// confirm/prompt 走浏览器原生弹窗（对应桌面端 desktopModal 替身）
export const confirmDialog = (msg) => Promise.resolve(window.confirm(msg));
export const promptDialog = (msg, def) => Promise.resolve(window.prompt(msg, def == null ? "" : def));

// caps 能力开关：web 端开放登录/多窗口仲裁；paging/permPolicy/meshControl
// 的服务端端点已就绪，UI 侧待样式与信息架构对齐后再逐项打开（保持既有行为）
export const caps = {
  auth: true,         // 登录/登出/接管 UI
  multiWindow: true,  // 多窗口会话写控制仲裁
  paging: false,      // 转录分片加载（GET /api/session/messages）
  permPolicy: false,  // ⛨ 权限策略 tab（GET/PUT /api/permissions）
  meshControl: false, // mesh 运行开关/权限配置（/api/mesh/config|running|start|stop）
  desktop: false,     // 桌面壳专属 UI（.desktop 类、dogeMenu 菜单钩子）
};
