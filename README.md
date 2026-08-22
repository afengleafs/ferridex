# ferridex

**English** | [中文](README.zh.md)

`ferridex` is a local Go proxy that reuses already-logged-in ChatGPT/Codex, Claude Code, Cursor CLI, and Grok CLI subscriptions, and exposes compatible APIs to this machine or to remote hosts connected over an SSH tunnel.

> This project calls unofficial upstream endpoints and reads local login credentials. It is for personal research and personal use only.
> Do not share or resell the proxy. For public-network access, use the dashboard ngrok tunnel (a key is required, and the dashboard is not exposed).
> The process refuses non-loopback listen addresses unless `-lan` is set; do not bypass this protection to bind the service to the public internet.

## Build

- Go 1.23 or later
- A completed subscription login for Codex CLI, Claude Code, Cursor CLI, and/or Grok CLI (as needed)
- Local `ssh` plus a passwordless key (or `ssh-agent`) if you manage reverse tunnels from the dashboard

```bash
git clone https://github.com/afengleafs/ferridex.git
cd ferridex
mkdir -p ~/.local/bin
go build -o ~/.local/bin/ferridex .
export PATH="$HOME/.local/bin:$PATH"
```

After pulling source changes, rebuild and restart the process. `go build ./...` alone does not replace the binary on your `PATH`.

## Start

```bash
# Start Codex, Claude, Cursor, Grok, and the web dashboard; open a browser
ferridex serve

# Pin the port / skip opening a browser / pick the next free port
ferridex serve -addr 127.0.0.1:8789
ferridex serve -no-open
ferridex serve -auto-port

# Expose AI APIs on the LAN (auto-generated key; dashboard stays loopback-only)
ferridex serve -lan

# Run a single proxy, or skip the dashboard
ferridex codex
ferridex claude
ferridex cursor
ferridex grok

# Check local login status
ferridex status
```

The default listen address is `127.0.0.1:8788`. Dashboard: `http://127.0.0.1:8788/`.
Addresses such as `-addr 0.0.0.0:...` are rejected; use `ferridex serve -lan` for LAN access.

## Custom Responses upstream

The dashboard **Responses upstream** card hot-switches between two sources. Saving or switching does not require a ferridex restart:

- **Local subscription (`subscription`)**: keep using the ChatGPT/Codex login on this machine.
- **Custom upstream (`custom`)**: set a provider name, API root (Base URL), API key, and default model, then forward to an enterprise service that speaks the OpenAI Responses API.

Example: wiring an enterprise VOD endpoint in the dashboard:

```text
Provider name:  VOD GPT
Base URL:       https://text-aigc.vod-qcloud.com/v1
API Key:        <VOD_API_TOKEN>
Default model:  gpt-5.6-sol
```

> If a real VOD token was ever pasted into chat, an issue, logs, or a git repo, revoke and rotate it on the enterprise side first, then paste the new token into the dashboard. Never record real tokens in docs or config samples.

The real upstream API key is stored only in `~/.ferridex/config.json` on this machine (mode `0600`). It is never returned to the web UI and never written into client config. Codex still talks only to ferridex and sends ferridex's **downstream key** via `LOCAL_PROXY_KEY`. Do not set the enterprise upstream API key as `LOCAL_PROXY_KEY`:

```toml
model = "gpt-5.6-sol"
model_provider = "ferridex"

[model_providers.ferridex]
name = "Ferridex Responses Proxy"
base_url = "http://127.0.0.1:8788/v1"
env_key = "LOCAL_PROXY_KEY"
wire_api = "responses"
```

Loopback, LAN, SSH, and ngrok clients all follow the currently selected Responses upstream. Switching back to the local subscription does not require client-config changes. SSH and ngrok tunnels connect only to a dedicated relay-only port, require the ferridex downstream key, and never expose the dashboard or the real upstream key.

Only one custom Responses upstream is supported. There is no multi-profile set, no automatic failover, and no protocol translation. There is also no bare public listen. For public access use the ngrok flow below; do not bind ferridex to `0.0.0.0`.

## Claude upstream profiles

The **Upstream** panel's Claude tab hot-switches, by clicking cards, between the **local Anthropic subscription** and multiple **custom Anthropic-compatible upstreams** (OpenRouter and similar gateways). The layout follows cc-switch: Claude / Codex tabs plus full-width provider cards. Local Claude Code needs no config change and no restart:

- Profiles live in `ferridex-profiles.env` in ferridex's **working directory** (a commented template is created on first start; file mode `0600`). Grammar: `[profile-name]` sections, one `KEY=VALUE` per line, with optional `export` prefixes, single/double quotes, and `#` comments — you can paste Claude Code env syntax as-is:

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

- **Model mapping**: if the requested model name contains `opus` / `sonnet` / `haiku` (case-insensitive), it is rewritten to the matching profile field. Unmatched or unset tiers pass through unchanged; ferridex never silently remaps across tiers. `CLAUDE_CODE_SUBAGENT_MODEL` is a client-local variable: ferridex accepts it but does not consume it.
- **Credentials**: `ANTHROPIC_AUTH_TOKEN` → `Authorization: Bearer`; `ANTHROPIC_API_KEY` → `x-api-key` (Bearer wins when both are set). `BASE_URL` is the API root, with or without a trailing `/v1`; ferridex always forwards to `<root>/v1/messages`.
- **Hot switch**: clicking a card takes effect immediately and is persisted to `~/.ferridex/config.json` (`claude_upstream`). It is restored on restart. After editing the file, click **Reload profiles**. If a selected profile is deleted, ferridex falls back to the local subscription and marks that in the panel; add the profile back and reload to restore it.
- **Scope**: only `/v1/messages` (the Claude Code main path). OAuth refresh, quota breaker, and Claude Code fingerprint headers apply only in **local subscription** mode. Custom upstreams do not inject those headers; HTTP 429 is passed through.
- **Security**: the profiles file holds real credentials, is gitignored, and must not be committed or shared. Management APIs return only configured/not-configured booleans and never echo credential values.

## Client setup

Once a local or remote client points at ferridex, downstream credentials are replaced with whatever the active upstream needs. Dashboard-generated configs always use the ferridex downstream key. That key is required for LAN, SSH, and ngrok; the same config also works on loopback:

- **Codex**: set `base_url` in `~/.codex/config.toml` to `http://<host>:<port>/v1` and `env_key` to `LOCAL_PROXY_KEY`.
- **Claude Code**: `ANTHROPIC_BASE_URL=http://<host>:<port>`, `ANTHROPIC_AUTH_TOKEN=<downstream-key>`.
- **Cursor CLI**: `agent -e http://localhost:<port> --auth-token <downstream-key>` (`localhost` is required, and `-e` must be passed explicitly).
- **Grok CLI**: Grok has no `-e` flag; point it at the proxy with env vars. **Do not set `XAI_API_KEY`** — that switches grok into BYOK mode against `api.x.ai`, bypasses the proxy, and returns “Incorrect API key”. Use `auth_provider_command` for the placeholder credential (grok sends it as a session token; ferridex then substitutes the local subscription token):

  ```bash
  rm -f $HOME/.grok-ferridex/auth.json          # drop cache so the current placeholder token is used
  export GROK_HOME=$HOME/.grok-ferridex \
    GROK_CLI_CHAT_PROXY_BASE_URL=http://<host>:<port>/grok/v1 \
    GROK_AUTH_PROVIDER_COMMAND='echo <downstream-key>' \
    GROK_AUTH_TOKEN_TTL=3600
  grok
  ```

  A separate `GROK_HOME` keeps this from rewriting the real `~/.grok` login (`auth_provider_command` mutates `auth.json` on refresh). The opening `rm` is required because grok caches the downstream token; after switching SSH↔LAN or rotating the key, a stale cache yields `401`.

The dashboard **Remote client config** card generates these commands for the current target and key, ready to copy.

## SSH reverse tunnel

Enter `user@host` in the dashboard and start the tunnel. SSH forwards only a remote loopback port to ferridex's dedicated AI relay port; `/api/*` and the web dashboard are not carried to the remote side. If the remote port is taken, ferridex walks to the next free one and the dashboard shows the actual port. Choose **SSH remote** under **Remote client config** to copy a snippet with that port and the downstream key.

## Public access (ngrok tunnel)

To connect from the public internet (not on the same LAN, and with no SSH server of your own), use the dashboard **Public tunnel** card to start [ngrok](https://ngrok.com/) and get a TLS `https://<random>.ngrok-free.app` URL. ngrok uses 443/TLS and works through HTTP proxies, so it can traverse transparent-proxy / VPN networks that only allow 443 (cloudflared uses 7844 and is often blocked there).

```bash
brew install ngrok                          # macOS; see ngrok.com for other platforms
ngrok config add-authtoken <token>          # register free, copy the token from dashboard.ngrok.com, once
ferridex serve                              # open the panel, Public tunnel → Start
```

ferridex only runs `ngrok http`. It never stores or handles your authtoken.

- The tunnel points only at a **dedicated, AI-only, key-required** local relay port. The dashboard and `/api/*` cannot be reached over the public URL.
- Public requests **must** send the key (`Authorization: Bearer <key>` or `X-API-Key`). It is the same key shown under **Remote client config**. Missing key → `401`. Free ngrok URLs change on every start and have no auth of their own; **the key is the only control, do not leak it**.
- While the tunnel is up, **Remote client config** grows a target toggle; pick **Public** to copy launch commands that use the public URL.
- Free ngrok inserts an interstitial on **browser GET**, but AI clients send JSON API requests and skip it. The free plan allows one tunnel at a time and is rate-limited; that is enough for personal use.
- Cursor CLI requires host `localhost` and end-to-end HTTP/2 for Connect-RPC, so it may fail through a tunnel. Claude / Codex / Grok work.

Smoke test (a keyed request from the public internet should list models; omitting the key should `401`):

```bash
curl -sS -H "Authorization: Bearer <key>" https://<random>.ngrok-free.app/grok/v1/models
```

Smoke test (confirm the local Grok proxy; should list models):

```bash
curl -sS http://127.0.0.1:8788/grok/v1/models
```
