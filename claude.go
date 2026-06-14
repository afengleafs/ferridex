package main

// ClaudeProvider relays to the Anthropic Messages API using the local Claude Code
// subscription login. Anthropic gates plan-quota vs "third-party usage" on the
// full Claude Code client fingerprint, so this mirrors what the official CLI sends
// (headers, beta flags, system identity, metadata.user_id, full model ids) —
// details ported from Wei-Shaw/sub2api (pkg/claude/constants.go, pkg/oauth,
// service/claude_code_validator.go).
//
// Credentials are sourced from the local Claude Code login at runtime:
// ~/.claude/.credentials.json, else the macOS Keychain item "Claude Code-credentials".
// Refreshed tokens are cached in ~/.ferridex/claude-creds.json (the Keychain item
// itself is never modified).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	claudeClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeTokenURL    = "https://platform.claude.com/v1/oauth/token"
	claudeMessagesURL = "https://api.anthropic.com/v1/messages"
	anthropicVersion  = "2023-06-01"
	claudeCLIVersion  = "2.1.161"
	claudeSystemID    = "You are Claude Code, Anthropic's official CLI for Claude."
	claudeRefreshSkew = 60 * time.Second
	claudeKeychainSvc = "Claude Code-credentials"
)

// claudeMimicHeaders is the rest of the official Claude Code CLI request fingerprint.
var claudeMimicHeaders = map[string]string{
	"User-Agent":                                "claude-cli/" + claudeCLIVersion + " (external, cli)",
	"X-App":                                     "cli",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-Package-Version":               "0.94.0",
	"X-Stainless-OS":                            "Linux",
	"X-Stainless-Arch":                          "arm64",
	"X-Stainless-Runtime":                       "node",
	"X-Stainless-Runtime-Version":               "v24.3.0",
	"X-Stainless-Retry-Count":                   "0",
	"X-Stainless-Timeout":                       "600",
	"Anthropic-Dangerous-Direct-Browser-Access": "true",
}

// claudeModelOverrides maps short model names to the full ids Claude OAuth wants.
var claudeModelOverrides = map[string]string{
	"claude-sonnet-4-5": "claude-sonnet-4-5-20250929",
	"claude-opus-4-5":   "claude-opus-4-5-20251101",
	"claude-haiku-4-5":  "claude-haiku-4-5-20251001",
}

// claudeBetas is the full Claude Code mimicry beta set (aligned with sub2api's
// FullClaudeCodeMimicryBetas). claude-code/oauth gate the subscription path; the
// rest gate request-body features the real CLI sends (e.g. context_management).
// The client's own anthropic-beta is merged on top, so any beta Claude Code adds
// keeps its matching body field permitted (else upstream returns 400 "Extra inputs").
var claudeBetas = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
	"interleaved-thinking-2025-05-14",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"effort-2025-11-24",
	"extended-cache-ttl-2025-04-11",
}

// mergeBetas unions ferridex's required betas with whatever the client sent.
func mergeBetas(clientHeader string) string {
	seen := map[string]bool{}
	out := make([]string, 0, len(claudeBetas)+8)
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b == "" || seen[b] {
			return
		}
		seen[b] = true
		out = append(out, b)
	}
	for _, b := range claudeBetas {
		add(b)
	}
	for _, b := range strings.Split(clientHeader, ",") {
		add(b)
	}
	return strings.Join(out, ",")
}

type ClaudeProvider struct {
	client *http.Client
	mu     sync.Mutex

	// stable per-process identifiers for metadata.user_id
	uhash string // 64 hex
	acct  string // uuid
	sid   string // uuid
}

func NewClaudeProvider() *ClaudeProvider {
	return &ClaudeProvider{
		client: &http.Client{Timeout: 10 * time.Minute},
		uhash:  randHex(32),
		acct:   randUUID(),
		sid:    randUUID(),
	}
}

func (c *ClaudeProvider) Name() string { return "claude" }

func (c *ClaudeProvider) userID() string {
	return "user_" + c.uhash + "_account_" + c.acct + "_session_" + c.sid
}

func (c *ClaudeProvider) Status() ProviderStatus {
	st := ProviderStatus{
		Name: "claude", Title: "Claude · Anthropic", Endpoint: "/v1/messages",
		Models: []string{"claude-sonnet-4-5-20250929", "claude-opus-4-8", "claude-haiku-4-5-20251001"},
	}
	// Fast path (no Keychain shell on every status poll): ferridex cache or the file.
	if w, ok := loadClaudeCache(); ok && w.AccessToken != "" {
		st.LoggedIn = true
		st.Detail = "Claude 订阅(令牌已缓存)"
		return st
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
			st.LoggedIn = true
			st.Detail = "Claude 订阅(凭证文件)"
			return st
		}
	}
	st.Detail = "凭证可能在 Keychain;发一次 /v1/messages 即激活并缓存"
	return st
}

func (c *ClaudeProvider) Relay(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	patched := c.patchClaudeBody(rawBody)
	wantStream := requestWantsStream(rawBody)

	token, err := c.token(r.Context())
	if err != nil {
		http.Error(w, "claude auth: "+err.Error(), http.StatusUnauthorized)
		return
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, claudeMessagesURL, bytes.NewReader(patched))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", firstNonEmpty(r.Header.Get("Accept"), "application/json"))
	upstreamReq.Header.Set("Authorization", "Bearer "+token)
	upstreamReq.Header.Set("anthropic-version", anthropicVersion)
	upstreamReq.Header.Set("anthropic-beta", mergeBetas(r.Header.Get("anthropic-beta")))
	for k, v := range claudeMimicHeaders {
		upstreamReq.Header.Set(k, v)
	}

	upstreamResp, err := c.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstreamResp.Body.Close()

	// Anthropic native protocol on both ends — transparent passthrough.
	w.Header().Set("Content-Type", firstNonEmpty(upstreamResp.Header.Get("Content-Type"), "application/json"))
	w.WriteHeader(upstreamResp.StatusCode)
	if wantStream {
		streamCopy(w, upstreamResp.Body)
	} else {
		_, _ = io.Copy(w, upstreamResp.Body)
	}
}

// patchClaudeBody makes the request look like the official Claude Code CLI:
// normalize model id, ensure the Claude Code system identity is the first system
// block, and add a well-formed metadata.user_id if missing.
func (c *ClaudeProvider) patchClaudeBody(raw []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return raw
	}

	if m, ok := payload["model"].(string); ok {
		if full, ok := claudeModelOverrides[m]; ok {
			payload["model"] = full
		}
	}

	idBlock := map[string]any{"type": "text", "text": claudeSystemID}
	switch sys := payload["system"].(type) {
	case nil:
		payload["system"] = []any{idBlock}
	case string:
		if sys == claudeSystemID {
			payload["system"] = []any{idBlock}
		} else {
			payload["system"] = []any{idBlock, map[string]any{"type": "text", "text": sys}}
		}
	case []any:
		if !claudeHasIdentity(sys) {
			payload["system"] = append([]any{idBlock}, sys...)
		}
	}

	meta, _ := payload["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	if getString(meta, "user_id") == "" {
		meta["user_id"] = c.userID()
	}
	payload["metadata"] = meta

	out, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return out
}

func claudeHasIdentity(blocks []any) bool {
	if len(blocks) == 0 {
		return false
	}
	first, ok := blocks[0].(map[string]any)
	if !ok {
		return false
	}
	return getString(first, "text") == claudeSystemID
}

func streamCopy(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// --- token sourcing: ferridex cache -> local Claude Code login (file/Keychain) -> refresh ---

func (c *ClaudeProvider) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if w, ok := loadClaudeCache(); ok && claudeTokenValid(w.ExpiresAt) {
		return w.AccessToken, nil
	}

	access, refresh, expiresAt, _, err := loadClaudeSeed()
	if err != nil {
		return "", err
	}
	if claudeTokenValid(expiresAt) {
		saveClaudeCache(claudeWorking{access, refresh, expiresAt})
		return access, nil
	}
	if refresh == "" {
		return "", errors.New("access token 过期且无 refreshToken(重新登录 Claude Code)")
	}
	refreshed, err := refreshClaudeToken(ctx, refresh)
	if err != nil {
		return "", err
	}
	rt := refreshed.RefreshToken
	if rt == "" {
		rt = refresh
	}
	exp := time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	saveClaudeCache(claudeWorking{refreshed.AccessToken, rt, exp})
	return refreshed.AccessToken, nil
}

func claudeTokenValid(expiresAtMs int64) bool {
	return expiresAtMs > 0 && time.Now().Before(time.UnixMilli(expiresAtMs).Add(-claudeRefreshSkew))
}

// loadClaudeSeed reads the Claude Code OAuth credential from the local login:
// ~/.claude/.credentials.json first, else the macOS Keychain.
func loadClaudeSeed() (access, refresh string, expiresAt int64, subType string, err error) {
	if home, e := os.UserHomeDir(); e == nil {
		if b, e := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json")); e == nil {
			if a, r, exp, st, ok := parseClaudeOAuthJSON(b); ok {
				return a, r, exp, st, nil
			}
		}
	}
	if runtime.GOOS == "darwin" {
		for _, svc := range []string{claudeKeychainSvc, "Claude Code"} {
			out, e := exec.Command("security", "find-generic-password", "-s", svc, "-w").Output()
			if e == nil && len(bytes.TrimSpace(out)) > 0 {
				if a, r, exp, st, ok := parseClaudeOAuthJSON(out); ok {
					return a, r, exp, st, nil
				}
			}
		}
	}
	return "", "", 0, "", errors.New("未找到 Claude Code 凭证(~/.claude/.credentials.json 或 Keychain 'Claude Code-credentials')")
}

func parseClaudeOAuthJSON(b []byte) (access, refresh string, expiresAt int64, subType string, ok bool) {
	var doc map[string]any
	if json.Unmarshal(bytes.TrimSpace(b), &doc) != nil {
		return
	}
	o, has := getMap(doc, "claudeAiOauth")
	if !has {
		return
	}
	access = getString(o, "accessToken")
	refresh = getString(o, "refreshToken")
	if v, k := o["expiresAt"].(float64); k {
		expiresAt = int64(v)
	}
	subType = getString(o, "subscriptionType")
	ok = access != ""
	return
}

// claudeWorking is ferridex's own refreshed-token cache (never the Keychain item).
type claudeWorking struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // ms epoch
}

func claudeCachePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ferridex", "claude-creds.json")
}

func loadClaudeCache() (claudeWorking, bool) {
	var w claudeWorking
	b, err := os.ReadFile(claudeCachePath())
	if err != nil {
		return w, false
	}
	if json.Unmarshal(b, &w) != nil || w.AccessToken == "" {
		return w, false
	}
	return w, true
}

func saveClaudeCache(w claudeWorking) {
	p := claudeCachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.MarshalIndent(w, "", "  ")
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

type claudeRefreshResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func refreshClaudeToken(ctx context.Context, refreshToken string) (*claudeRefreshResp, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeClientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "axios/1.13.6")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude token refresh network error: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("claude token refresh HTTP %d: %s", resp.StatusCode, truncate(string(rb), 500))
	}
	var out claudeRefreshResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("claude refresh: response had no access_token")
	}
	return &out, nil
}

// --- small id helpers for metadata.user_id ---

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
