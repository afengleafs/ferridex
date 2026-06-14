# ferridex

`ferridex` 是一个运行在本机的 Go 代理，复用已经登录的 ChatGPT/Codex 与
Claude Code 订阅，并向本机或通过 SSH 隧道连接的远程主机提供兼容接口。

> 本项目调用非公开上游端点并读取本机登录凭据，仅适合个人研究和自用。
> 上游协议、客户端指纹或服务条款变化都可能导致功能失效。不要共享、转售
> 或将代理暴露到公网。

## 功能

- `POST /v1/responses`：转发 OpenAI Responses 协议请求到 Codex 后端。
- `POST /v1/messages`：转发 Anthropic Messages 协议请求到 Claude 后端。
- `GET /healthz`：健康检查。
- `GET /`：统一面板，显示登录状态、端点、实时日志和 SSH 隧道状态。
- 面板可一键复制远程 Codex、Claude Code 配置。
- 面板可启动和停止绑定远端回环地址的 SSH 反向隧道。
- 自动读取并刷新本机 Codex/Claude 登录令牌。
- 可选下游 API Key，避免本机或隧道中的其他进程直接调用代理。

## 工作方式

### Codex

`ferridex` 从 `$CODEX_HOME/auth.json` 或默认的 `~/.codex/auth.json` 读取
ChatGPT 登录令牌。令牌过期时会尝试刷新，并以 `0600` 权限更新该文件。

运行前先安装 Codex CLI 并完成 ChatGPT 订阅登录：

```bash
codex login
```

### Claude

`ferridex` 依次读取：

1. `~/.ferridex/claude-creds.json` 中的有效缓存；
2. `~/.claude/.credentials.json`；
3. macOS Keychain 中的 `Claude Code-credentials` 或 `Claude Code` 项。

刷新后的 Claude 令牌会写入 `~/.ferridex/claude-creds.json`，目录权限为
`0700`，文件权限为 `0600`。

运行前先安装 Claude Code 并完成订阅登录。

## 环境要求

- Go 1.23 或更高版本。
- 已完成订阅登录的 Codex CLI 和/或 Claude Code。
- 使用面板管理反向隧道时，需要本机安装 `ssh`，并使用免密密钥或
  已加载到 `ssh-agent` 的密钥。

在 macOS 26 上建议使用较新的 Go 版本；旧版本构建的二进制可能出现
`missing LC_UUID`。

## 构建

```bash
git clone https://github.com/afengleafs/ferridex.git
cd ferridex
mkdir -p ~/.local/bin
go build -o ~/.local/bin/ferridex .
```

确保 `~/.local/bin` 位于 `PATH` 中：

```bash
export PATH="$HOME/.local/bin:$PATH"
```

## 启动

```bash
# 同时启动 Codex、Claude 和 Web 面板，并自动打开浏览器
ferridex serve

# 使用指定端口启动统一网关
ferridex serve -addr 127.0.0.1:8789

# 启动统一网关，但不自动打开浏览器
ferridex serve -no-open

# 只启动一个代理，不提供 Web 面板
ferridex codex
ferridex claude

# 检查本机登录状态
ferridex status
```

默认监听地址为 `127.0.0.1:8788`。不要将监听地址改为 `0.0.0.0`，除非已经
理解凭据代理被其他主机访问的风险并配置了下游鉴权。

## Web 面板

执行 `ferridex serve` 后访问：

```text
http://127.0.0.1:8788/
```

面板提供：

- Codex 和 Claude 登录状态；
- 当前代理端点；
- Claude Code `~/.claude/settings.json` 配置一键复制；
- Codex `~/.codex/config.toml` 配置一键复制；
- `export LOCAL_PROXY_KEY=dummy` 命令一键复制；
- SSH 反向隧道管理；
- `/v1/responses` 和 `/v1/messages` 请求日志。

复制出的配置会自动跟随面板当前端口。

## SSH 远程使用

推荐始终将远端监听地址绑定到 `127.0.0.1`。下面示例把本地 `8789` 端口
转发到远程主机的 `127.0.0.1:8789`：

```bash
# 本地 Mac：启动统一网关
ferridex serve -addr 127.0.0.1:8789

# 本地 Mac：建立 SSH 反向隧道
ssh -N \
  -o ServerAliveInterval=30 \
  -o ServerAliveCountMax=3 \
  -o ExitOnForwardFailure=yes \
  -R 127.0.0.1:8789:127.0.0.1:8789 \
  user@example-host
```

也可以在 Web 面板中填写 `user@host` 和 SSH key 路径启动隧道。远程主机
安装 Codex 或 Claude Code 后，打开面板并复制对应配置即可。

### 远程客户端配置

以下示例假设 SSH 反向隧道使用远程端口 `8789`。如果使用其他端口，请将
配置中的 `8789` 替换为实际端口。Web 面板中的一键复制内容会自动跟随面板
当前端口。

#### Claude Code

将以下内容写入远程主机的 `~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8789",
    "ANTHROPIC_AUTH_TOKEN": "dummy",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-4-8",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4-5-20250929",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku-4-5-20251001"
  },
  "permissions": {
    "defaultMode": "bypassPermissions"
  },
  "skipDangerousModePermissionPrompt": true,
  "model": "opus"
}
```

#### Codex

将以下内容写入远程主机的 `~/.codex/config.toml`：

```toml
model_provider = "localproxy"
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
base_url = "http://127.0.0.1:8789/v1"
wire_api = "responses"
env_key = "LOCAL_PROXY_KEY"
requires_openai_auth = false
```

启动 Codex 前，在同一个远程 shell 中执行：

```bash
export LOCAL_PROXY_KEY=dummy
codex
```

`LOCAL_PROXY_KEY=dummy` 是 `localproxy` provider 要求的占位凭据。它不是真实
OpenAI API Key，也不会替代远程主机的 `~/.codex/auth.json`。

上面的 Claude 和 Codex 配置分别启用了 `bypassPermissions` 与
`danger-full-access`。它们会降低命令执行确认和沙箱限制，只应在可信远程主机
与可信项目中使用。

远程验证：

```bash
curl -sS http://127.0.0.1:8789/healthz
```

## 冒烟测试

```bash
curl -sS http://127.0.0.1:8788/healthz
```

Codex：

```bash
curl -sS http://127.0.0.1:8788/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"say hi in three words"}]}]}'
```

Claude：

```bash
curl -sS http://127.0.0.1:8788/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[{"role":"user","content":"say hi in three words"}]}'
```

## 可选下游鉴权

默认情况下，能访问监听端口的进程无需 API Key。可以创建
`~/.ferridex/config.json` 并重启 `ferridex`：

```json
{
  "downstream_key": "replace-with-a-random-secret"
}
```

客户端请求需要携带以下任一请求头：

```text
Authorization: Bearer replace-with-a-random-secret
X-API-Key: replace-with-a-random-secret
```

不要把 `~/.ferridex/config.json`、Codex/Claude 凭据文件或真实密钥提交到
Git 仓库。

## 开发与检查

```bash
go test ./...
go vet ./...
go build ./...
```

项目当前没有自动化测试用例；`go test ./...` 主要执行编译检查。

## 已知限制

- Codex 与 Claude 使用的上游端点、OAuth 参数和客户端指纹可能随时变化。
- Claude 转发会模拟 Claude Code 的请求特征，兼容性取决于当前上游行为。
- 模型名称和账号权限必须与实际订阅一致。
- 实时日志仅记录请求路径、状态码和耗时，不记录请求正文。
- 反向隧道会让远程主机上的进程访问本机代理，应仅连接可信主机。

## 上游归属与许可证状态

部分 Codex 转发逻辑参考或移植自
[`David-Factor/codex-responses-proxy`](https://github.com/David-Factor/codex-responses-proxy)
（Apache-2.0），部分 Claude Code 请求兼容逻辑参考或移植自
[`Wei-Shaw/sub2api`](https://github.com/Wei-Shaw/sub2api)（LGPL-3.0）。

本仓库当前没有声明统一的项目许可证。上游项目相关部分仍受其各自许可证
约束；在重新分发或修改前，请自行检查并遵守对应许可证与服务条款。
