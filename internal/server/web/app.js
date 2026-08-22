const $ = (s) => document.querySelector(s);
const $$ = (s) => document.querySelectorAll(s);

function escapeHTML(value) {
  return String(value == null ? "" : value).replace(/[&<>"']/g, (char) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  })[char]);
}

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
// 远程隧道状态 + 客户端配置目标（local | ssh | public）。
let sshTunnel = { running: false, remote_port: "" };
let pub = { running: false, url: "", key: "" };
let clientTarget = "local";
let responsesUpstream = {
  active_source: "subscription",
  subscription: {},
  custom: {
    configured: false,
    name: "Custom Responses",
    base_url: "",
    default_model: "gpt-5.6-sol",
    has_api_key: false,
  },
};
// Claude 上游档案(ferridex-profiles.env):active 为 "" 表示本机 Anthropic 订阅。
let claudeUpstream = {
  active: "",
  stale_profile: "",
  profiles: [],
  subscription: {},
  file: "",
  file_error: "",
  warnings: [],
};
let claudeUpstreamBusy = false;

function publicActive() {
  return clientTarget === "public" && pub.running && !!pub.url;
}

function sshActive() {
  return clientTarget === "ssh" && sshTunnel.running && !!sshTunnel.remote_port;
}

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
  if (publicActive()) return pub.url; // 完整 https://xxx.ngrok-free.app,无端口
  if (sshActive()) return `http://127.0.0.1:${sshTunnel.remote_port}`;
  return `http://${proxyHost()}:${currentPort()}`;
}
// Cursor CLI only routes agent traffic through a custom endpoint when the host
// is "localhost" (127.0.0.1 bypasses the proxy and hits agentn.* directly).
function cursorBaseURL(port) {
  if (publicActive()) return pub.url;
  if (sshActive()) return `http://localhost:${sshTunnel.remote_port}`;
  const host = proxyHost();
  const cursorHost = host === "127.0.0.1" ? "localhost" : host;
  return `http://${cursorHost}:${port || currentPort()}`;
}
function authToken() {
  if (publicActive()) return pub.key || "dummy";
  return netinfo.lan_key || "dummy";
}

function effectiveResponsesModel() {
  const custom = responsesUpstream.custom || {};
  const subscription = responsesUpstream.subscription || {};
  if (responsesUpstream.active_source === "custom" && custom.configured && custom.default_model) {
    return String(custom.default_model);
  }
  if (subscription && typeof subscription === "object" && subscription.default_model) {
    return String(subscription.default_model);
  }
  return "gpt-5.6-sol";
}

// 档案激活时,生成的 Claude settings.json 模型名跟随档案映射(服务端也会改写,
// 这里让客户端展示的配置与实际生效模型一致)。
function effectiveClaudeModels() {
  const profile = activeClaudeProfile();
  return {
    opus: (profile && profile.opus_model) || "claude-opus-4-8",
    sonnet: (profile && profile.sonnet_model) || "claude-sonnet-4-5-20250929",
    haiku: (profile && profile.haiku_model) || "claude-haiku-4-5-20251001",
    subagent: (profile && profile.subagent_model) || "",
  };
}

// 档案激活且配置了模型映射时,给一键启动命令追加模型 env——这样 claude CLI
// 的欢迎栏和 /model 列表会直接显示档案里的实际模型名(显示是客户端本地行为)。
function claudeModelEnvParts() {
  const profile = activeClaudeProfile();
  if (!profile) return [];
  const parts = [];
  if (profile.opus_model) parts.push(`ANTHROPIC_DEFAULT_OPUS_MODEL=${shellSingleQuote(profile.opus_model)}`);
  if (profile.sonnet_model) parts.push(`ANTHROPIC_DEFAULT_SONNET_MODEL=${shellSingleQuote(profile.sonnet_model)}`);
  if (profile.haiku_model) parts.push(`ANTHROPIC_DEFAULT_HAIKU_MODEL=${shellSingleQuote(profile.haiku_model)}`);
  if (profile.subagent_model) parts.push(`CLAUDE_CODE_SUBAGENT_MODEL=${shellSingleQuote(profile.subagent_model)}`);
  return parts;
}

function activeClaudeProfile() {
  if (!claudeUpstream.active) return null;
  return claudeUpstream.profiles.find((p) => p.name === claudeUpstream.active && p.valid) || null;
}

function tomlString(value) {
  return JSON.stringify(String(value == null ? "" : value));
}

function shellSingleQuote(value) {
  return `'${String(value == null ? "" : value).replace(/'/g, `'"'"'`)}'`;
}

function loadClientConfigs() {
  const baseURL = proxyBaseURL();
  const token = authToken();
  const model = effectiveResponsesModel();
  const claudeModels = effectiveClaudeModels();
  const claude = {
    env: {
      ANTHROPIC_BASE_URL: baseURL,
      ANTHROPIC_AUTH_TOKEN: token,
      ANTHROPIC_DEFAULT_OPUS_MODEL: claudeModels.opus,
      ANTHROPIC_DEFAULT_SONNET_MODEL: claudeModels.sonnet,
      ANTHROPIC_DEFAULT_HAIKU_MODEL: claudeModels.haiku,
      ...(claudeModels.subagent ? { CLAUDE_CODE_SUBAGENT_MODEL: claudeModels.subagent } : {}),
    },
    permissions: { defaultMode: "bypassPermissions" },
    skipDangerousModePermissionPrompt: true,
    model: "opus",
  };
  const codex = `model = ${tomlString(model)}
model_provider = "ferridex"

model_reasoning_effort = "xhigh"
model_reasoning_summary = "none"
model_verbosity = "medium"

[model_providers.ferridex]
name = "Ferridex Responses Proxy"
base_url = ${tomlString(`${baseURL}/v1`)}
wire_api = "responses"
env_key = "LOCAL_PROXY_KEY"
request_max_retries = 4
stream_max_retries = 5
stream_idle_timeout_ms = 300000
requires_openai_auth = false`;

  // 一键启动命令内联当前有效模型、Responses 路由和企业上游所需的稳定重试参数。
  // 档案激活时带上模型 env,让 CLI 界面直接显示档案配置的实际模型。
  const claudeLaunch = [...claudeModelEnvParts(), `ANTHROPIC_BASE_URL=${baseURL}`, `ANTHROPIC_AUTH_TOKEN=${token}`, "claude"].join(" ");
  const codexLaunch = [
    `LOCAL_PROXY_KEY=${shellSingleQuote(token)} codex`,
    `-c ${shellSingleQuote(`model=${tomlString(model)}`)}`,
    `-c ${shellSingleQuote('model_provider="ferridex"')}`,
    `-c ${shellSingleQuote('model_reasoning_effort="xhigh"')}`,
    `-c ${shellSingleQuote('model_reasoning_summary="none"')}`,
    `-c ${shellSingleQuote('model_verbosity="medium"')}`,
    `-c ${shellSingleQuote('model_providers.ferridex.name="Ferridex Responses Proxy"')}`,
    `-c ${shellSingleQuote(`model_providers.ferridex.base_url=${tomlString(`${baseURL}/v1`)}`)}`,
    `-c ${shellSingleQuote('model_providers.ferridex.wire_api="responses"')}`,
    `-c ${shellSingleQuote('model_providers.ferridex.env_key="LOCAL_PROXY_KEY"')}`,
    `-c ${shellSingleQuote('model_providers.ferridex.request_max_retries=4')}`,
    `-c ${shellSingleQuote('model_providers.ferridex.stream_max_retries=5')}`,
    `-c ${shellSingleQuote('model_providers.ferridex.stream_idle_timeout_ms=300000')}`,
    `-c ${shellSingleQuote('model_providers.ferridex.requires_openai_auth=false')}`,
  ].join(" \\\n  ");

  $("#proxy-url").textContent = baseURL;
  $("#claude-launch").textContent = claudeLaunch;
  $("#claude-config").textContent = JSON.stringify(claude, null, 2);
  $("#codex-launch").textContent = codexLaunch;
  $("#codex-config").textContent = codex;
  $("#codex-env").textContent = `export LOCAL_PROXY_KEY=${shellSingleQuote(token)}`;
  $("#cursor-config").textContent = `agent -e ${cursorBaseURL()} --auth-token ${token}`;
  // Grok 没有 -e/--auth-token,用环境变量指向代理。**不要用 XAI_API_KEY**:它会切到
  // BYOK 模式直连 api.x.ai,绕过代理。改用 auth_provider_command 提供占位 token(grok
  // 会当成会话令牌发给代理,ferridex 再替换成本机订阅 token)。独立 GROK_HOME 避免污染
  // 本机真实的 ~/.grok 登录。LAN 下占位 token 即密钥。
  // 先清隔离 home 的缓存,保证 grok 每次都用当前占位 token(dummy/密钥),
  // 避免切换 SSH↔LAN 或换密钥后 grok 复用旧缓存 token 导致 401。
  $("#grok-launch").textContent =
    `rm -f $HOME/.grok-ferridex/auth.json; export GROK_HOME=$HOME/.grok-ferridex GROK_CLI_CHAT_PROXY_BASE_URL=${baseURL}/grok/v1 GROK_AUTH_PROVIDER_COMMAND='echo ${token}' GROK_AUTH_TOKEN_TTL=3600; grok`;
  // 上游页 Grok/Cursor 标签的命令预览与「客户端」页保持一致。
  const grokPreview = $("#grok-cmd");
  if (grokPreview) grokPreview.textContent = $("#grok-launch").textContent;
  const cursorPreview = $("#cursor-cmd");
  if (cursorPreview) cursorPreview.textContent = $("#cursor-config").textContent;
}

// 按指定路由模式临时生成客户端配置并取其中一段(上游页「一键复制」用)。
// 复用 loadClientConfigs:交换全局 clientTarget 生成后立即还原,不影响页面状态。
function withRoutedLaunch(mode, pick) {
  const prev = clientTarget;
  clientTarget = mode;
  try {
    loadClientConfigs();
    return pick();
  } finally {
    clientTarget = prev;
    loadClientConfigs();
  }
}

async function loadNetinfo() {
  try {
    const r = await fetch("/api/netinfo");
    netinfo = await r.json();
  } catch {}
  loadClientConfigs();
  renderLanInfo();
}

/* ===== 上游切换中心:Claude / Codex / Grok / Cursor 标签页 ===== */
const HUB_TABS = ["claude", "codex", "grok", "cursor"];
const HUB_COPY_LABELS = {
  claude: "复制 Claude 启动命令",
  codex: "复制 Codex 启动命令",
  grok: "复制 Grok 启动命令",
  cursor: "复制 Cursor 启动命令",
};
// 一键复制读取的命令元素(与「客户端」页生成的一一对应)。
const HUB_COPY_TARGETS = {
  claude: "#claude-launch",
  codex: "#codex-launch",
  grok: "#grok-launch",
  cursor: "#cursor-config",
};

let hubTab = "claude";
try {
  const saved = localStorage.getItem("ferridex.hubTab");
  if (HUB_TABS.includes(saved)) hubTab = saved;
} catch {}

function setHubTab(tab) {
  hubTab = HUB_TABS.includes(tab) ? tab : "claude";
  $$(".hub-tab").forEach((btn) => {
    const on = btn.dataset.hubTab === hubTab;
    btn.classList.toggle("active", on);
    btn.setAttribute("aria-selected", String(on));
  });
  $$(".hub-pane").forEach((pane) => {
    pane.hidden = pane.dataset.hubPane !== hubTab;
  });
  // 「重新加载档案」只对 Claude 档案有意义,跟随标签页显隐。
  $("#claude-upstream-reload").hidden = hubTab !== "claude";
  $("#claude-upstream-status").hidden = hubTab !== "claude";
  // 一键复制跟随子标签:复制当前客户端类型的一键启动命令。
  const copyBtn = $("#hub-copy-launch");
  if (copyBtn) copyBtn.textContent = HUB_COPY_LABELS[hubTab];
  try { localStorage.setItem("ferridex.hubTab", hubTab); } catch {}
}

/* ===== Responses 上游配置 ===== */
let upstreamFormDirty = false;
let upstreamBusy = false;

function normalizeResponsesUpstream(data) {
  const raw = data && typeof data === "object" ? data : {};
  const custom = raw.custom && typeof raw.custom === "object" ? raw.custom : {};
  return {
    active_source: raw.active_source === "custom" ? "custom" : "subscription",
    subscription: raw.subscription == null ? {} : raw.subscription,
    custom: {
      configured: !!custom.configured,
      name: String(custom.name || "Custom Responses"),
      base_url: String(custom.base_url || ""),
      default_model: String(custom.default_model || "gpt-5.6-sol"),
      has_api_key: !!custom.has_api_key,
    },
  };
}

function subscriptionAvailable() {
  const subscription = responsesUpstream.subscription;
  if (typeof subscription === "boolean") return subscription;
  if (!subscription || typeof subscription !== "object") return true;
  if (subscription.available != null) return !!subscription.available;
  if (subscription.logged_in != null) return !!subscription.logged_in;
  return true;
}

function renderResponsesUpstream() {
  const custom = responsesUpstream.custom || {};
  const active = responsesUpstream.active_source;
  const subscriptionOK = subscriptionAvailable();
  const customConfigured = !!custom.configured;

  $("#upstream-source-subscription").classList.toggle("active", active === "subscription");
  $("#upstream-source-custom").classList.toggle("active", active === "custom");
  $("#upstream-custom-title").textContent = custom.name || "Custom Responses";
  // cc-switch 风格:绿色「当前使用」药丸跟随激活态,自定义卡显示已存 Base URL。
  $("#upstream-source-subscription .pill-current").hidden = active !== "subscription";
  $("#upstream-source-custom .pill-current").hidden = active !== "custom";
  const customURL = $("#upstream-custom-url");
  customURL.textContent = custom.base_url || "";
  customURL.hidden = !custom.base_url;

  if (active === "subscription") {
    $("#upstream-subscription-state").textContent = "当前使用";
  } else {
    $("#upstream-subscription-state").textContent = subscriptionOK ? "可切换" : "不可用";
  }
  if (active === "custom") {
    $("#upstream-custom-state").textContent = "当前使用";
  } else {
    $("#upstream-custom-state").textContent = customConfigured ? "已配置" : "尚未配置";
  }

  $("#upstream-use-subscription").textContent = active === "subscription" ? "当前使用本机订阅" : "切换到本机订阅";
  $("#upstream-activate").textContent = active === "custom" ? "当前使用自定义上游" : "启用自定义上游";

  const controls = $$("#codex-pane button, #codex-pane input");
  controls.forEach((control) => { control.disabled = upstreamBusy; });
  if (!upstreamBusy) {
    $("#upstream-use-subscription").disabled = active === "subscription" || !subscriptionOK;
    $("#upstream-activate").disabled = active === "custom" || !customConfigured;
    $("#upstream-test").disabled = !customConfigured;
    $("#upstream-clear").disabled = !customConfigured;
  }

  loadClientConfigs();
}

function populateResponsesUpstreamForm(force) {
  if (upstreamFormDirty && !force) return;
  const custom = responsesUpstream.custom || {};
  $("#upstream-name").value = custom.name || "Custom Responses";
  $("#upstream-base-url").value = custom.base_url || "";
  $("#upstream-model").value = custom.default_model || "gpt-5.6-sol";
  $("#upstream-api-key").value = "";
  $("#upstream-api-key").type = "password";
  $("#upstream-key-toggle").textContent = "显示";
  $("#upstream-key-toggle").setAttribute("aria-pressed", "false");
  $("#upstream-key-toggle").setAttribute("aria-label", "显示本次输入的 API Key");
  $("#upstream-api-key").placeholder = custom.has_api_key ? "留空则保留已保存的 API Key" : "首次保存时必填";
  $("#upstream-key-state").textContent = custom.has_api_key
    ? "API Key 已安全保存；留空会保留原值，页面不会回显。"
    : "尚未保存 API Key；首次保存时必填。";
  upstreamFormDirty = false;
}

function upstreamErrorMessage(data, fallback) {
  if (data && typeof data.error === "string" && data.error) return data.error;
  if (data && data.error && typeof data.error.message === "string") return data.error.message;
  if (data && typeof data.message === "string" && data.message) return data.message;
  return fallback;
}

async function upstreamRequest(path, options) {
  const response = await fetch(path, options);
  let data = {};
  const text = await response.text();
  if (text) {
    try { data = JSON.parse(text); } catch { data = {}; }
  }
  if (!response.ok) {
    throw new Error(upstreamErrorMessage(data, `请求失败（HTTP ${response.status}）`));
  }
  return data;
}

function setUpstreamError(message) {
  $("#upstream-error").textContent = message || "";
}

function setUpstreamBusy(busy, message) {
  upstreamBusy = busy;
  $("#upstream-action-status").textContent = message || (!busy && upstreamFormDirty ? "有未保存的修改" : "");
  renderResponsesUpstream();
}

async function loadResponsesUpstream(forceForm) {
  try {
    const data = await upstreamRequest("/api/responses-upstream");
    responsesUpstream = normalizeResponsesUpstream(data);
    populateResponsesUpstreamForm(!!forceForm);
    renderResponsesUpstream();
    setUpstreamError("");
  } catch (error) {
    setUpstreamError(error instanceof Error ? error.message : String(error));
  }
}

async function activateResponsesSource(source) {
  setUpstreamError("");
  $("#upstream-test-result").textContent = "";
  setUpstreamBusy(true, "正在切换…");
  try {
    await upstreamRequest("/api/responses-upstream/activate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ source }),
    });
    await loadResponsesUpstream(false);
    toast(source === "custom" ? "已启用自定义 Responses 上游" : "已切换到本机订阅");
  } catch (error) {
    setUpstreamError(error instanceof Error ? error.message : String(error));
    toast("切换 Responses 上游失败", "err");
  } finally {
    setUpstreamBusy(false, "");
  }
}

async function saveResponsesUpstream(event) {
  event.preventDefault();
  const form = $("#upstream-form");
  if (!form.reportValidity()) return;

  const apiKey = $("#upstream-api-key").value.trim();
  if (!apiKey && !responsesUpstream.custom.has_api_key) {
    setUpstreamError("首次保存自定义上游时必须填写 API Key。");
    $("#upstream-api-key").focus();
    return;
  }

  const payload = {
    name: $("#upstream-name").value.trim(),
    base_url: $("#upstream-base-url").value.trim(),
    default_model: $("#upstream-model").value.trim(),
  };
  if (apiKey) payload.api_key = apiKey;

  setUpstreamError("");
  $("#upstream-test-result").textContent = "";
  setUpstreamBusy(true, "正在保存…");
  try {
    await upstreamRequest("/api/responses-upstream/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    upstreamFormDirty = false;
    await loadResponsesUpstream(true);
    toast("自定义 Responses 上游已保存");
  } catch (error) {
    setUpstreamError(error instanceof Error ? error.message : String(error));
    toast("保存 Responses 上游失败", "err");
  } finally {
    setUpstreamBusy(false, "");
  }
}

async function testResponsesUpstream() {
  if (!window.confirm("测试会向已保存的上游发送一次最小 Responses 请求，可能产生少量费用。继续测试吗？")) return;
  setUpstreamError("");
  $("#upstream-test-result").textContent = "";
  setUpstreamBusy(true, "正在测试连接…");
  try {
    const data = await upstreamRequest("/api/responses-upstream/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    if (data.ok === false || data.error) {
      throw new Error(upstreamErrorMessage(data, "上游连接测试失败"));
    }
    const details = [];
    const status = Number(data.status ?? data.status_code);
    const latency = Number(data.latency_ms ?? data.duration_ms);
    if (Number.isFinite(status) && status > 0) details.push(`HTTP ${status}`);
    if (Number.isFinite(latency) && latency >= 0) details.push(`${Math.round(latency)} ms`);
    if (typeof data.message === "string" && data.message) details.unshift(data.message);
    $("#upstream-test-result").textContent = details.length ? details.join(" · ") : "连接成功";
    toast("Responses 上游连接成功");
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    $("#upstream-test-result").textContent = `测试失败：${message}`;
    toast("Responses 上游连接测试失败", "err");
  } finally {
    setUpstreamBusy(false, "");
  }
}

async function clearResponsesUpstream() {
  if (!window.confirm("确定清除自定义 Responses 配置和已保存的 API Key 吗？清除后会切回本机订阅。")) return;
  setUpstreamError("");
  $("#upstream-test-result").textContent = "";
  setUpstreamBusy(true, "正在清除…");
  try {
    await upstreamRequest("/api/responses-upstream/clear", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    upstreamFormDirty = false;
    await loadResponsesUpstream(true);
    toast("自定义 Responses 配置已清除");
  } catch (error) {
    setUpstreamError(error instanceof Error ? error.message : String(error));
    toast("清除 Responses 配置失败", "err");
  } finally {
    setUpstreamBusy(false, "");
  }
}

/* ===== Claude 上游档案 ===== */
// 与标签页/静态卡共用的字形:六芒星 = Anthropic 订阅,六边形 = Codex/自定义。
const GLYPH_ANTHROPIC =
  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M12 4v16"/><path d="m5 8 14 8"/><path d="m19 8L5 16"/></svg>`;

function normalizeClaudeUpstream(data) {
  const raw = data && typeof data === "object" ? data : {};
  const profiles = Array.isArray(raw.profiles) ? raw.profiles : [];
  return {
    active: typeof raw.active === "string" ? raw.active : "",
    stale_profile: typeof raw.stale_profile === "string" ? raw.stale_profile : "",
    profiles: profiles.map((p) => {
      const item = p && typeof p === "object" ? p : {};
      return {
        name: String(item.name || ""),
        base_url: String(item.base_url || ""),
        auth_mode: String(item.auth_mode || ""),
        has_auth_token: !!item.has_auth_token,
        has_api_key: !!item.has_api_key,
        valid: !!item.valid,
        opus_model: String(item.opus_model || ""),
        sonnet_model: String(item.sonnet_model || ""),
        haiku_model: String(item.haiku_model || ""),
        subagent_model: String(item.subagent_model || ""),
        problems: Array.isArray(item.problems) ? item.problems.map(String) : [],
        warnings: Array.isArray(item.warnings) ? item.warnings.map(String) : [],
      };
    }),
    subscription: raw.subscription == null ? {} : raw.subscription,
    file: String(raw.file || ""),
    file_error: String(raw.file_error || ""),
    warnings: Array.isArray(raw.warnings) ? raw.warnings.map(String) : [],
  };
}

function renderClaudeUpstream() {
  const sources = $("#claude-upstream-sources");
  if (!sources) return;
  const active = claudeUpstream.active;
  const sub = claudeUpstream.subscription || {};
  const subOK = sub.available == null ? true : !!sub.available;

  const cards = [];
  cards.push(
    `<div class="upstream-source${active === "" ? " active" : ""}" data-claude-upstream="">` +
      `<span class="src-icon grad" aria-hidden="true">${GLYPH_ANTHROPIC}</span>` +
      `<div class="src-main">` +
        `<div class="src-name-row"><strong>本机 Anthropic 订阅</strong>` +
        (active === "" ? `<span class="pill-current">当前使用</span>` : "") +
        `</div>` +
        `<div class="src-url">api.anthropic.com · OAuth 登录态</div>` +
        `<p class="src-desc">继续使用本机 Claude 登录态与配额熔断，不需要额外 API Key。</p>` +
      `</div>` +
      `<span class="src-side">${active === "" ? "当前使用" : subOK ? "可切换" : "不可用"}</span>` +
    `</div>`
  );
  claudeUpstream.profiles.forEach((p) => {
    if (!p.name) return;
    const initial = escapeHTML((p.name.trim()[0] || "?").toUpperCase());
    const tags = [
      ["opus", p.opus_model],
      ["sonnet", p.sonnet_model],
      ["haiku", p.haiku_model],
    ]
      .filter(([, model]) => model)
      .map(([tier, model]) => `<span class="tag">${escapeHTML(tier)}→${escapeHTML(model)}</span>`);
    const problems = [...(p.problems || []), ...(p.warnings || [])];
    const problemHTML = problems.length
      ? `<ul class="upstream-source-problems">${problems.map((x) => `<li>${escapeHTML(x)}</li>`).join("")}</ul>`
      : "";
    cards.push(
      `<div class="upstream-source${active === p.name ? " active" : ""}${p.valid ? "" : " invalid"}" data-claude-upstream="${escapeHTML(p.name)}">` +
        `<span class="src-icon soft" aria-hidden="true">${initial}</span>` +
        `<div class="src-main">` +
          `<div class="src-name-row"><strong>${escapeHTML(p.name)}</strong>` +
          (active === p.name ? `<span class="pill-current">当前使用</span>` : "") +
          `</div>` +
          (p.base_url ? `<div class="src-url">${escapeHTML(p.base_url)}</div>` : "") +
          (p.auth_mode
            ? `<div class="src-meta">${p.auth_mode === "bearer" ? "Authorization: Bearer" : "x-api-key"} 凭据已配置</div>`
            : "") +
          (tags.length ? `<div class="upstream-tags">${tags.join("")}</div>` : "") +
          problemHTML +
        `</div>` +
        `<span class="src-side">${!p.valid ? "配置无效" : active === p.name ? "当前使用" : "点击切换"}</span>` +
      `</div>`
    );
  });
  // 「新增档案」提示卡:非交互,告诉用户入口在档案文件里。
  cards.push(
    `<div class="upstream-source src-add" aria-hidden="true">` +
      `<span class="src-icon">+</span>` +
      `<div class="src-main">` +
        `<div class="src-name-row"><strong>新增档案</strong></div>` +
        `<div class="src-desc">在 ferridex-profiles.env 里添加 <span class="tag">[档案名]</span> 段并保存，然后点上方「重新加载档案」。</div>` +
      `</div>` +
    `</div>`
  );
  sources.innerHTML = cards.join("");

  $("#claude-upstream-reload").disabled = claudeUpstreamBusy;

  let hint = claudeUpstream.file ? `档案文件：${claudeUpstream.file}。` : "";
  if (claudeUpstream.stale_profile) {
    hint += ` 已保存的档案「${claudeUpstream.stale_profile}」不在文件中，当前已回退到本机订阅。`;
  }
  $("#claude-upstream-hint").textContent = hint;

  // 档案激活状态变化时,立即让「客户端配置」里的模型名跟随档案映射。
  loadClientConfigs();
}

function setClaudeUpstreamError(message) {
  $("#claude-upstream-error").textContent = message || "";
}

async function loadClaudeUpstream() {
  try {
    const data = await upstreamRequest("/api/claude-upstream");
    claudeUpstream = normalizeClaudeUpstream(data);
    renderClaudeUpstream();
    setClaudeUpstreamError("");
  } catch (error) {
    setClaudeUpstreamError(error instanceof Error ? error.message : String(error));
  }
}

async function activateClaudeUpstream(name) {
  setClaudeUpstreamError("");
  claudeUpstreamBusy = true;
  $("#claude-upstream-status").textContent = "正在切换…";
  renderClaudeUpstream();
  try {
    await upstreamRequest("/api/claude-upstream/activate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    await loadClaudeUpstream();
    toast(name ? `已切换到 Claude 档案「${name}」` : "已切换到本机 Anthropic 订阅");
  } catch (error) {
    setClaudeUpstreamError(error instanceof Error ? error.message : String(error));
    toast("切换 Claude 上游失败", "err");
  } finally {
    claudeUpstreamBusy = false;
    $("#claude-upstream-status").textContent = "";
    renderClaudeUpstream();
  }
}

async function reloadClaudeProfiles() {
  setClaudeUpstreamError("");
  claudeUpstreamBusy = true;
  $("#claude-upstream-status").textContent = "正在重新加载…";
  renderClaudeUpstream();
  try {
    await upstreamRequest("/api/claude-upstream/reload", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    await loadClaudeUpstream();
    toast("已重新加载 Claude 档案");
  } catch (error) {
    setClaudeUpstreamError(error instanceof Error ? error.message : String(error));
    toast("重新加载 Claude 档案失败", "err");
  } finally {
    claudeUpstreamBusy = false;
    $("#claude-upstream-status").textContent = "";
    renderClaudeUpstream();
  }
}

function renderLanInfo() {
  const el = $("#lan-info");
  if (!el) return;
  if (netinfo.lan_enabled) {
    const port = currentPort();
    const ips = netinfo.lan_ips || [];
    const urls = ips.length
      ? ips.map((ip) => escapeHTML(`http://${ip}:${port}`)).join("　")
      : "未检测到局域网 IPv4 地址";
    el.innerHTML =
      `<div class="ep"><span class="name">局域网</span><code>${urls}</code></div>` +
      `<div class="ep"><span class="name">密钥</span><code>${escapeHTML(netinfo.lan_key)}</code></div>` +
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
        <span class="usage-label">${escapeHTML(u.label || "")}</span>
        <span class="usage-pct">${pct}%</span>
      </div>
      <div class="bar"><div class="bar-fill ${usageLevel(frac)}" style="width:${pct}%"></div></div>
      ${reset ? `<div class="usage-reset">${reset}</div>` : ""}
    </div>`;
}

/* ===== 按需用量查询(点击才查,不自动轮询;镜像桌面端的弹层交互) ===== */
const usagePanels = {}; // name -> { open, loading, usage, error }

const usageChartSVG = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3v16a2 2 0 0 0 2 2h16"/><path d="M13 17V9"/><path d="M18 17V5"/><path d="M8 17v-3"/></svg>`;
const usageLoaderSVG = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>`;

function usagePopHTML(p, st) {
  let body;
  if (st.loading) {
    body = `<div class="usage-empty">查询中…</div>`;
  } else if (st.error) {
    body = `<div class="usage-err">${escapeHTML(st.error)}</div>`;
  } else if (st.usage && st.usage.length) {
    body = `<div class="usage">${st.usage.map(usageBar).join("")}</div>`;
  } else {
    body = `<div class="usage-empty">未返回用量数据</div>`;
  }
  return `
    <div class="usage-pop">
      <div class="usage-pop-head">
        <strong>${escapeHTML(p.title || p.name)} · 用量</strong>
        <button class="usage-close" data-usage-close="${escapeHTML(p.name)}" title="关闭">×</button>
      </div>
      ${body}
    </div>`;
}

async function queryUsage(name) {
  const st = usagePanels[name] || (usagePanels[name] = {});
  st.open = true;
  st.loading = true;
  st.error = "";
  renderCards();
  try {
    const r = await fetch(`/api/usage/${encodeURIComponent(name)}`, {
      method: "POST",
    });
    const d = await r.json();
    st.usage = d.usage || [];
    st.error = d.error || "";
  } catch (e) {
    st.error = String(e);
  }
  st.loading = false;
  renderCards();
}

let lastProviders = [];

// 渲染 provider 卡片(loadStatus 每 5 秒调用;用量弹层开合时也就地重渲染)。
// 返回解析出的 Claude 恢复时间,供健康条倒计时使用。
function renderCards() {
  const providers = lastProviders;
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
        const safeName = escapeHTML(p.name);
        const safeTitle = escapeHTML(p.title || p.name);
        const claudeCounting = p.name === "claude" && claudeResetMs;
        if (claudeCounting) {
          countdowns.push({ id: cardId, resetMs: claudeResetMs });
        }
        const collapsedClass = collapsedCards.has(p.name) ? " collapsed" : "";
        const st = usagePanels[p.name] || {};
        const usageBtn = p.supports_usage
          ? `<button class="usage-btn${st.loading ? " loading" : ""}${st.open ? " active" : ""}" data-usage="${safeName}" title="查询用量" ${p.logged_in ? "" : "disabled"}>${st.loading ? usageLoaderSVG : usageChartSVG}</button>`
          : "";
        return `
      <div class="card ${p.logged_in ? "" : "off"}${collapsedClass}" id="${escapeHTML(cardId)}">
        <div class="card-head">
          <span class="caret" aria-hidden="true">▸</span>
          <div class="card-head-main">
            <h3>${safeTitle}</h3>
            <span class="who">${safeName}</span>
          </div>
          <span class="card-head-status">
            ${dot(p.logged_in)}
            <strong>${p.logged_in ? "已登录" : "未登录"}</strong>
          </span>
          ${usageBtn}
        </div>
        ${st.open ? usagePopHTML(p, st) : ""}
        <div class="card-body">
          ${p.account ? `<div class="acct">${escapeHTML(p.account)}</div>` : ""}
          ${claudeCounting ? `<div class="detail"><span class="health-countdown"></span></div>` : (p.detail ? `<div class="detail">${escapeHTML(p.detail)}</div>` : "")}
          <div class="meta">${(p.models || []).map((m) => `<span class="tag">${escapeHTML(m)}</span>`).join("")}</div>
        </div>
      </div>`;
      })
      .join("") || `<div class="empty">无 provider</div>`;

  return claudeResetMs;
}

async function loadStatus() {
  try {
    const r = await fetch("/api/status");
    const d = await r.json();
    const providers = d.providers || [];
    lastProviders = providers;
    lastProvidersOK = true;

    const claudeResetMs = renderCards();

    $("#endpoints").innerHTML = providers
      .map((p) => {
        if (p.name === "cursor") {
          return `<div class="ep"><span class="name">${escapeHTML(p.name)}</span><code>agent -e ${escapeHTML(cursorBaseURL())} --auth-token ${escapeHTML(authToken())}</code></div>`;
        }
        if (p.name === "grok") {
          return `<div class="ep"><span class="name">${escapeHTML(p.name)}</span><code>GROK_CLI_CHAT_PROXY_BASE_URL=${escapeHTML(proxyBaseURL())}/grok/v1 (+ auth_provider_command)</code></div>`;
        }
        return `<div class="ep"><span class="name">${escapeHTML(p.name)}</span><code>POST ${escapeHTML(location.origin)}${escapeHTML(p.endpoint)}</code></div>`;
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
  return `<span class="t">${escapeHTML(o.time)}</span>  <span class="m">${escapeHTML(o.method)}</span> ${escapeHTML(o.path)} <span class="s ${statusClass(o.status)}">${escapeHTML(o.status)}</span> <span class="t">${escapeHTML(o.dur)}</span>`;
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

  // 筛选段(只绑定日志工具栏内的按钮,避免误伤客户端面板的「本地/公网」切换)
  $$(".log-toolbar .seg-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      $$(".log-toolbar .seg-btn").forEach((b) => b.classList.remove("active"));
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

    sshTunnel = {
      running: !!st.running,
      remote_port: st.remote_port ? String(st.remote_port) : "",
    };

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
    syncRemoteTargetAvailability();
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

/* ===== 公网隧道 (ngrok) ===== */
async function loadPublic() {
  try {
    const r = await fetch("/api/public");
    const d = await r.json();
    const st = d.status || {};
    const panel = $("#public-panel");
    const badge = $("#pub-badge");

    pub = { running: !!st.running, url: st.url || "", key: d.key || "" };

    panel.classList.toggle("is-running", pub.running);
    panel.classList.toggle("is-error", !pub.running && !!st.last_error);

    badge.className = "badge " + (pub.running ? "badge-on" : "badge-off");
    badge.textContent = pub.running ? "运行中" : st.last_error ? "异常" : "未运行";

    $("#pub-start").disabled = pub.running;
    $("#pub-stop").disabled = !pub.running;

    const urlRow = $("#pub-url-row");
    if (pub.running && pub.url) {
      $("#pub-url").textContent = pub.url;
      urlRow.hidden = false;
      const parts = [`<span class="pill">pid ${st.pid}</span>`, `<span class="pill">已运行 ${fmtUptime(st.uptime_sec)}</span>`];
      $("#pub-info").innerHTML = parts.join("");
    } else {
      urlRow.hidden = true;
      $("#pub-info").textContent = st.last_error ? "" : "未运行";
    }
    $("#pub-err").textContent = st.last_error || "";

    syncRemoteTargetAvailability();
    renderHubPublicButton();
  } catch (e) {}
}

function syncTargetToggle() {
  $$("#target-toggle .seg-btn").forEach((b) =>
    b.classList.toggle("active", b.dataset.target === clientTarget)
  );
}

function syncRemoteTargetAvailability() {
  const toggle = $("#target-toggle");
  const sshButton = $("#target-ssh");
  const publicButton = $("#target-public");
  if (sshButton) sshButton.hidden = !sshTunnel.running;
  if (publicButton) publicButton.hidden = !pub.running;
  if (toggle) toggle.hidden = !sshTunnel.running && !pub.running;
  if ((clientTarget === "ssh" && !sshTunnel.running) || (clientTarget === "public" && !pub.running)) {
    clientTarget = "local";
  }
  syncTargetToggle();
  loadClientConfigs();
}

async function publicAction(path) {
  $("#pub-info").textContent = "处理中…（首次建立隧道可能需要几秒）";
  try {
    const r = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
    const d = await r.json();
    if (d.error) {
      $("#pub-err").textContent = d.error;
      toast("公网隧道操作失败", "err");
    } else {
      toast(path.endsWith("/start") ? "公网隧道已启动" : "公网隧道已停止");
    }
  } catch (e) {
    $("#pub-err").textContent = String(e);
    toast("公网隧道操作失败", "err");
  }
  loadPublic();
}

/* ===== 上游页快捷操作:启动公网 + 一键复制启动命令 ===== */
let hubPublicBusy = false;

// 跟随 loadPublic(5s 轮询)同步上游页的公网启停按钮;busy 时让位给进行中文案。
function renderHubPublicButton() {
  const btn = $("#hub-public-toggle");
  if (!btn || hubPublicBusy) return;
  const running = pub.running && !!pub.url;
  btn.textContent = running ? "停止公网" : "启动公网";
  btn.classList.toggle("running", running);
  btn.title = running ? `公网地址：${pub.url}` : "启动 ngrok 公网隧道";
}

async function toggleHubPublic() {
  if (hubPublicBusy) return;
  hubPublicBusy = true;
  const btn = $("#hub-public-toggle");
  const running = pub.running;
  btn.disabled = true;
  btn.textContent = running ? "停止中…" : "启动中…（首次需几秒）";
  try {
    await publicAction(running ? "/api/public/stop" : "/api/public/start");
  } finally {
    // publicAction 收尾的 loadPublic 未被等待,这里再取一次确保按钮反映最终状态。
    await loadPublic();
    hubPublicBusy = false;
    btn.disabled = false;
    renderHubPublicButton();
  }
}

async function copyHubLaunch() {
  // 路由自动跟随公网状态,与「客户端」页的目标选择器无关。
  const usePublic = pub.running && !!pub.url;
  const btn = $("#hub-copy-launch");
  const original = btn.textContent;
  try {
    const cmd = withRoutedLaunch(usePublic ? "public" : "local", () =>
      $(HUB_COPY_TARGETS[hubTab] || HUB_COPY_TARGETS.claude).textContent
    );
    await copyText(cmd);
    btn.textContent = "已复制";
    toast(usePublic ? "已复制公网启动命令" : "已复制本机启动命令");
  } catch {
    btn.textContent = "复制失败";
    toast("复制失败", "err");
  }
  setTimeout(() => {
    btn.textContent = original;
  }, 1600);
}

/* ===== 主标签页:上游 / Provider 状态 / 远程访问 / 客户端 / 日志 ===== */
const CONSOLE_TABS = ["upstream", "providers", "access", "clients", "logs"];

let consoleTab = "upstream";
try {
  const saved = localStorage.getItem("ferridex.consoleTab");
  if (CONSOLE_TABS.includes(saved)) consoleTab = saved;
} catch {}

function setConsoleTab(tab) {
  consoleTab = CONSOLE_TABS.includes(tab) ? tab : "upstream";
  $$(".console-tab").forEach((btn) => {
    const on = btn.dataset.consoleTab === consoleTab;
    btn.classList.toggle("active", on);
    btn.setAttribute("aria-selected", String(on));
  });
  $$(".console-pane").forEach((pane) => {
    pane.hidden = pane.dataset.consolePane !== consoleTab;
  });
  try { localStorage.setItem("ferridex.consoleTab", consoleTab); } catch {}
}

/* ===== 启动 ===== */
initTheme();
setConsoleTab(consoleTab);
setHubTab(hubTab);

$(".console-tabs").addEventListener("click", (e) => {
  const btn = e.target.closest(".console-tab");
  if (btn) setConsoleTab(btn.dataset.consoleTab);
});

$(".hub-tabs").addEventListener("click", (e) => {
  const btn = e.target.closest(".hub-tab");
  if (btn) setHubTab(btn.dataset.hubTab);
});

// 上游页快捷操作:一键复制(路由跟随公网状态) + 公网隧道就地启停。
$("#hub-copy-launch").addEventListener("click", copyHubLaunch);
$("#hub-public-toggle").addEventListener("click", toggleHubPublic);

$("#upstream-form").addEventListener("submit", saveResponsesUpstream);
$("#upstream-form").addEventListener("input", () => {
  upstreamFormDirty = true;
  $("#upstream-action-status").textContent = "有未保存的修改";
});
$("#upstream-use-subscription").addEventListener("click", () => activateResponsesSource("subscription"));
$("#upstream-activate").addEventListener("click", () => activateResponsesSource("custom"));
// Codex 行卡整卡可点(与 cc-switch 一致);卡内按钮有自己的直绑 handler,直接放行。
$("#responses-cards").addEventListener("click", (e) => {
  if (e.target.closest("button")) return;
  const card = e.target.closest("[data-responses-source]");
  if (!card || upstreamBusy) return;
  activateResponsesSource(card.dataset.responsesSource);
});
$("#upstream-test").addEventListener("click", testResponsesUpstream);
$("#upstream-clear").addEventListener("click", clearResponsesUpstream);
$("#upstream-key-toggle").addEventListener("click", () => {
  const input = $("#upstream-api-key");
  const button = $("#upstream-key-toggle");
  const reveal = input.type === "password";
  input.type = reveal ? "text" : "password";
  button.textContent = reveal ? "隐藏" : "显示";
  button.setAttribute("aria-pressed", String(reveal));
  button.setAttribute("aria-label", reveal ? "隐藏本次输入的 API Key" : "显示本次输入的 API Key");
});

$("#tun-start").addEventListener("click", () =>
  tunnelAction("/api/tunnel/start", { remote: $("#tun-remote").value, key: $("#tun-key").value })
);
$("#tun-stop").addEventListener("click", () => tunnelAction("/api/tunnel/stop", {}));

// Claude 上游档案:委托绑在常驻容器上,5 秒重渲染后依旧生效;点击当前已激活
// 的卡片不重复发请求。
$("#claude-upstream-reload").addEventListener("click", reloadClaudeProfiles);
$("#claude-upstream-sources").addEventListener("click", (e) => {
  const card = e.target.closest("[data-claude-upstream]");
  if (!card || claudeUpstreamBusy) return;
  const name = card.dataset.claudeUpstream;
  if ((name || "") === (claudeUpstream.active || "")) return;
  activateClaudeUpstream(name);
});

$("#pub-start").addEventListener("click", () => publicAction("/api/public/start"));
$("#pub-stop").addEventListener("click", () => publicAction("/api/public/stop"));

// 客户端配置目标切换:本地/局域网、SSH 远端或 ngrok 公网。
$("#target-toggle").addEventListener("click", (e) => {
  const btn = e.target.closest(".seg-btn");
  if (!btn) return;
  const requested = btn.dataset.target;
  clientTarget = requested === "ssh" || requested === "public" ? requested : "local";
  syncTargetToggle();
  loadClientConfigs();
});

// provider 卡片交互:委托绑在常驻的 #cards 上,5 秒重渲染后依旧生效。
// 用量按钮/弹层的点击优先于头部折叠处理。
$("#cards").addEventListener("click", (e) => {
  const usageBtn = e.target.closest("[data-usage]");
  if (usageBtn) {
    const name = usageBtn.dataset.usage;
    const st = usagePanels[name];
    if (st && st.open) {
      st.open = false;
      renderCards();
    } else {
      queryUsage(name);
    }
    return;
  }
  const closeBtn = e.target.closest("[data-usage-close]");
  if (closeBtn) {
    const st = usagePanels[closeBtn.dataset.usageClose];
    if (st) st.open = false;
    renderCards();
    return;
  }
  if (e.target.closest(".usage-pop")) return; // 弹层内点击不触发折叠
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
loadResponsesUpstream(false);
loadClaudeUpstream();
startCopyButtons();
startAccordions();
loadStatus();
loadTunnel();
loadPublic();
setInterval(() => {
  loadStatus();
  loadTunnel();
  loadPublic();
  loadResponsesUpstream(false);
  loadClaudeUpstream();
}, 5000);
startLogs();
