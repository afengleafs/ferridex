# ferridex

[English](README.md) | **中文**

`ferridex` 是一个运行在本机的 Go 代理，复用已登录的 ChatGPT/Codex、Claude Code、Cursor CLI 与 Grok CLI 订阅，并向本机或通过 SSH 隧道连接的远程主机提供兼容接口。

> 本项目调用非公开上游端点并读取本机登录凭据，仅适合个人研究和自用。
> 不要共享或转售代理。需要从公网连接时，请用面板的 ngrok 隧道（强制密钥，且不暴露面板），
> 程序会拒绝不带 `-lan` 的非回环监听；不要绕过这项保护把服务裸奔到公网。

## 环境与构建

- Go 1.23 或更高版本
- 已完成订阅登录的 Codex CLI、Claude Code、Cursor CLI 和/或 Grok CLI（按需）
- 使用面板管理反向隧道时，需要本机 `ssh` 与免密密钥（或 `ssh-agent`）

```bash
git clone https://github.com/afengleafs/ferridex.git
cd ferridex
mkdir -p ~/.local/bin
go build -o ~/.local/bin/ferridex .
export PATH="$HOME/.local/bin:$PATH"
```

源码更新后需重新构建并重启进程；仅 `go build ./...` 不会替换 `PATH` 中的二进制。

## 启动

```bash
# 同时启动 Codex、Claude、Cursor、Grok 和 Web 面板，并自动打开浏览器
ferridex serve

# 指定端口 / 不自动打开浏览器 / 端口占用时自动顺延
ferridex serve -addr 127.0.0.1:8789
ferridex serve -no-open
ferridex serve -auto-port

# 向同一局域网开放 AI 接口（自动密钥，面板仍限本机）
ferridex serve -lan

# 只启动单个代理，或不带面板
ferridex codex
ferridex claude
ferridex cursor
ferridex grok

# 检查本机登录状态
ferridex status
```

默认监听 `127.0.0.1:8788`，面板地址：`http://127.0.0.1:8788/`。
程序会拒绝 `-addr 0.0.0.0:...` 等非回环地址；局域网请用 `ferridex serve -lan`。

## Responses 自定义上游

网页面板的「Responses 上游」可以在两种来源之间热切换，保存或切换后无需重启 ferridex：

- **本机订阅（subscription）**：继续复用本机已登录的 ChatGPT/Codex 订阅。
- **自定义上游（custom）**：填写 Provider 名称、API 根地址（Base URL）、API Key 和默认模型，
  转发兼容 OpenAI Responses API 的企业服务。

例如接入企业 VOD 时，在面板中填写：

```text
Provider 名称: VOD GPT
Base URL:       https://text-aigc.vod-qcloud.com/v1
API Key:        <VOD_API_TOKEN>
默认模型:       gpt-5.6-sol
```

> 如果真实 VOD Token 曾被粘贴到聊天、Issue、日志或版本库中，请先在企业平台吊销并轮换，
> 再把新 Token 填入面板。文档和配置示例中不要记录真实 Token。

真实上游 API Key 只保存在本机 `~/.ferridex/config.json`（文件权限 `0600`），不会返回给网页，
也不会写进客户端配置。Codex 客户端仍然只连接 ferridex，并通过 `LOCAL_PROXY_KEY` 提交
ferridex 的**下游密钥**；不要把企业上游 API Key 设置为 `LOCAL_PROXY_KEY`：

```toml
model = "gpt-5.6-sol"
model_provider = "ferridex"

[model_providers.ferridex]
name = "Ferridex Responses Proxy"
base_url = "http://127.0.0.1:8788/v1"
env_key = "LOCAL_PROXY_KEY"
wire_api = "responses"
```

本机、LAN、SSH 和 ngrok 接入都会跟随当前选中的 Responses 上游；切回本机订阅时客户端配置
无需改动。SSH 与 ngrok 隧道都只连接专用的 relay-only 端口并强制校验 ferridex 下游密钥，
不会暴露管理面板或真实上游 Key。

当前只提供一个自定义 Responses 上游，不支持多 profile、自动故障转移或其他协议转换；也不提供
裸公网监听。公网访问请使用下方的 ngrok 方案，不要把 ferridex 直接监听到 `0.0.0.0`。

## Claude 上游档案

网页「上游切换」面板的 Claude 页可以在**本机 Anthropic 订阅**与多个**自定义 Anthropic 兼容上游**
（OpenRouter 等网关）之间点击卡片热切换（仿 cc-switch：Claude / Codex 标签页 + 全宽 provider 卡），
本地 Claude Code 无需任何改动、无需重启：

- 档案定义在 ferridex **运行目录**下的 `ferridex-profiles.env`（首次启动会自动生成带注释的
  模板；文件权限 `0600`）。语法：`[档案名]` 分段，段内每行一个 `KEY=VALUE`，支持 `export`
  前缀、单/双引号和 `#` 注释——可以直接粘贴 Claude Code 的环境变量写法：

  ```ini
  [openrouter]
  ANTHROPIC_BASE_URL="https://openrouter.ai/api"
  ANTHROPIC_AUTH_TOKEN="sk-or-v1-..."
  ANTHROPIC_API_KEY=""
  ANTHROPIC_DEFAULT_SONNET_MODEL="stealth/ox-alpha"
  ANTHROPIC_DEFAULT_OPUS_MODEL="stealth/ox-alpha"
  ANTHROPIC_DEFAULT_HAIKU_MODEL="stealth/ox-alpha"
  CLAUDE_CODE_SUBAGENT_MODEL="stealth/ox-alpha"
  ```

- **模型映射**：请求模型名含 `opus` / `sonnet` / `haiku`（大小写不敏感）时改写为档案对应字段；
  未命中或未配置的档位原样透传，不会跨档位静默改写。`CLAUDE_CODE_SUBAGENT_MODEL` 仅是
  客户端本地变量，ferridex 接受但不消费。
- **凭据**：`ANTHROPIC_AUTH_TOKEN` → `Authorization: Bearer`；`ANTHROPIC_API_KEY` →
  `x-api-key`（两者都设时 Bearer 优先）。`BASE_URL` 填 API 根地址，结尾带不带 `/v1` 都可以，
  ferridex 统一转发到 `<根地址>/v1/messages`。
- **热切换**：「上游切换」面板点击卡片即生效并持久化到 `~/.ferridex/config.json`
  （`claude_upstream` 字段）；重启后自动恢复。编辑文件后点「重新加载档案」重新读取；档案被删掉时
  自动回退本机订阅并在面板标注，加回文件再点「重新加载」即可恢复。
- **范围**：只影响 `/v1/messages`（Claude Code 主链路）；订阅路径的 OAuth 刷新、配额熔断、
  Claude Code 指纹伪装仅在**本机订阅**模式下生效，自定义上游不注入这些头、429 直接透传。
- **安全**：档案文件保存真实凭据，已被 `.gitignore` 忽略，请勿提交或分享；管理 API 只返回
  「是否已配置」布尔位，永不回显凭据值。

## 客户端接入

远程/本机客户端指向 ferridex 后，下游凭据会被替换为当前上游所需的凭据。面板生成的配置统一使用
ferridex 下游密钥；这是 LAN、SSH 和 ngrok 的必需认证，本机回环访问也可以直接复用同一份配置：

- **Codex**：`~/.codex/config.toml` 的 `base_url` 指向 `http://<host>:<port>/v1`，`env_key` 用 `LOCAL_PROXY_KEY`。
- **Claude Code**：`ANTHROPIC_BASE_URL=http://<host>:<port>`，`ANTHROPIC_AUTH_TOKEN=<下游密钥>`。
- **Cursor CLI**：`agent -e http://localhost:<port> --auth-token <下游密钥>`（必须用 `localhost` 且显式传 `-e`）。
- **Grok CLI**：Grok 没有 `-e`，用环境变量指向代理（等价物）。**不要用 `XAI_API_KEY`**——它会让 grok 切到 BYOK 模式直连 `api.x.ai`，绕过代理并报 “Incorrect API key”。占位凭据改用 `auth_provider_command` 提供（grok 会当成会话令牌发给代理，ferridex 再替换为本机订阅令牌）：

  ```bash
  rm -f $HOME/.grok-ferridex/auth.json          # 清缓存,确保用当前占位 token
  export GROK_HOME=$HOME/.grok-ferridex \
    GROK_CLI_CHAT_PROXY_BASE_URL=http://<host>:<port>/grok/v1 \
    GROK_AUTH_PROVIDER_COMMAND='echo <下游密钥>' \
    GROK_AUTH_TOKEN_TTL=3600
  grok
  ```

  说明：独立的 `GROK_HOME` 避免污染本机真实的 `~/.grok` 登录（`auth_provider_command` 会在刷新时改写 auth.json）。开头的 `rm` 是因为 grok 会缓存下游 token，切换 SSH↔局域网或轮换密钥后不清缓存会用旧 token 导致 `401`。

面板「远程客户端配置」会按当前目标和密钥生成上述命令，可一键复制。

## SSH 反向隧道

在面板填写 `user@host` 并启动后，SSH 只把远端回环端口转发到 ferridex 的专用 AI 中继端口；
`/api/*` 和网页面板不会被带到远端。远端端口若被占用会自动顺延，面板会显示实际端口。
在「远程客户端配置」选择「SSH 远端」，即可复制包含实际端口和下游密钥的配置。

## 公网接入（ngrok 隧道）

要从公网（不在同一局域网、也没有自备 SSH 服务器）连接，用面板的「公网隧道」卡片一键起
[ngrok](https://ngrok.com/)，拿到一个自带 TLS 的 `https://<随机>.ngrok-free.app` 公网地址。
ngrok 走 443/TLS 并支持 HTTP 代理，能穿透只放行 443 的透明代理/VPN 环境（cloudflared 走 7844，
在这类网络里常被挡）。

```bash
brew install ngrok                          # macOS;其他平台见 ngrok.com
ngrok config add-authtoken <token>          # 免费注册后在 dashboard.ngrok.com 拿 token,配一次即可
ferridex serve                              # 打开面板,点「公网隧道」→ 启动
```

ferridex 只调用 `ngrok http`，不保存也不经手你的 authtoken。

- 隧道只指向一个**专用的、只提供 AI 接口且强制要求密钥**的本地中继端口；面板与 `/api/*`
  在结构上无法经公网访问。
- 公网请求**必须**带密钥（`Authorization: Bearer <密钥>` 或 `X-API-Key`），密钥就是面板
  「远程客户端配置」里显示的那个；无密钥返回 `401`。ngrok 免费版地址每次启动都会变、自身无鉴权，
  **密钥是唯一防线，请勿泄露**。
- 隧道运行时，「远程客户端配置」右上角会出现相应目标切换，选「公网」即可复制指向公网
  地址的启动命令。
- ngrok 免费版对**浏览器 GET** 有一个中间警告页，但 AI 客户端发的是 JSON API 请求，不会触发；
  另外免费版同时只能开 1 个隧道、有速率限制，个人自用够。
- Cursor CLI 依赖 host 为 `localhost` 且 Connect-RPC 需端到端 HTTP/2，经隧道可能不通；
  Claude / Codex / Grok 正常。

冒烟测试（外网带密钥应返回模型列表，去掉密钥应 `401`）：

```bash
curl -sS -H "Authorization: Bearer <密钥>" https://<随机>.ngrok-free.app/grok/v1/models
```

冒烟测试（确认 Grok 代理连通，应返回模型列表）：

```bash
curl -sS http://127.0.0.1:8788/grok/v1/models
```
