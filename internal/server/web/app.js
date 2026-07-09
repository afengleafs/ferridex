const $ = (s) => document.querySelector(s);
const $$ = (s) => document.querySelectorAll(s);

/* ===== 主题切换 ===== */
function initTheme() {
  const root = document.documentElement;
  const btn = $("#theme-toggle");
  const sync = () => {
    const dark = root.getAttribute("data-theme") === "dark";
    btn.setAttribute("aria-pressed", String(dark));
  };
  sync();
  btn.addEventListener("click", () => {
    const dark = root.getAttribute("data-theme") !== "dark";
    root.setAttribute("data-theme", dark ? "dark" : "light");
    try { localStorage.setItem("theme", dark ? "dark" : "light"); } catch (e) {}
    sync();
  });
}

/* ===== 全局 toast ===== */
function toast(msg, kind) {
  const wrap = $("#toast");
  if (!wrap) return;
  const el = document.createElement("div");
  el.className = "toast-item" + (kind === "err" ? " err" : "");
  el.textContent = msg;
  wrap.appendChild(el);
  requestAnimationFrame(() => el.classList.add("show"));
  setTimeout(() => {
    el.classList.remove("show");
    setTimeout(() => el.remove(), 220);
  }, 1800);
}

/* ===== 剪贴板 ===== */
async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) throw new Error("copy failed");
}

/* ===== 复制按钮(按钮文案 + toast 双反馈) ===== */
function startCopyButtons() {
  $$(".copy-btn").forEach((button) => {
    button.addEventListener("click", async (e) => {
      e.stopPropagation();
      const target = document.getElementById(button.dataset.copy);
      if (!target) return;
      const original = button.textContent;
      try {
        await copyText(target.textContent);
        button.textContent = "已复制";
        button.classList.add("copied");
        toast("已复制到剪贴板");
      } catch {
        button.textContent = "复制失败";
        toast("复制失败", "err");
      }
      setTimeout(() => {
        button.textContent = original;
        button.classList.remove("copied");
      }, 1600);
    });
  });
}

/* ===== 折叠(默认展开,仍可收起) ===== */
function startAccordions() {
  $$(".accordion-head").forEach((head) => {
    head.addEventListener("click", () => {
      head.closest(".config-block").classList.toggle("open");
    });
  });
}

/* ===== netinfo / 远程客户端配置 ===== */
let netinfo = { lan_enabled: false, lan_ips: [], lan_key: "" };

function currentPort() {
  return location.port || (location.protocol === "https:" ? "443" : "80");
}
function proxyHost() {
  if (netinfo.lan_enabled && netinfo.lan_ips && netinfo.lan_ips.length) {
    return netinfo.lan_ips[0];
  }
  return "127.0.0.1";
}
function proxyBaseURL() {
  return `http://${proxyHost()}:${currentPort()}`;
}
// Cursor CLI only routes agent traffic through a custom endpoint when the host
// is "localhost" (127.0.0.1 bypasses the proxy and hits agentn.* directly).
function cursorBaseURL(port) {
  const host = proxyHost();
  const cursorHost = host === "127.0.0.1" ? "localhost" : host;
  return `http://${cursorHost}:${port || currentPort()}`;
}
function authToken() {
  return netinfo.lan_key || "dummy";
}

function loadClientConfigs() {
  const baseURL = proxyBaseURL();
  const token = authToken();
  const claude = {
    env: {
      ANTHROPIC_BASE_URL: baseURL,
      ANTHROPIC_AUTH_TOKEN: token,
      ANTHROPIC_DEFAULT_OPUS_MODEL: "claude-opus-4-8",
      ANTHROPIC_DEFAULT_SONNET_MODEL: "claude-sonnet-4-5-20250929",
      ANTHROPIC_DEFAULT_HAIKU_MODEL: "claude-haiku-4-5-20251001",
    },
    permissions: { defaultMode: "bypassPermissions" },
    skipDangerousModePermissionPrompt: true,
    model: "opus",
  };
  const codex = `model_provider = "localproxy"
model = "gpt-5.5"
disable_response_storage = true
model_reasoning_effort = "low"
plan_mode_reasoning_effort = "xhigh"
model_reasoning_summary = "none"
model_context_window = 1050000
model_auto_compact_token_limit = 945000
approval_policy = "never"
sandbox_mode = "danger-full-access"
suppress_unstable_features_warning = true
approvals_reviewer = "user"

[model_providers.localproxy]
name = "Local Codex Proxy"
base_url = "${baseURL}/v1"
wire_api = "responses"
env_key = "LOCAL_PROXY_KEY"
requires_openai_auth = false`;

  // 一键启动命令:只内联「路由到 ferridex」必须的部分,不钉死模型/权限/推理档,
  // 启动后就是登录后的默认模型界面(和 Cursor 的 `agent -e ...` 一致)。
  const claudeLaunch = `ANTHROPIC_BASE_URL=${baseURL} ANTHROPIC_AUTH_TOKEN=${token} claude`;
  const codexLaunch = [
    `LOCAL_PROXY_KEY=${token} codex`,
    `-c 'model_provider="localproxy"'`,
    `-c 'model_providers.localproxy.base_url="${baseURL}/v1"'`,
    `-c 'model_providers.localproxy.wire_api="responses"'`,
    `-c 'model_providers.localproxy.env_key="LOCAL_PROXY_KEY"'`,
    `-c 'model_providers.localproxy.requires_openai_auth=false'`,
  ].join(" \\\n  ");

  $("#proxy-url").textContent = baseURL;
  $("#claude-launch").textContent = claudeLaunch;
  $("#claude-config").textContent = JSON.stringify(claude, null, 2);
  $("#codex-launch").textContent = codexLaunch;
  $("#codex-config").textContent = codex;
  $("#codex-env").textContent = `export LOCAL_PROXY_KEY=${token}`;
  $("#cursor-config").textContent = `agent -e ${cursorBaseURL()} --auth-token ${token}`;
  // Grok 没有 -e/--auth-token,用环境变量指向代理。**不要用 XAI_API_KEY**:它会切到
  // BYOK 模式直连 api.x.ai,绕过代理。改用 auth_provider_command 提供占位 token(grok
  // 会当成会话令牌发给代理,ferridex 再替换成本机订阅 token)。独立 GROK_HOME 避免污染
  // 本机真实的 ~/.grok 登录。LAN 下占位 token 即密钥。
  // 先清隔离 home 的缓存,保证 grok 每次都用当前占位 token(dummy/密钥),
  // 避免切换 SSH↔LAN 或换密钥后 grok 复用旧缓存 token 导致 401。
  $("#grok-launch").textContent =
    `rm -f $HOME/.grok-ferridex/auth.json; export GROK_HOME=$HOME/.grok-ferridex GROK_CLI_CHAT_PROXY_BASE_URL=${baseURL}/grok/v1 GROK_AUTH_PROVIDER_COMMAND='echo ${token}' GROK_AUTH_TOKEN_TTL=3600; grok`;
}

async function loadNetinfo() {
  try {
    const r = await fetch("/api/netinfo");
    netinfo = await r.json();
  } catch {}
  loadClientConfigs();
  renderLanInfo();
}

function renderLanInfo() {
  const el = $("#lan-info");
  if (!el) return;
  if (netinfo.lan_enabled) {
    const port = currentPort();
    const ips = netinfo.lan_ips || [];
    const urls = ips.length
      ? ips.map((ip) => `http://${ip}:${port}`).join("　")
      : "未检测到局域网 IPv4 地址";
    el.innerHTML =
      `<div class="ep"><span class="name">局域网</span><code>${urls}</code></div>` +
      `<div class="ep"><span class="name">密钥</span><code>${netinfo.lan_key}</code></div>` +
      `<div class="lan-note">同一 Wi-Fi 下的另一台电脑使用上面地址；下方配置已包含密钥，可直接复制粘贴。若对方连不上，多半是公司 Wi-Fi 的客户端隔离，请改用 SSH 反向隧道。</div>`;
  } else {
    el.innerHTML =
      `<div class="lan-note">本机模式：仅 127.0.0.1 可用。要让同一 Wi-Fi 的另一台电脑直连，请用 <code>ferridex serve -lan</code> 启动。</div>`;
  }
}

/* ===== 状态卡片 + 整体健康条 ===== */
function dot(on) {
  return `<span class="dot ${on ? "on" : "off"}"></span>`;
}

// 从 Claude detail 里提取「恢复时间 HH:MM:SS」并返回绝对时间戳(ms);无法解析返回 null。
function parseClaudeReset(detail) {
  if (!detail) return null;
  const m = detail.match(/(\d{1,2}):(\d{2}):(\d{2})/);
  if (!m) return null;
  const now = new Date();
  const reset = new Date(now);
  reset.setHours(+m[1], +m[2], +m[3], 0);
  if (reset.getTime() <= now.getTime()) {
    // 跨天:恢复时间已过但 detail 尚未刷新,加 24h 显示为「即将到期」。
    reset.setDate(reset.getDate() + 1);
  }
  return reset.getTime();
}

// 每个卡片的 detail 可能有倒计时,这里记录 {cardId, resetMs} 供每秒刷新。
let countdowns = [];
let countdownTimer = null;

function fmtCountdown(ms) {
  if (ms <= 0) return null;
  const s = Math.round(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h > 0) return `约 ${h} 时 ${m} 分后恢复`;
  if (m > 0) return `约 ${m} 分 ${sec} 秒后恢复`;
  return `约 ${sec} 秒后恢复`;
}

function tickCountdown() {
  const now = Date.now();
  countdowns.forEach((c) => {
    const el = document.getElementById(c.id);
    if (!el) return;
    const span = el.querySelector(".health-countdown");
    const txt = fmtCountdown(c.resetMs - now);
    if (span) {
      span.textContent = txt || "";
    }
  });
}

function setHealth(state, text, countdownMs) {
  const bar = $("#healthbar");
  bar.classList.remove("ok", "warn", "bad");
  if (state) bar.classList.add(state);
  $("#health-dot").className = "dot hero-dot";
  if (state === "ok") $("#health-dot").classList.add("on");
  else if (state === "bad") $("#health-dot").classList.add("off");
  let extra = "";
  if (countdownMs) {
    extra = ` <span class="health-countdown"> · ${fmtCountdown(countdownMs - Date.now()) || ""}</span>`;
  }
  $("#health-text").innerHTML = text + extra;
}

let lastProvidersOK = false;
let lastDetail = {};

// 记住被手动收起的 provider 卡片(按 name),让每 5 秒的重渲染保持折叠状态。
const collapsedCards = new Set();

/* ===== 订阅用量进度条 ===== */
function usageLevel(frac) {
  if (frac >= 0.9) return "bad";
  if (frac >= 0.7) return "warn";
  return "ok";
}

function fmtResetIn(resetsAt) {
  if (!resetsAt) return "";
  const s = Math.round(resetsAt * 1000 - Date.now()) / 1000;
  if (s <= 0) return "已重置";
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h >= 24) return `${Math.floor(h / 24)} 天后重置`;
  if (h > 0) return `${h} 时 ${m} 分后重置`;
  if (m > 0) return `${m} 分后重置`;
  return `${Math.round(s)} 秒后重置`;
}

function usageBar(u) {
  const frac = Math.max(0, Math.min(1, u.utilization || 0));
  const pct = Math.round(frac * 100);
  const reset = fmtResetIn(u.resets_at);
  return `
    <div class="usage-row">
      <div class="usage-top">
        <span class="usage-label">${u.label || ""}</span>
        <span class="usage-pct">${pct}%</span>
      </div>
      <div class="bar"><div class="bar-fill ${usageLevel(frac)}" style="width:${pct}%"></div></div>
      ${reset ? `<div class="usage-reset">${reset}</div>` : ""}
    </div>`;
}

async function loadStatus() {
  try {
    const r = await fetch("/api/status");
    const d = await r.json();
    const providers = d.providers || [];
    lastProvidersOK = true;

    countdowns = [];
    let claudeResetMs = null;

    $("#cards").innerHTML =
      providers
        .map((p) => {
          if (p.name === "claude" && p.detail) {
            const rm = parseClaudeReset(p.detail);
            if (rm) claudeResetMs = rm;
          }
          const cardId = `card-${p.name}`;
          const claudeCounting = p.name === "claude" && claudeResetMs;
          if (claudeCounting) {
            countdowns.push({ id: cardId, resetMs: claudeResetMs });
          }
          const collapsedClass = collapsedCards.has(p.name) ? " collapsed" : "";
          return `
        <div class="card ${p.logged_in ? "" : "off"}${collapsedClass}" id="${cardId}">
          <div class="card-head">
            <span class="caret" aria-hidden="true">▸</span>
            <div class="card-head-main">
              <h3>${p.title || p.name}</h3>
              <span class="who">${p.name}</span>
            </div>
            <span class="card-head-status">
              ${dot(p.logged_in)}
              <strong>${p.logged_in ? "已登录" : "未登录"}</strong>
            </span>
          </div>
          <div class="card-body">
            ${p.account ? `<div class="acct">${p.account}</div>` : ""}
            ${claudeCounting ? `<div class="detail"><span class="health-countdown"></span></div>` : (p.detail ? `<div class="detail">${p.detail}</div>` : "")}
            ${p.usage && p.usage.length ? `<div class="usage">${p.usage.map(usageBar).join("")}</div>` : ""}
            <div class="meta">${(p.models || []).map((m) => `<span class="tag">${m}</span>`).join("")}</div>
          </div>
        </div>`;
        })
        .join("") || `<div class="empty">无 provider</div>`;

    $("#endpoints").innerHTML = providers
      .map((p) => {
        if (p.name === "cursor") {
          return `<div class="ep"><span class="name">${p.name}</span><code>agent -e ${cursorBaseURL()} --auth-token ${authToken()}</code></div>`;
        }
        if (p.name === "grok") {
          return `<div class="ep"><span class="name">${p.name}</span><code>GROK_CLI_CHAT_PROXY_BASE_URL=${proxyBaseURL()}/grok/v1 (+ auth_provider_command)</code></div>`;
        }
        return `<div class="ep"><span class="name">${p.name}</span><code>POST ${location.origin}${p.endpoint}</code></div>`;
      })
      .join("");

    // 整体健康
    if (!providers.length) {
      setHealth("warn", "未检测到 provider");
    } else {
      const onCount = providers.filter((p) => p.logged_in).length;
      if (onCount === providers.length) {
        setHealth("ok", `全部就绪 · ${onCount}/${providers.length} 已登录`, claudeResetMs);
      } else {
        setHealth("warn", `部分未登录 · ${onCount}/${providers.length}`, claudeResetMs);
      }
    }

    if (claudeResetMs && !countdownTimer) {
      countdownTimer = setInterval(tickCountdown, 1000);
      tickCountdown();
    }
  } catch (e) {
    lastProvidersOK = false;
    $("#cards").innerHTML = `<div class="empty">无法连接到 ferridex</div>`;
    setHealth("bad", "无法连接到 ferridex");
  }
}

/* ===== 实时日志:筛选 / 暂停 / 清空 / 计数 / 重连 ===== */
let logFilter = "all"; // all | 2 | 4 | 5
let logPaused = false;
let logTotal = 0; // 本次会话累计
let pendingNew = 0; // 暂停期间新增条数
const MAX_LOG = 300;

function statusClass(n) {
  if (n >= 500) return "s5";
  if (n >= 400) return "s4";
  if (n >= 200 && n < 300) return "s2";
  if (n >= 300) return "so";
  return "s4";
}

function logLineHTML(o) {
  return `<span class="t">${o.time}</span>  <span class="m">${o.method}</span> ${o.path} <span class="s ${statusClass(o.status)}">${o.status}</span> <span class="t">${o.dur}</span>`;
}

function updateLogCount(displayed) {
  $("#log-count").textContent = `${logTotal} / ${displayed}`;
}

function applyFilter() {
  let shown = 0;
  $$("#log .logline").forEach((line) => {
    const n = +line.dataset.status;
    const match = logFilter === "all" || Math.floor(n / 100) === +logFilter;
    line.style.display = match ? "" : "none";
    if (match) shown++;
  });
  updateLogCount(shown);
}

function scrollLogBottom() {
  const box = $("#log");
  box.scrollTop = box.scrollHeight;
}

function startLogs() {
  const box = $("#log");
  let backoff = 1000;

  function connect() {
    const es = new EventSource("/api/logs/stream");
    es.onopen = () => {
      $("#livedot").classList.add("on");
      backoff = 1000;
    };
    es.onerror = () => {
      $("#livedot").classList.remove("on");
      es.close();
      // 指数退避重连,上限 10s。
      setTimeout(connect, Math.min(backoff, 10000));
      backoff = Math.min(backoff * 2, 10000);
    };
    es.onmessage = (e) => {
      let o;
      try { o = JSON.parse(e.data); } catch { return; }
      if (o.path === undefined) return;
      const empty = box.querySelector(".empty");
      if (empty) empty.remove();

      logTotal++;
      const line = document.createElement("div");
      line.className = "logline";
      line.dataset.status = o.status;
      line.innerHTML = logLineHTML(o);
      const match = logFilter === "all" || Math.floor(o.status / 100) === +logFilter;
      line.style.display = match ? "" : "none";
      box.appendChild(line);

      while (box.childElementCount > MAX_LOG) box.firstElementChild.remove();

      if (logPaused) {
        pendingNew++;
        $("#new-count").textContent = pendingNew;
        $("#new-logs").hidden = false;
      } else {
        scrollLogBottom();
      }
      applyFilter();
    };
  }
  connect();

  // 筛选段
  $$(".seg-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      $$(".seg-btn").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      logFilter = btn.dataset.filter;
      applyFilter();
    });
  });

  // 暂停滚动
  $("#log-pause").addEventListener("click", () => {
    logPaused = !logPaused;
    const btn = $("#log-pause");
    btn.setAttribute("aria-pressed", String(logPaused));
    btn.textContent = logPaused ? "继续滚动" : "暂停滚动";
    if (!logPaused) {
      pendingNew = 0;
      $("#new-logs").hidden = true;
      scrollLogBottom();
    }
  });

  // 清空
  $("#log-clear").addEventListener("click", () => {
    box.innerHTML = `<div class="empty">已清空显示（后端缓冲不受影响）</div>`;
    applyFilter();
  });

  // 跳到最新
  $("#new-logs").addEventListener("click", () => {
    pendingNew = 0;
    $("#new-logs").hidden = true;
    scrollLogBottom();
  });
}

/* ===== SSH 隧道 ===== */
function fmtUptime(sec) {
  if (!sec || sec < 0) return "0s";
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  if (m >= 60) {
    const h = Math.floor(m / 60);
    return `${h}h ${m % 60}m`;
  }
  return m > 0 ? `${m}m ${s}s` : `${s}s`;
}

async function loadTunnel() {
  try {
    const r = await fetch("/api/tunnel");
    const d = await r.json();
    const st = d.status || {};
    const panel = $("#tunnel-panel");
    const badge = $("#tun-badge");

    panel.classList.toggle("is-running", !!st.running);
    panel.classList.toggle("is-error", !st.running && !!st.last_error);

    badge.className = "badge " + (st.running ? "badge-on" : "badge-off");
    badge.textContent = st.running ? "运行中" : st.last_error ? "异常" : "未运行";

    const remoteEl = $("#tun-remote");
    const keyEl = $("#tun-key");
    if (document.activeElement !== remoteEl && !remoteEl.value && d.remote) remoteEl.value = d.remote;
    if (document.activeElement !== keyEl && !keyEl.value && d.key) keyEl.value = d.key;

    $("#tun-start").disabled = !!st.running;
    $("#tun-stop").disabled = !st.running;

    if (st.running) {
      const parts = [`<span class="pill">pid ${st.pid}</span>`, `<span class="pill">已运行 ${fmtUptime(st.uptime_sec)}</span>`];
      if (st.remote_port) parts.push(`<span class="pill hl">远端端口 ${st.remote_port}</span>`);
      $("#tun-info").innerHTML = parts.join("");
    } else {
      $("#tun-info").textContent = st.last_error ? "" : "未运行";
    }
    $("#tun-err").textContent = st.last_error || "";
  } catch (e) {}
}

async function tunnelAction(path, body) {
  $("#tun-info").textContent = "处理中…";
  try {
    const r = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
    });
    const d = await r.json();
    if (d.error) {
      $("#tun-err").textContent = d.error;
      toast("隧道操作失败", "err");
    } else {
      toast(body && body.remote ? "隧道已启动" : "隧道已停止");
    }
  } catch (e) {
    $("#tun-err").textContent = String(e);
    toast("隧道操作失败", "err");
  }
  loadTunnel();
}

/* ===== 启动 ===== */
initTheme();

$("#tun-start").addEventListener("click", () =>
  tunnelAction("/api/tunnel/start", { remote: $("#tun-remote").value, key: $("#tun-key").value })
);
$("#tun-stop").addEventListener("click", () => tunnelAction("/api/tunnel/stop", {}));

// provider 卡片折叠:委托绑在常驻的 #cards 上,5 秒重渲染后依旧生效。
$("#cards").addEventListener("click", (e) => {
  const head = e.target.closest(".card-head");
  if (!head) return;
  const card = head.closest(".card");
  if (!card) return;
  const name = card.id.slice(5); // 去掉 "card-" 前缀
  if (card.classList.toggle("collapsed")) collapsedCards.add(name);
  else collapsedCards.delete(name);
});

$("#log").innerHTML = `<div class="empty">等待请求…(对 /v1/responses 或 /v1/messages 发一次请求即可看到)</div>`;
updateLogCount(0);

loadNetinfo();
startCopyButtons();
startAccordions();
loadStatus();
loadTunnel();
setInterval(() => {
  loadStatus();
  loadTunnel();
}, 5000);
startLogs();
