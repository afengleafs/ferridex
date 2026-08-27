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

## Upstream suppliers

The dashboard's **Upstream** page uses cc-switch-style Claude / Codex tabs and full-width supplier cards. Both tabs hot-switch without restarting ferridex or changing client configuration:

- **Claude** switches between the local Anthropic subscription and multiple Anthropic-compatible suppliers.
- **Codex** switches between the local ChatGPT subscription and multiple OpenAI Responses-compatible suppliers.
- Supplier definitions are read from two files in ferridex's **working directory**. A commented template is created on first start and permissions are forced to `0600`:

  | Client | Supplier file | Required keys |
  | --- | --- | --- |
  | Claude | `claude_provider.env` | `ANTHROPIC_BASE_URL` plus `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` |
  | Codex | `codex_provider.env` | `OPENAI_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_DEFAULT_MODEL` |

The common grammar is `[supplier-name]` sections, one `KEY=VALUE` per line, with optional `export` prefixes, single/double quotes, and `#` comments. Add as many sections as needed, save the file, then click **Reload** in the corresponding dashboard tab.

Claude example (`claude_provider.env`):

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

Codex example (`codex_provider.env`):

```ini
[openrouter]
OPENAI_BASE_URL="https://openrouter.ai/api/v1"
OPENAI_API_KEY="sk-or-v1-..."
OPENAI_DEFAULT_MODEL="openai/gpt-5.6"
```

- **Claude model mapping**: if the requested model name contains `opus` / `sonnet` / `haiku` (case-insensitive), it is rewritten to the matching supplier field. Unmatched or unset tiers pass through unchanged; ferridex never silently remaps across tiers. `CLAUDE_CODE_SUBAGENT_MODEL` is a client-local variable: ferridex accepts it but does not consume it.
- **Credentials**: `ANTHROPIC_AUTH_TOKEN` → `Authorization: Bearer`; `ANTHROPIC_API_KEY` → `x-api-key` (Bearer wins when both are set). `BASE_URL` is the API root, with or without a trailing `/v1`; ferridex always forwards to `<root>/v1/messages`.
- **Codex protocol**: suppliers must speak the OpenAI Responses API; ferridex forwards to `<OPENAI_BASE_URL>/responses` without translating another protocol. The Test button sends one minimal request and may incur a small charge.
- **Hot switch**: clicking a card takes effect immediately. Only the selected supplier name is persisted in `~/.ferridex/config.json`; credentials remain in the supplier files. Claude falls back to its local subscription if a selected supplier disappears. Codex fails closed instead of silently consuming the local subscription; restore the section and reload to resume it.
- **Scope**: Claude suppliers affect only `/v1/messages`; Codex suppliers affect only `/v1/responses`. Subscription-only OAuth refresh, usage lookup, quota breaker, and fingerprint behavior are not applied to custom suppliers.
- **Migration and security**: on first start after upgrading, a non-empty legacy `ferridex-profiles.env` is copied to `claude_provider.env`; a legacy Codex custom upstream from `~/.ferridex/config.json` is copied to `codex_provider.env`. Legacy data is retained for rollback and a non-empty new file is never overwritten. All supplier files are gitignored. Management APIs expose only redacted status, never credential values.

Codex clients still send ferridex's **downstream key** via `LOCAL_PROXY_KEY`; never use a supplier API key as `LOCAL_PROXY_KEY`. Loopback, LAN, SSH, and ngrok clients all follow the current card selection. SSH and ngrok expose only the keyed relay, not the dashboard or supplier credentials.

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
