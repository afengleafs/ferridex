package provider

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
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"ferridex/internal/util"
)

const (
	claudeClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeTokenURL    = "https://platform.claude.com/v1/oauth/token"
	claudeMessagesURL = "https://api.anthropic.com/v1/messages"
	claudeUsageURL    = "https://api.anthropic.com/api/oauth/usage"
	anthropicVersion  = "2023-06-01"
	claudeCLIVersion  = "2.1.161"
	claudeSystemID    = "You are Claude Code, Anthropic's official CLI for Claude."
	claudeRefreshSkew = 60 * time.Second
	claudeKeychainSvc = "Claude Code-credentials"

	claudeTransientRateLimitCooldown = 5 * time.Second
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
	client      *http.Client
	accessToken func(context.Context) (string, error)
	now         func() time.Time
	mu          sync.Mutex
	limitMu     sync.Mutex
	limit       *claudeRateLimitState

	// on-demand subscription usage query (dashboard button); seam for tests.
	usageFetcher func(context.Context) ([]UsageWindow, error)

	// stable per-process identifiers for metadata.user_id
	uhash string // 64 hex
	acct  string // uuid
	sid   string // uuid
}

func NewClaudeProvider() *ClaudeProvider {
	c := &ClaudeProvider{
		client: &http.Client{Timeout: 10 * time.Minute},
		now:    time.Now,
		uhash:  randHex(32),
		acct:   randUUID(),
		sid:    randUUID(),
	}
	c.usageFetcher = c.fetchUsage
	return c
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
	} else if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
			st.LoggedIn = true
			st.Detail = "Claude 订阅(凭证文件)"
		}
	}
	if st.Detail == "" {
		st.Detail = "凭证可能在 Keychain;发一次 /v1/messages 即激活并缓存"
	}
	if limit, ok := c.activeRateLimit(); ok {
		st.Detail += fmt.Sprintf("; %s，恢复时间 %s", limit.label, limit.until.Local().Format("2006-01-02 15:04:05 MST"))
	}
	return st
}

func (c *ClaudeProvider) Relay(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	if limit, ok := c.activeRateLimit(); ok {
		writeClaudeRateLimitResponse(w, limit, c.nowTime())
		return
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	patched := c.patchClaudeBody(rawBody)
	wantStream := requestWantsStream(rawBody)

	token, err := c.getAccessToken(r.Context())
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
	upstreamReq.Header.Set("Accept", util.FirstNonEmpty(r.Header.Get("Accept"), "application/json"))
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

	if upstreamResp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(upstreamResp.Body)
		limit := newClaudeRateLimitState(upstreamResp.Header, body, c.nowTime())
		c.setRateLimit(limit)
		log.Printf("Claude 429 分类: %s; 恢复时间 %s; 禁止 Claude Code 重试=%t",
			limit.label, limit.until.Local().Format("2006-01-02 15:04:05 MST"), limit.stopRetries)
		writeClaudeRateLimitResponse(w, limit, c.nowTime())
		return
	}

	// Anthropic native protocol on both ends — transparent passthrough.
	writeClaudeResponseHeaders(w.Header(), upstreamResp.Header)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(upstreamResp.StatusCode)
	if wantStream {
		streamCopy(w, upstreamResp.Body)
	} else {
		_, _ = io.Copy(w, upstreamResp.Body)
	}
}

type claudeRateLimitState struct {
	until       time.Time
	body        []byte
	header      http.Header
	label       string
	stopRetries bool
}

func (c *ClaudeProvider) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *ClaudeProvider) getAccessToken(ctx context.Context) (string, error) {
	if c.accessToken != nil {
		return c.accessToken(ctx)
	}
	return c.token(ctx)
}

func (c *ClaudeProvider) activeRateLimit() (*claudeRateLimitState, bool) {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()

	if c.limit == nil {
		return nil, false
	}
	if !c.nowTime().Before(c.limit.until) {
		c.limit = nil
		return nil, false
	}
	return cloneClaudeRateLimitState(c.limit), true
}

func (c *ClaudeProvider) setRateLimit(limit *claudeRateLimitState) {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()

	if c.limit != nil && c.nowTime().Before(c.limit.until) && c.limit.stopRetries {
		if !limit.stopRetries || !limit.until.After(c.limit.until) {
			return
		}
	}
	c.limit = cloneClaudeRateLimitState(limit)
}

func cloneClaudeRateLimitState(limit *claudeRateLimitState) *claudeRateLimitState {
	if limit == nil {
		return nil
	}
	out := *limit
	out.body = append([]byte(nil), limit.body...)
	out.header = limit.header.Clone()
	return &out
}

func newClaudeRateLimitState(header http.Header, body []byte, now time.Time) *claudeRateLimitState {
	until, scope, exhausted := claudeQuotaReset(header, now)
	if !exhausted {
		until = now.Add(claudeTransientRateLimitCooldown)
		scope = "Claude 临时限流"
	}
	return &claudeRateLimitState{
		until:       until,
		body:        append([]byte(nil), body...),
		header:      filteredClaudeResponseHeaders(header),
		label:       scope,
		stopRetries: exhausted,
	}
}

func claudeQuotaReset(header http.Header, now time.Time) (time.Time, string, bool) {
	type window struct {
		name  string
		reset time.Time
	}
	var available []window
	var exhausted []window
	for _, name := range []string{"5h", "7d"} {
		prefix := "anthropic-ratelimit-unified-" + name + "-"
		reset, ok := parseClaudeResetTime(header.Get(prefix + "reset"))
		if !ok || !reset.After(now) {
			continue
		}
		candidate := window{name: name, reset: reset}
		available = append(available, candidate)
		if claudeWindowExceeded(header, prefix) {
			exhausted = append(exhausted, candidate)
		}
	}

	if len(exhausted) > 0 {
		chosen := exhausted[0]
		for _, candidate := range exhausted[1:] {
			if candidate.reset.After(chosen.reset) {
				chosen = candidate
			}
		}
		return chosen.reset, claudeQuotaLabel(chosen.name), true
	}

	// Anthropic sometimes returns window reset headers without a reliable
	// utilization/threshold signal. On a 429, sub2api treats the sooner official
	// window reset as the best available quota signal.
	if len(available) > 0 {
		chosen := available[0]
		for _, candidate := range available[1:] {
			if candidate.reset.Before(chosen.reset) {
				chosen = candidate
			}
		}
		return chosen.reset, claudeQuotaLabel(chosen.name), true
	}

	if reset, ok := parseClaudeResetTime(header.Get("anthropic-ratelimit-unified-reset")); ok && reset.After(now) {
		return reset, "Claude 限额已耗尽", true
	}
	return time.Time{}, "", false
}

func claudeQuotaLabel(window string) string {
	if window == "7d" {
		return "Claude 7 天限额已耗尽"
	}
	return "Claude 5 小时限额已耗尽"
}

func claudeWindowExceeded(header http.Header, prefix string) bool {
	if strings.EqualFold(strings.TrimSpace(header.Get(prefix+"surpassed-threshold")), "true") {
		return true
	}
	utilization := strings.TrimSpace(header.Get(prefix + "utilization"))
	if utilization == "" {
		return false
	}
	value, err := strconv.ParseFloat(utilization, 64)
	if err != nil {
		return false
	}
	return value >= 1.0-1e-9
}

func parseClaudeResetTime(raw string) (time.Time, bool) {
	timestamp, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || timestamp <= 0 {
		return time.Time{}, false
	}
	if timestamp > 1e11 {
		timestamp /= 1000
	}
	return time.Unix(timestamp, 0), true
}

func writeClaudeRateLimitResponse(w http.ResponseWriter, limit *claudeRateLimitState, now time.Time) {
	writeClaudeResponseHeaders(w.Header(), limit.header)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	if limit.stopRetries {
		w.Header().Set("X-Should-Retry", "false")
		remaining := limit.until.Sub(now)
		seconds := int64((remaining + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(limit.body)
}

func filteredClaudeResponseHeaders(src http.Header) http.Header {
	dst := make(http.Header)
	writeClaudeResponseHeaders(dst, src)
	return dst
}

func writeClaudeResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if lower != "content-type" &&
			lower != "retry-after" &&
			lower != "request-id" &&
			lower != "x-request-id" &&
			!strings.HasPrefix(lower, "anthropic-ratelimit-") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
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
	if util.GetString(meta, "user_id") == "" {
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
	return util.GetString(first, "text") == claudeSystemID
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

// --- subscription usage (undocumented OAuth usage endpoint, mirrors sub2api) ---

// QueryUsage fetches the usage windows on demand — only when the dashboard's
// 查询用量 button is clicked for this provider; nothing is cached or polled.
func (c *ClaudeProvider) QueryUsage(ctx context.Context) ([]UsageWindow, error) {
	return c.usageFetcher(ctx)
}

func (c *ClaudeProvider) fetchUsage(ctx context.Context) ([]UsageWindow, error) {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	for k, v := range claudeMimicHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("usage HTTP %d: %s", resp.StatusCode, util.Truncate(string(body), 300))
	}
	windows, ok := parseClaudeUsage(body)
	if !ok {
		return nil, errors.New("usage: unrecognized response shape")
	}
	return windows, nil
}

// parseClaudeUsage maps the /api/oauth/usage response into display windows. The
// endpoint is undocumented, so parse defensively: tolerate a 0..1 fraction or a
// 0..100 percentage, and a reset given as an ISO-8601 string or a unix epoch.
func parseClaudeUsage(body []byte) ([]UsageWindow, bool) {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return nil, false
	}
	windows := []struct{ key, label string }{
		{"five_hour", "5 小时"},
		{"seven_day", "7 天"},
	}
	out := make([]UsageWindow, 0, len(windows))
	for _, w := range windows {
		win, ok := util.GetMap(doc, w.key)
		if !ok {
			continue
		}
		util, ok := claudeUsageFraction(win["utilization"])
		if !ok {
			continue
		}
		out = append(out, UsageWindow{
			Label:       w.label,
			Utilization: util,
			ResetsAt:    claudeUsageReset(win["resets_at"]),
		})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// claudeUsageFraction normalizes a utilization value to a 0..1 fraction.
func claudeUsageFraction(v any) (float64, bool) {
	f, ok := toFloat(v)
	if !ok {
		return 0, false
	}
	if f > 1 {
		f /= 100
	}
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return f, true
}

// claudeUsageReset parses a reset marker into unix seconds (0 when unknown).
func claudeUsageReset(v any) int64 {
	switch t := v.(type) {
	case string:
		if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(t)); err == nil {
			return ts.Unix()
		}
	case float64:
		n := int64(t)
		if n > 1e11 { // milliseconds
			n /= 1000
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
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
	o, has := util.GetMap(doc, "claudeAiOauth")
	if !has {
		return
	}
	access = util.GetString(o, "accessToken")
	refresh = util.GetString(o, "refreshToken")
	if v, k := o["expiresAt"].(float64); k {
		expiresAt = int64(v)
	}
	subType = util.GetString(o, "subscriptionType")
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
		return nil, fmt.Errorf("claude token refresh HTTP %d: %s", resp.StatusCode, util.Truncate(string(rb), 500))
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
