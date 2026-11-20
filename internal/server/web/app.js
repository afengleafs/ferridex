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

const BRAND_ICON_PATHS = {
  agy: { src: "icons/antigravity.svg", mono: false },
  claude: { src: "icons/claude.svg", mono: false },
  codex: { src: "icons/openai.svg", mono: true },
  grok: { src: "icons/grok.svg", mono: true },
};

const TERMINAL_ICON =
  `<svg class="brand-icon utility-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m4 17 6-5-6-5"/><path d="M12 19h8"/></svg>`;

// Provider 名称由本机后端给出；这里只允许映射到随二进制嵌入的固定资源。
function brandIconHTML(name) {
  const key = String(name || "").toLowerCase();
  if (key === "cursor") return TERMINAL_ICON;
  const icon = BRAND_ICON_PATHS[key];
  if (!icon) return "";
  return `<img class="brand-icon${icon.mono ? " brand-icon-mono" : ""}" src="${icon.src}" alt="" aria-hidden="true" />`;
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
  active: "",
  stale_profile: "",
  profiles: [],
  subscription: {},
  file: "",
  file_error: "",
  warnings: [],
};
let codexUpstreamBusy = false;
let codexTestBusy = "";
// Claude 上游供应商(claude_provider.env):active 为 "" 表示本机 Anthropic 订阅。
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
  if (publicActive()) return pub.url; // 完整 https://xxx.ngrok-free.dev 等,无端口
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

// 供应商激活时,生成的 Claude settings.json 模型名跟随供应商映射(服务端也会改写,
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

// 供应商激活且配置了模型映射时,给一键启动命令追加模型 env——这样 claude CLI
// 的欢迎栏和 /model 列表会直接显示供应商里的实际模型名(显示是客户端本地行为)。
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
  const codex = `model_provider = "ferridex"

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

  // Codex 模型由客户端配置或默认值决定；这里只设置 Responses 路由和重试参数。
  // 供应商激活时带上模型 env,让 CLI 界面直接显示供应商配置的实际模型。
  const claudeLaunch = [...claudeModelEnvParts(), `ANTHROPIC_BASE_URL=${baseURL}`, `ANTHROPIC_AUTH_TOKEN=${token}`, "claude"].join(" ");
  const codexLaunch = [
    `LOCAL_PROXY_KEY=${shellSingleQuote(token)} codex`,
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
  // 上游页 Grok/Cursor/agy 标签的命令预览与当前快捷复制目标保持一致。
  const agyLaunch = `GEMINI_API_KEY=${shellSingleQuote(token)} GOOGLE_GEMINI_BASE_URL=${shellSingleQuote(baseURL + "/agy")} agy --settings '{"modelProvider":"gemini"}'`;
  $("#agy-launch").textContent = agyLaunch;
  $("#agy-cmd").textContent = agyLaunch;
  const grokPreview = $("#grok-cmd");
  if (grokPreview) grokPreview.textContent = $("#grok-launch").textContent;
  const cursorPreview = $("#cursor-cmd");
  if (cursorPreview) cursorPreview.textContent = $("#cursor-config").textContent;
  syncHubRoutePill();
}

function syncHubRoutePill() {
  const pill = $("#hub-route-pill");
  if (!pill) return;
  pill.classList.remove("is-public", "is-lan");
  if (pub.running && pub.url) {
    pill.textContent = "快捷目标：公网";
    pill.classList.add("is-public");
    pill.title = `快捷复制将使用公网地址：${pub.url}`;
  } else if (netinfo.lan_enabled && netinfo.lan_ips && netinfo.lan_ips.length) {
    pill.textContent = `快捷目标：局域网 (${netinfo.lan_ips[0]})`;
    pill.classList.add("is-lan");
    pill.title = "快捷复制将使用局域网地址";
  } else {
    pill.textContent = "快捷目标：本机";
    pill.title = "快捷复制将使用本机回环地址";
  }
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
const HUB_TABS = ["claude", "codex", "grok", "cursor", "agy"];
const HUB_COPY_LABELS = {
  claude: "复制 Claude 启动命令",
  codex: "复制 Codex 启动命令",
  grok: "复制 Grok 启动命令",
  agy: "复制 agy 启动命令",
  cursor: "复制 Cursor 启动命令",
};
// 一键复制读取的命令元素(与「客户端」页生成的一一对应)。
const HUB_COPY_TARGETS = {
  claude: "#claude-launch",
  codex: "#codex-launch",
  grok: "#grok-launch",
  agy: "#agy-launch",
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
    btn.tabIndex = on ? 0 : -1;
  });
  $$(".hub-pane").forEach((pane) => {
    pane.hidden = pane.dataset.hubPane !== hubTab;
  });
  // 重新加载按钮只在支持文件供应商的 Claude/Codex 标签中显示。
  $("#claude-upstream-reload").hidden = hubTab !== "claude";
  $("#claude-upstream-status").hidden = hubTab !== "claude";
  $("#codex-upstream-reload").hidden = hubTab !== "codex";
  $("#codex-upstream-status").hidden = hubTab !== "codex";
  // 一键复制跟随子标签:复制当前客户端类型的一键启动命令。
  const copyBtn = $("#hub-copy-launch");
  if (copyBtn) copyBtn.textContent = HUB_COPY_LABELS[hubTab];
  try { localStorage.setItem("ferridex.hubTab", hubTab); } catch {}
}

/* ===== Codex 上游供应商 ===== */

function normalizeResponsesUpstream(data) {
  const raw = data && typeof data === "object" ? data : {};
  const profiles = Array.isArray(raw.profiles) ? raw.profiles : [];
  return {
    active_source: raw.active_source === "custom" ? "custom" : "subscription",
    active: typeof raw.active === "string" ? raw.active : "",
    stale_profile: typeof raw.stale_profile === "string" ? raw.stale_profile : "",
    profiles: profiles.map((profile) => {
      const item = profile && typeof profile === "object" ? profile : {};
      return {
        name: String(item.name || ""),
        base_url: String(item.base_url || ""),
        default_model: String(item.default_model || ""),
        has_api_key: !!item.has_api_key,
        valid: !!item.valid,
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

function responsesSubscriptionAvailable() {
  const subscription = responsesUpstream.subscription;
  if (typeof subscription === "boolean") return subscription;
  if (!subscription || typeof subscription !== "object") return true;
  if (subscription.available != null) return !!subscription.available;
  if (subscription.logged_in != null) return !!subscription.logged_in;
  return true;
}

function renderResponsesUpstream() {
  const sources = $("#codex-upstream-sources");
  if (!sources) return;
  const active = responsesUpstream.active;
  const subscriptionActive = responsesUpstream.active_source !== "custom";
  const subscriptionOK = responsesSubscriptionAvailable();
  const cards = [];
  cards.push(
    `<div class="upstream-source${subscriptionActive ? " active" : ""}">` +
      `<span class="src-icon brand" aria-hidden="true">${brandIconHTML("codex")}</span>` +
      `<div class="src-main">` +
        `<div class="src-name-row"><strong>本机 ChatGPT 订阅</strong>` +
        (subscriptionActive ? `<span class="pill-current">当前使用</span>` : "") +
        `</div>` +
        `<div class="src-url">chatgpt.com · OAuth 登录态</div>` +
        `<p class="src-desc">继续使用本机 Codex 登录态，不需要额外 API Key。</p>` +
      `</div>` +
      `<div class="src-side"><span>${subscriptionActive ? "当前使用" : subscriptionOK ? "可切换" : "不可用"}</span>` +
        `<button class="ghost-btn" type="button" data-codex-activate="" ${subscriptionActive || !subscriptionOK || codexUpstreamBusy ? "disabled" : ""}>启用</button>` +
      `</div>` +
    `</div>`
  );

  responsesUpstream.profiles.forEach((profile) => {
    if (!profile.name) return;
    const current = responsesUpstream.active_source === "custom" && active === profile.name;
    const initial = escapeHTML((profile.name.trim()[0] || "?").toUpperCase());
    const problems = [...(profile.problems || []), ...(profile.warnings || [])];
    const problemHTML = problems.length
      ? `<ul class="upstream-source-problems">${problems.map((message) => `<li>${escapeHTML(message)}</li>`).join("")}</ul>`
      : "";
    cards.push(
      `<div class="upstream-source${current ? " active" : ""}${profile.valid ? "" : " invalid"}">` +
        `<span class="src-icon soft" aria-hidden="true">${initial}</span>` +
        `<div class="src-main">` +
          `<div class="src-name-row"><strong>${escapeHTML(profile.name)}</strong>` +
          (current ? `<span class="pill-current">当前使用</span>` : "") +
          `</div>` +
          (profile.base_url ? `<div class="src-url">${escapeHTML(profile.base_url)}</div>` : "") +
          (profile.has_api_key ? `<div class="src-meta">API Key 已配置</div>` : "") +
          (profile.default_model ? `<div class="upstream-tags"><span class="tag">${escapeHTML(profile.default_model)}</span></div>` : "") +
          problemHTML +
        `</div>` +
        `<div class="src-side"><span>${!profile.valid ? "配置无效" : current ? "当前使用" : "可切换"}</span>` +
          `<span class="source-actions">` +
            `<button class="ghost-btn" type="button" data-codex-test="${escapeHTML(profile.name)}" ${!profile.valid || codexUpstreamBusy ? "disabled" : ""}>${codexTestBusy === profile.name ? "测试中…" : "测试"}</button>` +
            `<button class="${current ? "ghost-btn" : "solid-btn"}" type="button" data-codex-activate="${escapeHTML(profile.name)}" ${!profile.valid || current || codexUpstreamBusy ? "disabled" : ""}>${current ? "当前使用" : "启用"}</button>` +
          `</span>` +
        `</div>` +
      `</div>`
    );
  });
  cards.push(
    `<div class="upstream-source src-add">` +
      `<span class="src-icon">+</span>` +
      `<div class="src-main">` +
        `<div class="src-name-row"><strong>新增供应商</strong></div>` +
        `<div class="src-desc hint">在 codex_provider.env 里添加 <span class="tag">[供应商名]</span> 段并保存，然后点击上方「重新加载」。</div>` +
      `</div>` +
    `</div>`
  );
  sources.innerHTML = cards.join("");
  $("#codex-upstream-reload").disabled = codexUpstreamBusy;

  const hints = [];
  if (responsesUpstream.file) hints.push(`供应商文件：${responsesUpstream.file}。`);
  if (responsesUpstream.stale_profile) hints.push(`已保存的供应商「${responsesUpstream.stale_profile}」不可用，Codex 路由保持关闭，不会回退本机订阅。`);
  if (responsesUpstream.file_error) hints.push(`读取失败：${responsesUpstream.file_error}`);
  if (responsesUpstream.warnings.length) hints.push(responsesUpstream.warnings.join("；"));
  $("#codex-upstream-hint").textContent = hints.join(" ");
  loadClientConfigs();
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

function setCodexUpstreamError(message) {
  $("#codex-upstream-error").textContent = message || "";
}

async function loadResponsesUpstream() {
  try {
    const data = await upstreamRequest("/api/responses-upstream");
    responsesUpstream = normalizeResponsesUpstream(data);
    renderResponsesUpstream();
    setCodexUpstreamError("");
  } catch (error) {
    setCodexUpstreamError(error instanceof Error ? error.message : String(error));
  }
}

async function activateResponsesSource(name) {
  setCodexUpstreamError("");
  codexUpstreamBusy = true;
  $("#codex-upstream-status").textContent = "切换中…";
  renderResponsesUpstream();
  try {
    await upstreamRequest("/api/responses-upstream/activate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    await loadResponsesUpstream();
    toast(name ? `已切换到 Codex 供应商「${name}」` : "已切换到本机 ChatGPT 订阅");
  } catch (error) {
    setCodexUpstreamError(error instanceof Error ? error.message : String(error));
    toast("切换 Codex 供应商失败", "err");
  } finally {
    codexUpstreamBusy = false;
    $("#codex-upstream-status").textContent = "";
    renderResponsesUpstream();
  }
}

async function reloadCodexProfiles() {
  setCodexUpstreamError("");
  codexUpstreamBusy = true;
  $("#codex-upstream-status").textContent = "重新加载中…";
  renderResponsesUpstream();
  try {
    await upstreamRequest("/api/responses-upstream/reload", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    await loadResponsesUpstream();
    toast("已重新加载 Codex 供应商");
  } catch (error) {
    setCodexUpstreamError(error instanceof Error ? error.message : String(error));
    toast("重新加载 Codex 供应商失败", "err");
  } finally {
    codexUpstreamBusy = false;
    $("#codex-upstream-status").textContent = "";
    renderResponsesUpstream();
  }
}

async function testResponsesUpstream(name) {
  if (!window.confirm(`测试会向供应商「${name}」发送一次最小 Responses 请求，可能产生少量费用。确定要测试吗？`)) return;
  setCodexUpstreamError("");
  codexTestBusy = name;
  renderResponsesUpstream();
  try {
    const data = await upstreamRequest("/api/responses-upstream/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    if (data.ok === false || data.error) {
      throw new Error(upstreamErrorMessage(data, "连接测试失败"));
    }
    const details = [];
    const status = Number(data.status ?? data.status_code);
    const latency = Number(data.latency_ms ?? data.duration_ms);
    if (Number.isFinite(status) && status > 0) details.push(`HTTP ${status}`);
    if (Number.isFinite(latency) && latency >= 0) details.push(`${Math.round(latency)} ms`);
    if (typeof data.message === "string" && data.message) details.unshift(data.message);
    toast(`${name}：${details.length ? details.join(" · ") : "连接成功"}`);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    setCodexUpstreamError(`测试失败：${message}`);
    toast(`${name} 连接测试失败`, "err");
  } finally {
    codexTestBusy = "";
    renderResponsesUpstream();
  }
}

/* ===== Claude 上游供应商 ===== */

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
  const subDisabled = active !== "" && !subOK;

  const cards = [];
  cards.push(
    `<div class="upstream-source${active === "" ? " active" : ""}" data-claude-upstream="" role="button" tabindex="0" aria-pressed="${active === ""}" aria-disabled="${subDisabled}">` +
      `<span class="src-icon brand" aria-hidden="true">${brandIconHTML("claude")}</span>` +
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
      `<div class="upstream-source${active === p.name ? " active" : ""}${p.valid ? "" : " invalid"}" data-claude-upstream="${escapeHTML(p.name)}" role="button" tabindex="0" aria-pressed="${active === p.name}" aria-disabled="${!p.valid}">` +
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
        `<span class="src-side">${!p.valid ? "配置无效" : active === p.name ? "当前使用" : "可切换"}</span>` +
      `</div>`
    );
  });
  // 「新增供应商」提示卡:非交互,告诉用户入口在供应商文件里。
  cards.push(
    `<div class="upstream-source src-add">` +
      `<span class="src-icon">+</span>` +
      `<div class="src-main">` +
        `<div class="src-name-row"><strong>新增供应商</strong></div>` +
        `<div class="src-desc hint">在 claude_provider.env 里添加 <span class="tag">[供应商名]</span> 段并保存，然后点击上方「重新加载」。</div>` +
      `</div>` +
    `</div>`
  );
  sources.innerHTML = cards.join("");

  $("#claude-upstream-reload").disabled = claudeUpstreamBusy;

  let hint = claudeUpstream.file ? `供应商文件：${claudeUpstream.file}。` : "";
  if (claudeUpstream.stale_profile) {
    hint += ` 已保存的供应商「${claudeUpstream.stale_profile}」不在文件中，已自动回退到本机订阅。`;
  }
  $("#claude-upstream-hint").textContent = hint;

  // 供应商激活状态变化时,立即让「客户端配置」里的模型名跟随供应商映射。
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
  $("#claude-upstream-status").textContent = "切换中…";
  renderClaudeUpstream();
  try {
    await upstreamRequest("/api/claude-upstream/activate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    await loadClaudeUpstream();
    toast(name ? `已切换到 Claude 供应商「${name}」` : "已切换到本机 Anthropic 订阅");
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
  $("#claude-upstream-status").textContent = "重新加载中…";
  renderClaudeUpstream();
  try {
    await upstreamRequest("/api/claude-upstream/reload", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    await loadClaudeUpstream();
    toast("已重新加载 Claude 供应商");
  } catch (error) {
    setClaudeUpstreamError(error instanceof Error ? error.message : String(error));
    toast("重新加载 Claude 供应商失败", "err");
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
      `<div class="lan-note hint">同一 Wi-Fi 下的其他设备使用上面的地址；下方配置已包含密钥，可直接复制粘贴。若对方连不上，多半是公司 Wi-Fi 的客户端隔离，请改用 SSH 反向隧道。</div>`;
  } else {
    el.innerHTML =
      `<div class="lan-note hint">当前为本机模式，仅 127.0.0.1 可用。要让同一 Wi-Fi 下的其他设备直连，请用 <code>ferridex serve -lan</code> 启动。</div>`;
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
  let activeCount = 0;
  countdowns.forEach((c) => {
    const el = document.getElementById(c.id);
    if (!el) return;
    const span = el.querySelector(".health-countdown");
    const txt = fmtCountdown(c.resetMs - now);
    if (txt) activeCount++;
    if (span) {
      span.textContent = txt ? (c.prefix ? c.prefix + txt : txt) : "";
    }
  });
  if (activeCount === 0 && countdownTimer) {
    clearInterval(countdownTimer);
    countdownTimer = null;
  }
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

function fmtPct(frac) {
  const pct = frac * 100;
  if (Math.abs(pct - Math.round(pct)) < 0.05) return `${Math.round(pct)}%`;
  return `${pct.toFixed(1)}%`;
}

function fmtResetClock(resetsAt) {
  if (!resetsAt) return "";
  const d = new Date(resetsAt * 1000);
  const now = new Date();
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate();
  if (sameDay) return `今日 ${hh}:${mm}`;
  const tomorrow = new Date(now);
  tomorrow.setDate(now.getDate() + 1);
  const isTomorrow =
    d.getFullYear() === tomorrow.getFullYear() &&
    d.getMonth() === tomorrow.getMonth() &&
    d.getDate() === tomorrow.getDate();
  if (isTomorrow) return `明日 ${hh}:${mm}`;
  const mo = String(d.getMonth() + 1).padStart(2, "0");
  const da = String(d.getDate()).padStart(2, "0");
  return `${mo}-${da} ${hh}:${mm}`;
}

function fmtTimeClock(ms) {
  if (!ms) return "";
  const d = new Date(ms);
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}

function usageBar(u, detailed) {
  const frac = Math.max(0, Math.min(1, u.utilization || 0));
  const remFrac = Math.max(0, 1 - frac);
  const pctWidth = Math.round(frac * 1000) / 10;
  const resetIn = fmtResetIn(u.resets_at);
  const resetClock = fmtResetClock(u.resets_at);

  let paceMarker = "";
  let paceTag = "";
  if (detailed && u.window_seconds > 0 && u.resets_at > 0) {
    const remSec = u.resets_at - Date.now() / 1000;
    if (remSec > 0 && remSec <= u.window_seconds) {
      const elapsedFrac = Math.max(0, Math.min(1, 1 - remSec / u.window_seconds));
      const elapsedPct = Math.round(elapsedFrac * 100);
      paceMarker = `<span class="bar-pace" style="left:${elapsedPct}%" title="当前窗口时间已过 ${elapsedPct}%"></span>`;
      if (elapsedFrac >= 0.05 && elapsedFrac <= 0.98) {
        if (frac - elapsedFrac >= 0.15) {
          paceTag = `<span class="quota-chip warn" title="已用比例高于窗口时间进度">消耗偏快</span>`;
        } else if (frac <= elapsedFrac + 0.05) {
          paceTag = `<span class="quota-chip ok" title="已用比例低于或持平窗口时间进度">节奏健康</span>`;
        }
      }
    }
  }

  const resetLine =
    resetIn && resetClock && detailed
      ? `${resetIn} · ${resetClock}`
      : resetIn || resetClock;
  const modelsHTML =
    detailed && Array.isArray(u.models) && u.models.length
      ? `<div class="quota-models">${u.models.map((m) => `<span class="quota-model-tag">${escapeHTML(m)}</span>`).join("")}</div>`
      : "";

  return `
    <div class="usage-row${detailed ? " detailed" : ""}">
      <div class="usage-top">
        <span class="usage-label-wrap">
          <span class="usage-label">${escapeHTML(u.label || "")}</span>
          ${detailed && u.group ? `<span class="mini-tag">${escapeHTML(u.group)}</span>` : ""}
        </span>
        <span class="usage-pct">
          ${detailed ? `已用 <strong>${fmtPct(frac)}</strong> · 剩余 ${fmtPct(remFrac)}` : fmtPct(frac)}
        </span>
      </div>
      <div class="bar">
        <div class="bar-fill ${usageLevel(frac)}" style="width:${pctWidth}%"></div>
        ${paceMarker}
      </div>
      ${
        resetLine || u.detail || paceTag
          ? `<div class="usage-foot">
              <span class="usage-reset">${escapeHTML(resetLine)}</span>
              <span class="usage-foot-right">
                ${u.detail ? `<span class="usage-detail">${escapeHTML(u.detail)}</span>` : ""}
                ${paceTag}
              </span>
            </div>`
          : ""
      }
      ${modelsHTML}
    </div>`;
}

/* ===== 按需用量查询(点击才查,不自动轮询;镜像桌面端的弹层交互) ===== */
const usagePanels = {}; // name -> { open, loading, usage, report, queriedAt, error }

const usageChartSVG = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3v16a2 2 0 0 0 2 2h16"/><path d="M13 17V9"/><path d="M18 17V5"/><path d="M8 17v-3"/></svg>`;
const usageLoaderSVG = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>`;

function usagePopHTML(p, st) {
  let body;
  if (st.loading) {
    body = `<div class="usage-empty">查询中…</div>`;
  } else if (st.error) {
    body = `<div class="usage-err">${escapeHTML(st.error)}</div>`;
  } else if (st.usage && st.usage.length) {
    body = `<div class="usage">${st.usage.map((u) => usageBar(u, false)).join("")}</div>`;
  } else {
    body = `<div class="usage-empty">暂无用量数据</div>`;
  }
  return `
    <div class="usage-pop">
      <div class="usage-pop-head">
        <strong>${escapeHTML(p.title || p.name)} · 用量</strong>
        <button class="usage-close" type="button" data-usage-close="${escapeHTML(p.name)}" aria-label="关闭用量面板">×</button>
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
    st.report = d.report || null;
    st.queriedAt = d.queried_at || Date.now();
    st.error = d.error || "";
  } catch (e) {
    st.error = String(e);
  }
  st.loading = false;
  renderCards();
  renderUsageQuotas();
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
        const expanded = !collapsedCards.has(p.name);
        const st = usagePanels[p.name] || {};
        const usageBtn = p.supports_usage
          ? `<button class="usage-btn${st.loading ? " loading" : ""}${st.open ? " active" : ""}" type="button" data-usage="${safeName}" aria-label="查询 ${safeTitle} 用量" aria-pressed="${!!st.open}" ${p.logged_in ? "" : "disabled"}>${st.loading ? usageLoaderSVG : usageChartSVG}</button>`
          : "";
        return `
      <div class="card ${p.logged_in ? "" : "off"}${collapsedClass}" id="${escapeHTML(cardId)}">
        <div class="card-head">
          <button class="card-toggle" type="button" data-card-toggle="${safeName}" aria-expanded="${expanded}" aria-controls="${escapeHTML(cardId)}-body">
            <span class="caret" aria-hidden="true">▸</span>
            <span class="provider-brand" aria-hidden="true">${brandIconHTML(p.name)}</span>
            <span class="card-head-main">
              <span class="card-title">${safeTitle}</span>
              <span class="who">${safeName}</span>
            </span>
            <span class="card-head-status">
              ${dot(p.logged_in)}
              <strong>${p.logged_in ? "已登录" : "未登录"}</strong>
            </span>
          </button>
          ${usageBtn}
        </div>
        ${st.open ? usagePopHTML(p, st) : ""}
        <div class="card-body" id="${escapeHTML(cardId)}-body">
          ${p.account ? `<div class="acct">${escapeHTML(p.account)}</div>` : ""}
          ${claudeCounting ? `<div class="detail"><span class="health-countdown"></span></div>` : (p.detail ? `<div class="detail">${escapeHTML(p.detail)}</div>` : "")}
          <div class="meta">${(p.models || []).map((m) => `<span class="tag">${escapeHTML(m)}</span>`).join("")}</div>
        </div>
      </div>`;
      })
      .join("") || `<div class="empty">还没有检测到 Provider</div>`;

  syncCardsToggleAllButton();
  renderUsageQuotas();
  return claudeResetMs;
}

function syncCardsToggleAllButton() {
  const btn = $("#cards-toggle-all");
  if (!btn) return;
  const total = lastProviders.length;
  if (!total) {
    btn.hidden = true;
    return;
  }
  btn.hidden = false;
  const allCollapsed = lastProviders.every((p) => collapsedCards.has(p.name));
  btn.textContent = allCollapsed ? "全部展开" : "全部收起";
}

function syncAccessTabDot() {
  const dotEl = $("#tab-dot-access");
  if (dotEl) dotEl.hidden = !(sshTunnel.running || pub.running);
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
        const providerName = `<span class="ep-name">${brandIconHTML(p.name)}<span class="name">${escapeHTML(p.name)}</span></span>`;
        let text;
        if (p.name === "cursor") {
          text = `agent -e ${cursorBaseURL()} --auth-token ${authToken()}`;
        } else if (p.name === "agy") {
          text = `GET ${proxyBaseURL()}/agy/v1beta/models`;
        } else if (p.name === "grok") {
          text = `GROK_CLI_CHAT_PROXY_BASE_URL=${proxyBaseURL()}/grok/v1 (+ auth_provider_command)`;
        } else {
          text = `POST ${location.origin}${p.endpoint}`;
        }
        return `<div class="ep">${providerName}<code>${escapeHTML(text)}</code><button class="ghost-btn copy-btn" type="button" data-ep-copy="${escapeHTML(text)}" aria-label="复制 ${escapeHTML(p.name)} 端点">复制</button></div>`;
      })
      .join("");

    // 整体健康 + 主标签页状态徽章
    const provBadge = $("#tab-badge-providers");
    if (!providers.length) {
      setHealth("warn", "未检测到 Provider");
      if (provBadge) provBadge.hidden = true;
    } else {
      const onCount = providers.filter((p) => p.logged_in).length;
      if (onCount === providers.length) {
        setHealth("ok", `全部就绪 · ${onCount}/${providers.length} 已登录`, claudeResetMs);
      } else {
        setHealth("warn", `部分未登录 · ${onCount}/${providers.length}`, claudeResetMs);
      }
      if (provBadge) {
        provBadge.hidden = false;
        provBadge.textContent = `${onCount}/${providers.length}`;
        provBadge.className = "tab-badge " + (onCount === providers.length ? "ok" : "warn");
      }
    }

    if (claudeResetMs) {
      countdowns.push({ id: "healthbar", resetMs: claudeResetMs, prefix: " · " });
      if (!countdownTimer) {
        countdownTimer = setInterval(tickCountdown, 1000);
      }
      tickCountdown();
    } else if (countdownTimer && !countdowns.length) {
      clearInterval(countdownTimer);
      countdownTimer = null;
    }
  } catch (e) {
    lastProvidersOK = false;
    $("#cards").innerHTML = `<div class="empty">无法连接到 ferridex</div>`;
    setHealth("bad", "无法连接到 ferridex");
    const provBadge = $("#tab-badge-providers");
    if (provBadge) {
      provBadge.hidden = false;
      provBadge.textContent = "!";
      provBadge.className = "tab-badge bad";
    }
  }
}

/* ===== 实时日志:筛选 / 搜索 / 暂停 / 清空 / 计数 / 重连 ===== */
let logFilter = "all"; // all | 2 | 4 | 5
let logQuery = "";
let logPaused = false;
let logTotal = 0; // 本次会话累计
let pendingNew = 0; // 暂停期间新增条数
let unreadLogs = 0; // 停留在其他标签页时收到的新日志数
let unreadLogErr = false;
const MAX_LOG = 300;

function statusClass(n) {
  if (n >= 500) return "s5";
  if (n >= 400) return "s4";
  if (n >= 200 && n < 300) return "s2";
  if (n >= 300) return "so";
  return "s4";
}

function logLineHTML(o) {
  return `<span class="t">${escapeHTML(o.time)}</span><span class="m">${escapeHTML(o.method)}</span><span class="p" title="${escapeHTML(o.path)}">${escapeHTML(o.path)}</span><span class="s ${statusClass(o.status)}">${escapeHTML(o.status)}</span><span class="d">${escapeHTML(o.dur)}</span>`;
}

function logMatches(line) {
  const n = +line.dataset.status;
  if (logFilter !== "all" && Math.floor(n / 100) !== +logFilter) return false;
  if (logQuery && !(line.dataset.search || "").includes(logQuery)) return false;
  return true;
}

function updateLogCount(displayed) {
  $("#log-count").textContent = `${logTotal} / ${displayed}`;
}

function syncLogTabBadge() {
  const badge = $("#tab-badge-logs");
  if (!badge) return;
  if (unreadLogs <= 0) {
    badge.hidden = true;
    return;
  }
  badge.hidden = false;
  badge.textContent = unreadLogs > 99 ? "99+" : String(unreadLogs);
  badge.className = "tab-badge " + (unreadLogErr ? "bad" : "ok");
}

function applyFilter() {
  let shown = 0;
  $$("#log .logline").forEach((line) => {
    const match = logMatches(line);
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
      line.dataset.search = `${o.method || ""} ${o.path || ""} ${o.status || ""}`.toLowerCase();
      line.innerHTML = logLineHTML(o);
      const match = logMatches(line);
      line.style.display = match ? "" : "none";
      box.appendChild(line);

      while (box.childElementCount > MAX_LOG) box.firstElementChild.remove();

      if (consoleTab !== "logs") {
        unreadLogs++;
        if (+o.status >= 400) unreadLogErr = true;
        syncLogTabBadge();
      }

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

  // 关键词搜索
  const searchInput = $("#log-search");
  if (searchInput) {
    searchInput.addEventListener("input", () => {
      logQuery = searchInput.value.trim().toLowerCase();
      applyFilter();
    });
  }

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
    box.innerHTML = `<div class="empty">已清空显示，后端缓冲不受影响</div>`;
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

    badge.className = "badge " + (st.running ? "badge-on" : st.last_error ? "badge-err" : "badge-off");
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

    badge.className = "badge " + (pub.running ? "badge-on" : st.last_error ? "badge-err" : "badge-off");
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
  syncAccessTabDot();
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

let hubCopyTimer = null;

async function copyHubLaunch() {
  // 路由自动跟随公网状态,与「客户端」页的目标选择器无关。
  const usePublic = pub.running && !!pub.url;
  const btn = $("#hub-copy-launch");
  if (hubCopyTimer) clearTimeout(hubCopyTimer);
  try {
    const cmd = withRoutedLaunch(usePublic ? "public" : "local", () =>
      $(HUB_COPY_TARGETS[hubTab] || HUB_COPY_TARGETS.claude).textContent
    );
    await copyText(cmd);
    btn.textContent = "已复制";
    btn.classList.add("copied");
    toast(usePublic ? "已复制公网启动命令" : "已复制本机启动命令");
  } catch {
    btn.textContent = "复制失败";
    btn.classList.remove("copied");
    toast("复制失败", "err");
  }
  hubCopyTimer = setTimeout(() => {
    btn.textContent = HUB_COPY_LABELS[hubTab];
    btn.classList.remove("copied");
    hubCopyTimer = null;
  }, 1600);
}

/* ===== 主标签页:上游 / 用量监控 / 响应速度 / Provider 状态 / 远程访问 / 客户端 / 日志 ===== */
const CONSOLE_TABS = ["upstream", "usage", "speed", "providers", "access", "clients", "logs"];

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
    btn.tabIndex = on ? 0 : -1;
  });
  $$(".console-pane").forEach((pane) => {
    pane.hidden = pane.dataset.consolePane !== consoleTab;
  });
  if (consoleTab === "logs") {
    unreadLogs = 0;
    unreadLogErr = false;
    syncLogTabBadge();
  }
  try { localStorage.setItem("ferridex.consoleTab", consoleTab); } catch {}
}

function startTablistKeyboard(container, selector, dataKey, activate) {
  container.addEventListener("keydown", (event) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    const tabs = Array.from(container.querySelectorAll(selector));
    if (!tabs.length) return;
    const current = tabs.indexOf(event.target.closest(selector));
    if (current < 0) return;
    event.preventDefault();
    let next = current;
    if (event.key === "Home") next = 0;
    else if (event.key === "End") next = tabs.length - 1;
    else if (event.key === "ArrowRight") next = (current + 1) % tabs.length;
    else next = (current - 1 + tabs.length) % tabs.length;
    const button = tabs[next];
    activate(button.dataset[dataKey]);
    button.focus();
  });
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
startTablistKeyboard($(".console-tabs"), ".console-tab", "consoleTab", setConsoleTab);
startTablistKeyboard($(".hub-tabs"), ".hub-tab", "hubTab", setHubTab);

// 上游页快捷操作:一键复制(路由跟随公网状态) + 公网隧道就地启停。
$("#hub-copy-launch").addEventListener("click", copyHubLaunch);
$("#hub-public-toggle").addEventListener("click", toggleHubPublic);

$("#tun-start").addEventListener("click", () =>
  tunnelAction("/api/tunnel/start", { remote: $("#tun-remote").value, key: $("#tun-key").value })
);
$("#tun-stop").addEventListener("click", () => tunnelAction("/api/tunnel/stop", {}));

// Claude 上游供应商:委托绑在常驻容器上,5 秒重渲染后依旧生效;点击当前已激活
// 的卡片不重复发请求。
$("#claude-upstream-reload").addEventListener("click", reloadClaudeProfiles);
function activateClaudeCard(card) {
  if (!card || claudeUpstreamBusy || card.getAttribute("aria-disabled") === "true") return;
  const name = card.dataset.claudeUpstream;
  if ((name || "") === (claudeUpstream.active || "")) return;
  activateClaudeUpstream(name);
}
$("#claude-upstream-sources").addEventListener("click", (e) => {
  activateClaudeCard(e.target.closest("[data-claude-upstream]"));
});
$("#claude-upstream-sources").addEventListener("keydown", (e) => {
  if (e.key !== "Enter" && e.key !== " ") return;
  const card = e.target.closest("[data-claude-upstream]");
  if (!card) return;
  e.preventDefault();
  activateClaudeCard(card);
});

// Codex 使用显式按钮,避免在可点击卡片中嵌套「测试」按钮。
$("#codex-upstream-reload").addEventListener("click", reloadCodexProfiles);
$("#codex-upstream-sources").addEventListener("click", (event) => {
  const testButton = event.target.closest("[data-codex-test]");
  if (testButton) {
    testResponsesUpstream(testButton.dataset.codexTest);
    return;
  }
  const activateButton = event.target.closest("[data-codex-activate]");
  if (activateButton) activateResponsesSource(activateButton.dataset.codexActivate);
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
  syncAccessTabDot();
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
  const toggle = e.target.closest("[data-card-toggle]");
  if (!toggle) return;
  const card = toggle.closest(".card");
  if (!card) return;
  const name = toggle.dataset.cardToggle;
  const collapsed = card.classList.toggle("collapsed");
  toggle.setAttribute("aria-expanded", String(!collapsed));
  if (collapsed) collapsedCards.add(name);
  else collapsedCards.delete(name);
  syncCardsToggleAllButton();
});

// 顶栏健康药丸点击跳转至 Provider 状态页
const healthBar = $("#healthbar");
if (healthBar) {
  healthBar.addEventListener("click", () => setConsoleTab("providers"));
  healthBar.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      setConsoleTab("providers");
    }
  });
}

// Provider 卡片一键全部展开/收起
const cardsToggleAll = $("#cards-toggle-all");
if (cardsToggleAll) {
  cardsToggleAll.addEventListener("click", () => {
    const allCollapsed = lastProviders.every((p) => collapsedCards.has(p.name));
    if (allCollapsed) collapsedCards.clear();
    else lastProviders.forEach((p) => collapsedCards.add(p.name));
    renderCards();
  });
}

// 点击外部或按 Escape 关闭打开的用量弹层
function closeAllUsagePopovers() {
  let changed = false;
  Object.keys(usagePanels).forEach((name) => {
    if (usagePanels[name] && usagePanels[name].open) {
      usagePanels[name].open = false;
      changed = true;
    }
  });
  if (changed) renderCards();
}
document.addEventListener("click", (e) => {
  if (e.target.closest(".usage-pop") || e.target.closest("[data-usage]")) return;
  closeAllUsagePopovers();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeAllUsagePopovers();
});

// 接入端点一键复制(事件委托,兼容 5s 重渲染)
const endpointsBox = $("#endpoints");
if (endpointsBox) {
  endpointsBox.addEventListener("click", async (e) => {
    const btn = e.target.closest("[data-ep-copy]");
    if (!btn) return;
    const text = btn.dataset.epCopy;
    if (!text) return;
    const original = btn.textContent;
    try {
      await copyText(text);
      btn.textContent = "已复制";
      btn.classList.add("copied");
      toast("已复制接入端点");
    } catch {
      btn.textContent = "复制失败";
      toast("复制失败", "err");
    }
    setTimeout(() => {
      btn.textContent = original;
      btn.classList.remove("copied");
    }, 1400);
  });
}

// 客户端页按客户端筛选
const clientFilterBar = $("#client-filter");
if (clientFilterBar) {
  clientFilterBar.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-client-filter]");
    if (!btn) return;
    const target = btn.dataset.clientFilter;
    $$("#client-filter .filter-pill").forEach((pill) => {
      pill.classList.toggle("active", pill === btn);
    });
    $$(".client-group").forEach((group) => {
      group.hidden = target !== "all" && group.dataset.clientGroup !== target;
    });
  });
}

/* ===== 用量监控 & 响应速度 (/api/metrics) ===== */
let metricsWindow = "24h";
let usageGroupBy = "model"; // model | provider
let speedProviderFilter = "all";
let metricsData = {
  window: "24h",
  summary: {},
  by_provider: [],
  by_model: [],
  hourly: [],
  recent: [],
};

function fmtTokens(n) {
  const v = Number(n || 0);
  if (v >= 1e9) return (v / 1e9).toFixed(2) + "B";
  if (v >= 1e6) return (v / 1e6).toFixed(2) + "M";
  if (v >= 1e3) return (v / 1e3).toFixed(1) + "K";
  return String(Math.round(v));
}

function fmtMs(ms) {
  const v = Number(ms || 0);
  if (v <= 0) return "—";
  if (v >= 10000) return (v / 1000).toFixed(1) + "s";
  if (v >= 1000) return (v / 1000).toFixed(2) + "s";
  return Math.round(v) + "ms";
}

function ttftClass(ms) {
  if (!ms || ms <= 0) return "na";
  if (ms < 800) return "fast";
  if (ms < 2500) return "mid";
  return "slow";
}

function tpsBarClass(tps) {
  if (tps >= 50) return "";
  if (tps >= 20) return "mid";
  return "slow";
}

function syncMetricsWindowButtons() {
  $$("[data-metrics-window]").forEach((btn) => {
    btn.classList.toggle("active", btn.dataset.metricsWindow === metricsWindow);
  });
}

function renderUsageQuotas() {
  const wrap = $("#usage-quotas");
  const allBtn = $("#quota-query-all");
  if (!wrap) return;
  // 仅展示已登录本机订阅的 Provider；未登录的不在此处显示。
  const list = lastProviders.filter((p) => p.supports_usage && p.logged_in);
  const anyLoading = list.some((p) => (usagePanels[p.name] || {}).loading);
  if (allBtn) {
    allBtn.hidden = !list.length;
    allBtn.disabled = !list.length || anyLoading;
    allBtn.textContent = anyLoading ? "查询中…" : "查询全部配额";
  }
  if (!list.length) {
    wrap.innerHTML = `<div class="empty">当前没有已登录且支持配额查询的本机订阅</div>`;
    return;
  }
  wrap.innerHTML = list
    .map((p) => {
      const safeName = escapeHTML(p.name);
      const safeTitle = escapeHTML(p.title || p.name);
      const st = usagePanels[p.name] || {};
      const rep = st.report || null;
      const acct = (rep && rep.account) || p.account || "";
      const planBadge =
        rep && rep.plan
          ? `<span class="quota-chip plan">${escapeHTML(rep.plan)}</span>`
          : "";
      const statusTone =
        rep && rep.status
          ? rep.status.includes("已达")
            ? "bad"
            : rep.status.includes("部分")
              ? "warn"
              : "ok"
          : "";
      const statusBadge =
        rep && rep.status
          ? `<span class="quota-chip ${statusTone}">${escapeHTML(rep.status)}</span>`
          : "";
      const updatedText = st.queriedAt && !st.loading ? `${fmtTimeClock(st.queriedAt)} 更新` : "";

      let body;
      if (st.loading) {
        body = `<div class="usage-empty">正在查询上游配额详情…</div>`;
      } else if (st.error) {
        body = `<div class="usage-err">${escapeHTML(st.error)}</div>`;
      } else if (st.usage && st.usage.length) {
        const extrasHTML =
          rep && Array.isArray(rep.extras) && rep.extras.length
            ? `<div class="quota-extras">${rep.extras
                .map(
                  (ex) =>
                    `<span class="quota-extra ${escapeHTML(ex.tone || "")}"><em>${escapeHTML(ex.label)}</em><strong>${escapeHTML(ex.value)}</strong></span>`
                )
                .join("")}</div>`
            : "";
        body = `<div class="usage">${st.usage.map((u) => usageBar(u, true)).join("")}</div>${extrasHTML}`;
      } else {
        body = `<div class="usage-empty">点击「查询配额」获取订阅套餐、剩余比例、重置时刻与模型共享池详情</div>`;
      }

      const metaLine = [acct, updatedText].filter(Boolean).join(" · ");
      return `
        <div class="quota-card">
          <div class="quota-head">
            <div class="quota-title-wrap">
              <div class="quota-title-row">
                <span class="quota-title">${brandIconHTML(p.name)}${safeTitle}</span>
                ${planBadge}
                ${statusBadge}
              </div>
              ${metaLine ? `<span class="quota-acct" title="${escapeHTML(metaLine)}">${escapeHTML(metaLine)}</span>` : ""}
            </div>
            <button class="ghost-btn" type="button" data-quota-query="${safeName}" ${st.loading ? "disabled" : ""}>${st.loading ? "查询中…" : st.usage && st.usage.length ? "刷新" : "查询配额"}</button>
          </div>
          ${body}
        </div>`;
    })
    .join("");
}

function renderUsageMonitor() {
  const s = metricsData.summary || {};
  const kpiWrap = $("#usage-kpis");
  if (kpiWrap) {
    kpiWrap.innerHTML = [
      {
        label: "总 Token 消耗",
        value: fmtTokens(s.total_tokens),
        cls: "accent",
        sub: `输入 ${fmtTokens(s.input_tokens)} · 输出 ${fmtTokens(s.output_tokens)}`,
      },
      {
        label: "缓存命中率",
        value: s.requests ? `${Number(s.cache_hit_rate || 0).toFixed(1)}%` : "—",
        cls: "ok",
        sub: `读取 ${fmtTokens(s.cache_read_tokens)} · 写入 ${fmtTokens(s.cache_write_tokens)}`,
      },
      {
        label: "对话请求数",
        value: String(s.requests || 0),
        cls: "",
        sub: s.requests ? `成功率 ${Number(s.success_rate || 0).toFixed(1)}% (${s.success_requests}/${s.requests})` : "暂无记录",
      },
      {
        label: "活跃模型 / Provider",
        value: `${s.active_models || 0} / ${s.active_providers || 0}`,
        cls: "",
        sub: `统计窗口：${metricsWindow === "today" ? "今日" : metricsWindow === "24h" ? "近 24 小时" : metricsWindow === "7d" ? "近 7 天" : "全部"}`,
      },
    ]
      .map(
        (k) => `
        <div class="kpi-card">
          <span class="kpi-label">${k.label}</span>
          <strong class="kpi-value ${k.cls}">${escapeHTML(k.value)}</strong>
          <span class="kpi-sub" title="${escapeHTML(k.sub)}">${escapeHTML(k.sub)}</span>
        </div>`
      )
      .join("");
  }

  // 24h 柱状图
  const hourlyWrap = $("#usage-hourly");
  if (hourlyWrap) {
    const buckets = Array.isArray(metricsData.hourly) ? metricsData.hourly : [];
    const maxTok = Math.max(1, ...buckets.map((b) => b.total_tokens || 0));
    hourlyWrap.innerHTML = buckets
      .map((b, idx) => {
        const total = b.total_tokens || 0;
        const inPct = total ? Math.round(((b.input_tokens || 0) / maxTok) * 100) : 0;
        const outPct = total ? Math.max(total > 0 ? 3 : 0, Math.round(((b.output_tokens || 0) / maxTok) * 100)) : 0;
        const showLbl = idx % 4 === 0 || idx === buckets.length - 1;
        const tip = `${b.hour} · ${b.requests || 0} 次请求 · 总计 ${fmtTokens(total)}（输入/缓存 ${fmtTokens(b.input_tokens)}，输出 ${fmtTokens(b.output_tokens)}）`;
        return `
          <div class="hourly-col" title="${escapeHTML(tip)}">
            <div class="hourly-bar-wrap">
              <div class="hourly-bar-out" style="height:${Math.min(100, outPct)}%"></div>
              <div class="hourly-bar-in" style="height:${Math.min(100 - Math.min(100, outPct), inPct)}%"></div>
            </div>
            <span class="hourly-lbl">${showLbl ? escapeHTML(b.hour.slice(0, 2)) : ""}</span>
          </div>`;
      })
      .join("");
  }

  // 明细表
  const bdWrap = $("#usage-breakdown");
  if (bdWrap) {
    const rows = usageGroupBy === "provider" ? metricsData.by_provider || [] : metricsData.by_model || [];
    const grandTotal = Math.max(1, Number(s.total_tokens || 0));
    if (!rows.length) {
      bdWrap.innerHTML = `<div class="empty">当前时间窗口内暂无对话 Token 记录，通过代理发起一次对话后即可在此查看统计</div>`;
    } else {
      const headLabel = usageGroupBy === "provider" ? "Provider" : "模型";
      bdWrap.innerHTML = `
        <table class="metrics-table">
          <thead>
            <tr>
              <th>${headLabel}</th>
              <th class="num">请求数</th>
              <th class="num">输入 Token</th>
              <th class="num">输出 Token</th>
              <th class="num">缓存读取</th>
              <th class="num">总计 / 占比</th>
              <th class="num">成功率</th>
            </tr>
          </thead>
          <tbody>
            ${rows
              .map((r) => {
                const share = Math.round(((r.total_tokens || 0) * 100) / grandTotal);
                const firstCell =
                  usageGroupBy === "provider"
                    ? `<span class="cell-provider">${brandIconHTML(r.provider)}<strong>${escapeHTML(r.provider)}</strong></span>`
                    : `<span class="cell-provider">${brandIconHTML(r.provider)}<span class="cell-model-name" title="${escapeHTML(r.model)}">${escapeHTML(r.model)}</span></span>`;
                return `
                  <tr>
                    <td>${firstCell}</td>
                    <td class="num">${r.requests || 0}</td>
                    <td class="num">${fmtTokens(r.input_tokens)}</td>
                    <td class="num">${fmtTokens(r.output_tokens)}</td>
                    <td class="num">${fmtTokens(r.cache_read_tokens)}</td>
                    <td class="num">${fmtTokens(r.total_tokens)}<span class="share-bar" title="${share}%"><span class="share-fill" style="width:${share}%"></span></span></td>
                    <td class="num">${Number(r.success_rate || 0).toFixed(1)}%</td>
                  </tr>`;
              })
              .join("")}
          </tbody>
        </table>`;
    }
  }
}

function renderSpeedMonitor() {
  const s = metricsData.summary || {};
  const badge = $("#tab-badge-speed");
  if (badge) {
    if (s.speed_samples > 0 && s.avg_tps > 0) {
      badge.hidden = false;
      badge.textContent = `${Number(s.avg_tps).toFixed(0)}t/s`;
      badge.className = "tab-badge " + (s.avg_tps >= 40 ? "ok" : "warn");
    } else {
      badge.hidden = true;
    }
  }

  const kpiWrap = $("#speed-kpis");
  if (kpiWrap) {
    kpiWrap.innerHTML = [
      {
        label: "平均生成速度",
        value: s.avg_tps > 0 ? `${Number(s.avg_tps).toFixed(1)} tok/s` : "—",
        cls: "ok",
        sub: s.speed_samples > 0 ? `峰值 ${Number(s.max_tps).toFixed(1)} · 最低 ${Number(s.min_tps).toFixed(1)} tok/s` : "等待对话样本",
      },
      {
        label: "中位首字延迟 (P50)",
        value: fmtMs(s.median_ttft_ms),
        cls: "accent",
        sub: s.p95_ttft_ms > 0 ? `P95 首字延迟 ${fmtMs(s.p95_ttft_ms)}` : "TTFT 越小响应越快",
      },
      {
        label: "平均首字延迟 (Avg)",
        value: fmtMs(s.avg_ttft_ms),
        cls: s.avg_ttft_ms >= 2500 ? "warn" : "",
        sub: "从发出请求到首个内容 Token",
      },
      {
        label: "有效测速对话",
        value: String(s.speed_samples || 0),
        cls: "",
        sub: `共记录 ${s.requests || 0} 次对话请求`,
      },
    ]
      .map(
        (k) => `
        <div class="kpi-card">
          <span class="kpi-label">${k.label}</span>
          <strong class="kpi-value ${k.cls}">${escapeHTML(k.value)}</strong>
          <span class="kpi-sub" title="${escapeHTML(k.sub)}">${escapeHTML(k.sub)}</span>
        </div>`
      )
      .join("");
  }

  // 模型平均速度对比条
  const barsWrap = $("#speed-model-bars");
  if (barsWrap) {
    const models = (metricsData.by_model || [])
      .filter((m) => m.avg_tps > 0 || m.median_ttft_ms > 0)
      .slice()
      .sort((a, b) => (b.avg_tps || 0) - (a.avg_tps || 0));
    if (!models.length) {
      barsWrap.innerHTML = `<div class="empty">暂无模型测速数据</div>`;
    } else {
      const maxTps = Math.max(1, ...models.map((m) => m.avg_tps || 0));
      barsWrap.innerHTML = models
        .map((m) => {
          const pct = m.avg_tps > 0 ? Math.max(6, Math.round((m.avg_tps / maxTps) * 100)) : 0;
          return `
            <div class="speed-bar-row">
              <div class="speed-bar-name" title="${escapeHTML(m.model)}">${brandIconHTML(m.provider)}<span>${escapeHTML(m.model)}</span></div>
              <div class="speed-bar-track"><div class="speed-bar-fill" style="width:${pct}%"></div></div>
              <div class="speed-bar-meta"><strong>${m.avg_tps > 0 ? Number(m.avg_tps).toFixed(1) + " tok/s" : "—"}</strong> · TTFT ${fmtMs(m.median_ttft_ms)} (${m.requests}次)</div>
            </div>`;
        })
        .join("");
    }
  }

  // 每次对话明细表
  const recWrap = $("#speed-records");
  if (recWrap) {
    const allRecent = Array.isArray(metricsData.recent) ? metricsData.recent : [];
    const list =
      speedProviderFilter === "all"
        ? allRecent
        : allRecent.filter((r) => r.provider === speedProviderFilter);
    if (!list.length) {
      recWrap.innerHTML = `<div class="empty">暂无符合条件的对话速度记录，向任意已接入客户端发送一条消息后即可实时看到 TTFT 与 tok/s</div>`;
    } else {
      const maxRowTps = Math.max(10, ...list.map((r) => r.tokens_per_sec || 0));
      recWrap.innerHTML = `
        <table class="metrics-table">
          <thead>
            <tr>
              <th>时间</th>
              <th>Provider / 上游</th>
              <th>模型</th>
              <th class="num">首字延迟 (TTFT)</th>
              <th class="num">生成速度</th>
              <th class="num">输出 / 输入</th>
              <th class="num">总耗时</th>
              <th class="num">状态</th>
            </tr>
          </thead>
          <tbody>
            ${list
              .map((r) => {
                const tps = Number(r.tokens_per_sec || 0);
                const tpsPct = tps > 0 ? Math.max(8, Math.min(100, Math.round((tps / maxRowTps) * 100))) : 0;
                const totalMs = Math.max(1, Number(r.total_ms || 0));
                const ttftPct = Math.min(100, Math.round((Number(r.ttft_ms || 0) * 100) / totalMs));
                const decPct = Math.max(0, 100 - ttftPct);
                const stCls = statusClass(r.status);
                const stBadge = `<span class="speed-pill ${stCls === "s2" ? "fast" : stCls === "s4" ? "mid" : "slow"}">${escapeHTML(r.status)}</span>`;
                return `
                  <tr>
                    <td class="mono">${escapeHTML(r.time || "")}</td>
                    <td>
                      <span class="cell-provider">
                        ${brandIconHTML(r.provider)}
                        <span>${escapeHTML(r.provider || "")}</span>
                        ${r.upstream ? `<span class="mini-tag">${escapeHTML(r.upstream)}</span>` : ""}
                      </span>
                    </td>
                    <td>
                      <span class="cell-model">
                        <span class="cell-model-name" title="${escapeHTML(r.model || r.path)}">${escapeHTML(r.model || r.path)}</span>
                        <span class="mini-tag${r.stream ? " sse" : ""}">${r.stream ? "SSE" : "JSON"}</span>
                      </span>
                    </td>
                    <td class="num"><span class="speed-pill ${ttftClass(r.ttft_ms)}">${fmtMs(r.ttft_ms)}</span></td>
                    <td class="num">
                      ${
                        tps > 0
                          ? `<span class="tps-cell"><strong>${tps.toFixed(1)}</strong><span class="mini-tag">tok/s</span><span class="tps-track"><span class="tps-fill ${tpsBarClass(tps)}" style="width:${tpsPct}%"></span></span></span>`
                          : `<span class="speed-pill na">—</span>`
                      }
                    </td>
                    <td class="num">${fmtTokens(r.output_tokens)} / ${fmtTokens(r.input_tokens)}</td>
                    <td class="num">
                      <div>${fmtMs(r.total_ms)}</div>
                      <div class="dur-bar" title="TTFT ${fmtMs(r.ttft_ms)} / 解码 ${fmtMs(r.decode_ms)}"><span class="dur-ttft" style="width:${ttftPct}%"></span><span class="dur-decode" style="width:${decPct}%"></span></div>
                    </td>
                    <td class="num">${stBadge}</td>
                  </tr>`;
              })
              .join("")}
          </tbody>
        </table>`;
    }
  }
}

async function loadMetrics() {
  try {
    const r = await fetch(`/api/metrics?window=${encodeURIComponent(metricsWindow)}`);
    if (!r.ok) return;
    metricsData = await r.json();
    renderUsageMonitor();
    renderSpeedMonitor();
  } catch {}
}

// 用量监控 / 响应速度交互绑定
$$("[data-metrics-window]").forEach((btn) => {
  btn.addEventListener("click", () => {
    metricsWindow = btn.dataset.metricsWindow || "24h";
    syncMetricsWindowButtons();
    loadMetrics();
  });
});

const usageGroupToggle = $("#usage-group-toggle");
if (usageGroupToggle) {
  usageGroupToggle.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-usage-group]");
    if (!btn) return;
    usageGroupBy = btn.dataset.usageGroup === "provider" ? "provider" : "model";
    $$("#usage-group-toggle .seg-btn").forEach((b) => b.classList.toggle("active", b === btn));
    renderUsageMonitor();
  });
}

const speedFilterBar = $("#speed-provider-filter");
if (speedFilterBar) {
  speedFilterBar.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-speed-provider]");
    if (!btn) return;
    speedProviderFilter = btn.dataset.speedProvider || "all";
    $$("#speed-provider-filter .filter-pill").forEach((p) => p.classList.toggle("active", p === btn));
    renderSpeedMonitor();
  });
}

const metricsClearBtn = $("#metrics-clear");
if (metricsClearBtn) {
  metricsClearBtn.addEventListener("click", async () => {
    if (!window.confirm("确定要清空本地保存的用量与测速历史记录吗？")) return;
    try {
      const r = await fetch("/api/metrics/clear", { method: "POST" });
      const d = await r.json();
      if (d.ok === false) throw new Error(d.error || "清空失败");
      toast("已清空用量与测速统计");
      loadMetrics();
    } catch (e) {
      toast("清空失败：" + (e.message || e), "err");
    }
  });
}

const usageQuotasBox = $("#usage-quotas");
if (usageQuotasBox) {
  usageQuotasBox.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-quota-query]");
    if (!btn) return;
    queryUsage(btn.dataset.quotaQuery);
  });
}

const quotaQueryAllBtn = $("#quota-query-all");
if (quotaQueryAllBtn) {
  quotaQueryAllBtn.addEventListener("click", () => {
    const queryable = lastProviders.filter((p) => p.supports_usage && p.logged_in);
    queryable.forEach((p) => queryUsage(p.name));
  });
}

$("#log").innerHTML = `<div class="empty">暂无请求日志，向 /v1/responses、/v1/messages、/agy 或 /grok 发一次请求即可看到</div>`;
updateLogCount(0);

loadNetinfo();
loadResponsesUpstream(false);
loadClaudeUpstream();
startCopyButtons();
startAccordions();
loadStatus();
loadTunnel();
loadPublic();
loadMetrics();
setInterval(() => {
  loadStatus();
  loadTunnel();
  loadPublic();
  loadResponsesUpstream(false);
  loadClaudeUpstream();
  loadMetrics();
}, 5000);
startLogs();
