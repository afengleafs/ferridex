# ferridex

**English** | [中文](README.zh.md)

`ferridex` is a local Go proxy that reuses your logged-in **Codex (ChatGPT)**, **Claude Code**, **Cursor CLI**, **Grok CLI**, and **Antigravity CLI (`agy`)** subscriptions, with a built-in web dashboard for upstream switching, quota & token monitoring, latency/speed tracking, and remote access.

> For personal research and self-use only. Do not share or resell the proxy. Non-loopback binding is rejected unless `-lan` is set; remote/public tunnels only expose keyed AI endpoints and never expose the dashboard or `/api/*`.

## Features

- **Multi-provider local gateway**: Unified endpoints for Codex (`/v1/responses`), Claude (`/v1/messages`), Antigravity (`/agy/v1beta`), Grok (`/grok/v1`), and Cursor CLI.
- **Hot upstream switching**: Switch Claude and Codex between local subscriptions and custom API suppliers (`claude_provider.env` / `codex_provider.env`) without restarting clients.
- **Usage & quota monitoring**: On-demand subscription quota lookup (Codex, Claude, `agy`) plus persistent token usage, cache hit rate, and 24h trend charts (`~/.ferridex/telemetry.jsonl`).
- **Per-turn speed telemetry**: Track TTFT, decode speed (`tok/s`), and total duration per conversation turn across models.
- **Remote access**: Built-in LAN (`-lan`), SSH reverse tunnel, and `ngrok` public tunnel support with one-click client config generation and live SSE request logs.

## Build & Run

Requires Go 1.23+ and local CLI logins for the providers you use.

```bash
git clone https://github.com/afengleafs/ferridex.git
cd ferridex
mkdir -p ~/.local/bin
go build -o ~/.local/bin/ferridex .
export PATH="$HOME/.local/bin:$PATH"
```

```bash
ferridex serve                      # start all proxies + dashboard (http://127.0.0.1:8788/)
ferridex serve -addr 127.0.0.1:8789 # custom loopback port (-no-open / -auto-port supported)
ferridex serve -lan                 # expose AI APIs on LAN with auto-generated key
ferridex status                     # check local provider login status
# Single-provider mode (no dashboard): ferridex codex | claude | cursor | grok | agy
```

## Custom Upstreams

Place supplier sections in `claude_provider.env` and/or `codex_provider.env` in ferridex's **working directory** (templates are created on first run with `0600` permissions), then click **Reload** in the dashboard:

| Client | File | Required keys |
| --- | --- | --- |
| Claude | `claude_provider.env` | `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` |
| Codex | `codex_provider.env` | `OPENAI_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_DEFAULT_MODEL` |

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

- Claude maps `opus` / `sonnet` / `haiku` tiers when configured; unmatched tiers pass through unchanged.
- Codex suppliers must speak the OpenAI Responses API (`/responses`). Clients still use ferridex's downstream key (`LOCAL_PROXY_KEY`), never the supplier API key.
- Supplier files are gitignored and management APIs never return secret values.

## Client Setup

Use the dashboard's **Clients** tab to copy ready-to-run commands for **Local / LAN**, **SSH**, or **Public (ngrok)**:

- **Claude Code**: `ANTHROPIC_BASE_URL=http://<host>:<port> ANTHROPIC_AUTH_TOKEN=<key> claude`
- **Codex**: set `base_url = "http://<host>:<port>/v1"` and `env_key = "LOCAL_PROXY_KEY"` in `~/.codex/config.toml`
- **Antigravity (`agy`)**: `GEMINI_API_KEY=<key> GOOGLE_GEMINI_BASE_URL=http://<host>:<port>/agy agy --settings '{"modelProvider":"gemini"}'`
- **Cursor CLI**: `agent -e http://localhost:<port> --auth-token <key>` (requires `localhost`)
- **Grok CLI**: isolate `GROK_HOME` and use `GROK_AUTH_PROVIDER_COMMAND` (do **not** set `XAI_API_KEY`, which bypasses the proxy):

```bash
rm -f $HOME/.grok-ferridex/auth.json
export GROK_HOME=$HOME/.grok-ferridex \
  GROK_CLI_CHAT_PROXY_BASE_URL=http://<host>:<port>/grok/v1 \
  GROK_AUTH_PROVIDER_COMMAND='echo <key>' \
  GROK_AUTH_TOKEN_TTL=3600
grok
```

## Remote Access

- **LAN**: Run `ferridex serve -lan` and copy the LAN commands (key included) from the dashboard.
- **SSH reverse tunnel**: Start from the **Remote Access** tab with `user@host` (requires local `ssh` + key/agent). Forwards only the keyed AI relay port.
- **Public tunnel (`ngrok`)**: Install `ngrok`, run `ngrok config add-authtoken <token>` once, then start from the dashboard. All public requests require the downstream key (`Authorization: Bearer <key>` / `X-API-Key` / `x-goog-api-key`); Cursor Connect-RPC may not work over public tunnels.
