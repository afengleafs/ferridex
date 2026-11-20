# ferridex

[English](README.md) | **中文**

`ferridex` 是一个本机 Go 代理，复用已登录的 **Codex (ChatGPT)**、**Claude Code**、**Cursor CLI**、**Grok CLI** 与 **Antigravity CLI（`agy`）** 订阅，内置 Web 面板用于上游热切换、配额与 Token 用量监控、单次对话响应速度监测及远程接入。

> 本项目调用非公开上游端点并读取本机凭据，仅适合个人研究与自用，请勿共享或转售。程序默认拒绝非回环监听（局域网需显式 `-lan`），SSH 与 ngrok 隧道仅暴露带密钥的 AI 接口，不暴露面板与 `/api/*`。

## 核心能力

- **多订阅统一代理**：同时提供 Codex（`/v1/responses`）、Claude（`/v1/messages`）、Antigravity（`/agy/v1beta`）、Grok（`/grok/v1`）与 Cursor CLI 兼容入口。
- **上游热切换**：Claude / Codex 可在本机订阅与多个第三方供应商（`claude_provider.env` / `codex_provider.env`）之间即时切换，客户端无需重启。
- **用量与配额监控**：支持按需查询 Codex、Claude、`agy` 订阅配额窗口，并持久化统计代理 Token 消耗、缓存命中率与近 24 小时趋势（`~/.ferridex/telemetry.jsonl`）。
- **响应速度监测**：逐次记录每次对话的首字延迟（TTFT）、输出解码速度（`tok/s`）与总耗时，支持按模型横向对比。
- **远程接入与实时日志**：内置 LAN（`-lan`）、SSH 反向隧道、ngrok 公网隧道、一键复制客户端启动命令与 SSE 实时请求日志。

## 构建与启动

需要 Go 1.23+，以及按需完成对应 CLI 的本机订阅登录。

```bash
git clone https://github.com/afengleafs/ferridex.git
cd ferridex
mkdir -p ~/.local/bin
go build -o ~/.local/bin/ferridex .
export PATH="$HOME/.local/bin:$PATH"
```

```bash
ferridex serve                      # 启动全部代理与面板（默认 http://127.0.0.1:8788/）
ferridex serve -addr 127.0.0.1:8789 # 指定回环端口（支持 -no-open / -auto-port）
ferridex serve -lan                 # 向局域网开放 AI 接口（自动生成密钥，面板仍限本机）
ferridex status                     # 检查本机各 Provider 登录状态
# 仅启动单个代理（不带面板）：ferridex codex | claude | cursor | grok | agy
```

## 自定义上游供应商

在 ferridex **运行目录**的 `claude_provider.env` / `codex_provider.env` 中添加 `[供应商名]` 分段（首次启动自动创建模板并强制 `0600` 权限），保存后在面板点击「重新加载」即可生效：

| 客户端 | 供应商文件 | 必填字段 |
| --- | --- | --- |
| Claude | `claude_provider.env` | `ANTHROPIC_BASE_URL`，以及 `ANTHROPIC_AUTH_TOKEN` 或 `ANTHROPIC_API_KEY` |
| Codex | `codex_provider.env` | `OPENAI_BASE_URL`、`OPENAI_API_KEY`、`OPENAI_DEFAULT_MODEL` |

```ini
# claude_provider.env
[openrouter]
ANTHROPIC_BASE_URL="https://openrouter.ai/api"
ANTHROPIC_AUTH_TOKEN="sk-or-v1-..."
ANTHROPIC_DEFAULT_OPUS_MODEL="stealth/ox-alpha"
ANTHROPIC_DEFAULT_SONNET_MODEL="stealth/ox-alpha"
ANTHROPIC_DEFAULT_HAIKU_MODEL="stealth/ox-alpha"
```

```ini
# codex_provider.env
[openrouter]
OPENAI_BASE_URL="https://openrouter.ai/api/v1"
OPENAI_API_KEY="sk-or-v1-..."
OPENAI_DEFAULT_MODEL="openai/gpt-5.6"
```

- Claude 支持按 `opus` / `sonnet` / `haiku` 档位改写模型名，未配置档位原样透传。
- Codex 供应商需支持 OpenAI Responses API（`/responses`）；客户端仍使用 ferridex 下游密钥（`LOCAL_PROXY_KEY`），勿直接填入供应商 API Key。
- 供应商文件已被 `.gitignore` 忽略，管理 API 只返回脱敏状态，不回显凭据值。

## 客户端接入

面板「客户端」页会按当前目标（本机 / 局域网 / SSH 远端 / 公网）生成带下游密钥的配置，直接一键复制即可：

- **Claude Code**：`ANTHROPIC_BASE_URL=http://<host>:<port> ANTHROPIC_AUTH_TOKEN=<密钥> claude`
- **Codex**：`~/.codex/config.toml` 设置 `base_url = "http://<host>:<port>/v1"`、`env_key = "LOCAL_PROXY_KEY"`
- **Antigravity（`agy`）**：`GEMINI_API_KEY=<密钥> GOOGLE_GEMINI_BASE_URL=http://<host>:<port>/agy agy --settings '{"modelProvider":"gemini"}'`
- **Cursor CLI**：`agent -e http://localhost:<port> --auth-token <密钥>`（host 必须为 `localhost`）
- **Grok CLI**：使用独立 `GROK_HOME` 与 `GROK_AUTH_PROVIDER_COMMAND`（**不要设置 `XAI_API_KEY`**，否则会绕过代理直连 `api.x.ai`）：

```bash
rm -f $HOME/.grok-ferridex/auth.json
export GROK_HOME=$HOME/.grok-ferridex \
  GROK_CLI_CHAT_PROXY_BASE_URL=http://<host>:<port>/grok/v1 \
  GROK_AUTH_PROVIDER_COMMAND='echo <密钥>' \
  GROK_AUTH_TOKEN_TTL=3600
grok
```

## 远程接入

- **局域网**：使用 `ferridex serve -lan` 启动，在面板直接复制局域网配置。
- **SSH 反向隧道**：在面板「远程访问」填入 `user@host` 启动（需本机 `ssh` 与免密密钥/agent），仅转发带密钥的 AI 中继端口，端口占用自动顺延。
- **公网隧道（ngrok）**：安装 `ngrok` 并执行一次 `ngrok config add-authtoken <token>` 后在面板一键启动；所有公网请求必须携带下游密钥（`Authorization: Bearer <密钥>` / `X-API-Key` / `x-goog-api-key`）。Cursor Connect-RPC 经公网隧道可能不可用。
