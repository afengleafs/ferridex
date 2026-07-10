# ferridex

`ferridex` 是一个运行在本机的 Go 代理，复用已登录的 ChatGPT/Codex、Claude Code、Cursor CLI 与 Grok CLI 订阅，并向本机或通过 SSH 隧道连接的远程主机提供兼容接口。

> 本项目调用非公开上游端点并读取本机登录凭据，仅适合个人研究和自用。
> 不要共享或转售代理。需要从公网连接时，请用面板的 ngrok 隧道（强制密钥，且不暴露面板），
> 不要把监听地址直接改成 `0.0.0.0` 裸奔到公网。

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
不要手动改成 `0.0.0.0`；局域网请用 `ferridex serve -lan`。

## 客户端接入

远程/本机客户端指向 ferridex 后，占位凭据会被替换为本机订阅令牌（局域网下用面板里的密钥代替 `dummy`）：

- **Codex**：`~/.codex/config.toml` 的 `base_url` 指向 `http://<host>:<port>/v1`，`env_key` 用 `LOCAL_PROXY_KEY`。
- **Claude Code**：`ANTHROPIC_BASE_URL=http://<host>:<port>`，`ANTHROPIC_AUTH_TOKEN=dummy`。
- **Cursor CLI**：`agent -e http://localhost:<port> --auth-token dummy`（必须用 `localhost` 且显式传 `-e`）。
- **Grok CLI**：Grok 没有 `-e`，用环境变量指向代理（等价物）。**不要用 `XAI_API_KEY`**——它会让 grok 切到 BYOK 模式直连 `api.x.ai`，绕过代理并报 “Incorrect API key”。占位凭据改用 `auth_provider_command` 提供（grok 会当成会话令牌发给代理，ferridex 再替换为本机订阅令牌）：

  ```bash
  rm -f $HOME/.grok-ferridex/auth.json          # 清缓存,确保用当前占位 token
  export GROK_HOME=$HOME/.grok-ferridex \
    GROK_CLI_CHAT_PROXY_BASE_URL=http://<host>:<port>/grok/v1 \
    GROK_AUTH_PROVIDER_COMMAND='echo dummy' \
    GROK_AUTH_TOKEN_TTL=3600
  grok
  ```

  说明：独立的 `GROK_HOME` 避免污染本机真实的 `~/.grok` 登录（`auth_provider_command` 会在刷新时改写 auth.json）；局域网模式下把两处 `dummy` 换成 `<密钥>`。开头的 `rm` 是因为 grok 会缓存占位 token，切换 SSH↔局域网或轮换密钥后不清缓存会用旧 token 导致 `401`。

面板「远程客户端配置」会按当前端口和密钥生成上述命令，可一键复制。

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
- 隧道运行时，「远程客户端配置」右上角会出现「本地 / 公网」切换，选「公网」即可复制指向公网
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
