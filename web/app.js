const $ = (s) => document.querySelector(s);

function dot(on) {
  return `<span class="dot ${on ? "on" : "off"}"></span>`;
}

function proxyBaseURL() {
  const port = location.port || (location.protocol === "https:" ? "443" : "80");
  return `http://127.0.0.1:${port}`;
}

function loadClientConfigs() {
  const baseURL = proxyBaseURL();
  const claude = {
    env: {
      ANTHROPIC_BASE_URL: baseURL,
      ANTHROPIC_AUTH_TOKEN: "dummy",
      ANTHROPIC_DEFAULT_OPUS_MODEL: "claude-opus-4-8",
      ANTHROPIC_DEFAULT_SONNET_MODEL: "claude-sonnet-4-5-20250929",
      ANTHROPIC_DEFAULT_HAIKU_MODEL: "claude-haiku-4-5-20251001",
    },
    permissions: {
      defaultMode: "bypassPermissions",
    },
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
name = "Local Codex Proxy (via SSH tunnel)"
base_url = "${baseURL}/v1"
wire_api = "responses"
env_key = "LOCAL_PROXY_KEY"
requires_openai_auth = false`;

  $("#claude-config").textContent = JSON.stringify(claude, null, 2);
  $("#codex-config").textContent = codex;
  $("#codex-env").textContent = "export LOCAL_PROXY_KEY=dummy";
}

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

function startCopyButtons() {
  document.querySelectorAll(".copy-btn").forEach((button) => {
    button.addEventListener("click", async () => {
      const target = document.getElementById(button.dataset.copy);
      const original = button.textContent;
      try {
        await copyText(target.textContent);
        button.textContent = "已复制";
        button.classList.add("copied");
      } catch {
        button.textContent = "复制失败";
      }
      setTimeout(() => {
        button.textContent = original;
        button.classList.remove("copied");
      }, 1600);
    });
  });
}

async function loadStatus() {
  try {
    const r = await fetch("/api/status");
    const d = await r.json();
    const providers = d.providers || [];

    $("#cards").innerHTML =
      providers
        .map(
          (p) => `
      <div class="card">
        <h3>${p.title || p.name}</h3>
        <div class="who">${p.name}</div>
        <div class="statusline">
          ${dot(p.logged_in)}
          <strong>${p.logged_in ? "已登录" : "未登录"}</strong>
          ${p.account ? `<span class="acct">· ${p.account}</span>` : ""}
        </div>
        ${p.detail ? `<div class="detail">${p.detail}</div>` : ""}
        <div class="meta">${(p.models || [])
          .map((m) => `<span class="tag">${m}</span>`)
          .join("")}</div>
      </div>`
        )
        .join("") || `<div class="empty">无 provider</div>`;

    $("#endpoints").innerHTML =
      `<div class="ep"><span class="name">base</span><code>${location.origin}</code></div>` +
      providers
        .map(
          (p) =>
            `<div class="ep"><span class="name">${p.name}</span><code>POST ${location.origin}${p.endpoint}</code></div>`
        )
        .join("");
  } catch (e) {
    $("#cards").innerHTML = `<div class="empty">无法连接到 ferridex</div>`;
  }
}

function fmtStatus(n) {
  const cls = n >= 400 ? "err" : "ok";
  return `<span class="${cls}">${n}</span>`;
}

function startLogs() {
  const box = $("#log");
  const es = new EventSource("/api/logs/stream");
  es.onopen = () => $("#livedot").classList.add("on");
  es.onerror = () => $("#livedot").classList.remove("on");
  es.onmessage = (e) => {
    let o;
    try {
      o = JSON.parse(e.data);
    } catch {
      return;
    }
    if (o.path === undefined) return;
    const empty = box.querySelector(".empty");
    if (empty) empty.remove();
    const line = document.createElement("div");
    line.className = "logline";
    line.innerHTML = `<span class="t">${o.time}</span>  <span class="m">${o.method}</span> ${o.path} ${fmtStatus(
      o.status
    )} <span class="t">${o.dur}</span>`;
    box.appendChild(line);
    box.scrollTop = box.scrollHeight;
    while (box.childElementCount > 300) box.firstElementChild.remove();
  };
}

async function loadTunnel() {
  try {
    const r = await fetch("/api/tunnel");
    const d = await r.json();
    const st = d.status || {};
    $("#tun-dot").className = "dot " + (st.running ? "on" : "off");
    const remoteEl = $("#tun-remote");
    const keyEl = $("#tun-key");
    if (document.activeElement !== remoteEl && !remoteEl.value && d.remote) remoteEl.value = d.remote;
    if (document.activeElement !== keyEl && !keyEl.value && d.key) keyEl.value = d.key;
    $("#tun-start").disabled = !!st.running;
    $("#tun-stop").disabled = !st.running;
    $("#tun-info").textContent = st.running
      ? `运行中 · pid ${st.pid} · ${st.uptime_sec}s`
      : "未运行";
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
    $("#tun-err").textContent = d.error || "";
  } catch (e) {
    $("#tun-err").textContent = String(e);
  }
  loadTunnel();
}

$("#tun-start").addEventListener("click", () =>
  tunnelAction("/api/tunnel/start", { remote: $("#tun-remote").value, key: $("#tun-key").value })
);
$("#tun-stop").addEventListener("click", () => tunnelAction("/api/tunnel/stop", {}));

$("#log").innerHTML =
  `<div class="empty">等待请求…(对 /v1/responses 或 /v1/messages 发一次请求即可看到)</div>`;
loadClientConfigs();
startCopyButtons();
loadStatus();
loadTunnel();
setInterval(() => {
  loadStatus();
  loadTunnel();
}, 5000);
startLogs();
