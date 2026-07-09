package provider

// GrokProvider relays Grok CLI traffic to xAI's CLI chat proxy using the local
// grok login (~/.grok/auth.json). Grok points at a custom proxy via the
// GROK_CLI_CHAT_PROXY_BASE_URL env var (its equivalent of Cursor's -e); ferridex
// serves it under the /grok prefix, strips the prefix, swaps the placeholder
// bearer token for the real OIDC token and forwards to cli-chat-proxy.grok.com.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ferridex/internal/util"

	"golang.org/x/net/http2"
)

const (
	grokUpstreamBase    = "https://cli-chat-proxy.grok.com"
	grokPathPrefix      = "/grok" // panel hands out base_url .../grok/v1
	grokTokenURL        = "https://auth.x.ai/oauth2/token"
	grokDefaultClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	grokRefreshSkew     = 60 * time.Second
)

var grokAuthMu sync.Mutex

type GrokProvider struct {
	transport *http.Transport
	client    *http.Client
}

func NewGrokProvider() *GrokProvider {
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: true,
	}
	_ = http2.ConfigureTransport(transport)
	return &GrokProvider{
		transport: transport,
		client:    &http.Client{Transport: transport, Timeout: 10 * time.Minute},
	}
}

func (g *GrokProvider) Name() string { return "grok" }

func (g *GrokProvider) Status() ProviderStatus {
	st := ProviderStatus{
		Name:     "grok",
		Title:    "Grok · CLI",
		Endpoint: "GROK_CLI_CHAT_PROXY_BASE_URL (/grok/v1)",
		Models:   grokModels(),
	}
	if w, ok := loadGrokCache(); ok && w.AccessToken != "" {
		st.LoggedIn = true
		st.Detail = "Grok 订阅(令牌已缓存)"
		if email := grokAccountEmail(); email != "" {
			st.Account = email
		}
		return st
	}
	_, e, ok := loadGrokBestEntry()
	if !ok || (e.Key == "" && e.RefreshToken == "") {
		st.Detail = "未找到 Grok 登录(请先 grok login)"
		return st
	}
	st.LoggedIn = e.RefreshToken != "" || grokTokenValid(e.Key)
	st.Detail = "Grok 订阅"
	if email := grokAccountEmail(); email != "" {
		st.Account = email
	}
	return st
}

func (g *GrokProvider) Relay(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		defer r.Body.Close()
	}

	token, err := borrowGrokToken(r.Context())
	if err != nil {
		http.Error(w, "grok auth: "+err.Error(), http.StatusUnauthorized)
		return
	}

	target, err := url.Parse(grokUpstreamBase)
	if err != nil {
		http.Error(w, "invalid upstream: "+err.Error(), http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = g.transport
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		// Strip the /grok prefix so the upstream sees /v1/chat/completions etc.
		req.URL.Path = grokUpstreamPath(req.URL.Path)
		if req.URL.RawPath != "" {
			req.URL.RawPath = grokUpstreamPath(req.URL.RawPath)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		// Force the CLI session-token auth mode so the proxy accepts the swapped
		// token regardless of how the downstream grok sent credentials.
		req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, e error) {
		log.Printf("grok upstream %s %s: %v", req.Method, req.URL.Path, e)
		http.Error(rw, "upstream request failed: "+e.Error(), http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Alt-Svc")
		return nil
	}
	proxy.ServeHTTP(w, r)
}

// IsGrokPath reports whether a path is served by the grok provider.
func IsGrokPath(path string) bool {
	return strings.HasPrefix(path, grokPathPrefix+"/")
}

// grokUpstreamPath maps an incoming /grok/... path to the upstream path by
// dropping the /grok prefix (e.g. /grok/v1/models -> /v1/models).
func grokUpstreamPath(path string) string {
	return strings.TrimPrefix(path, grokPathPrefix)
}

func IsGrokRelayPath(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return false
	}
	return IsGrokPath(r.URL.Path)
}

func IsGrokLANPath(path string) bool {
	return IsGrokPath(path)
}

// --- credentials ---

func grokHome() string {
	if h := os.Getenv("GROK_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".grok")
}

func grokAuthPath() string {
	if p := os.Getenv("GROK_AUTH_PATH"); p != "" {
		return p
	}
	return filepath.Join(grokHome(), "auth.json")
}

type grokAuthEntry struct {
	Key          string `json:"key"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	ClientID     string `json:"oidc_client_id"`
	Email        string `json:"email"`
}

// loadGrokBestEntry reads ~/.grok/auth.json (a map keyed by issuer::client_id,
// kept fresh by grok's own silent refresh) and returns the most usable entry
// plus its map key: prefer a valid access token, then one with a refresh token.
func loadGrokBestEntry() (entryKey string, e grokAuthEntry, ok bool) {
	b, err := os.ReadFile(grokAuthPath())
	if err != nil {
		return "", grokAuthEntry{}, false
	}
	var m map[string]grokAuthEntry
	if json.Unmarshal(b, &m) != nil {
		return "", grokAuthEntry{}, false
	}
	for k, cur := range m {
		if cur.Key == "" && cur.RefreshToken == "" {
			continue
		}
		if !ok {
			entryKey, e, ok = k, cur, true
			continue
		}
		if grokTokenValid(cur.Key) && !grokTokenValid(e.Key) {
			entryKey, e = k, cur
		} else if cur.RefreshToken != "" && e.RefreshToken == "" {
			entryKey, e = k, cur
		}
	}
	return entryKey, e, ok
}

func grokAccountEmail() string {
	if _, e, ok := loadGrokBestEntry(); ok {
		return e.Email
	}
	return ""
}

func grokModels() []string {
	fallback := []string{"grok-4.5", "grok-build"}
	b, err := os.ReadFile(filepath.Join(grokHome(), "models_cache.json"))
	if err != nil {
		return fallback
	}
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(b, &doc) != nil || len(doc.Models) == 0 {
		return fallback
	}
	out := make([]string, 0, len(doc.Models))
	for k := range doc.Models {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// grokRefreshAndCache refreshes via OIDC, caches the result, and writes the
// rotated token back to ~/.grok/auth.json so grok and ferridex stay in sync
// (xAI rotates refresh tokens: refreshing with a stale one is rejected).
func grokRefreshAndCache(ctx context.Context, entryKey, refresh, clientID string) (string, error) {
	refreshed, err := refreshGrokToken(ctx, refresh, clientID)
	if err != nil {
		return "", err
	}
	if refreshed.AccessToken == "" {
		return "", errors.New("grok refresh: empty access_token")
	}
	rt := refreshed.RefreshToken
	if rt == "" {
		rt = refresh
	}
	exp := time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	saveGrokCache(grokWorking{AccessToken: refreshed.AccessToken, RefreshToken: rt, ExpiresAt: exp, ClientID: clientID})
	if entryKey != "" {
		writeGrokAuthFileToken(entryKey, refreshed.AccessToken, rt, refreshed.ExpiresIn)
	}
	return refreshed.AccessToken, nil
}

// writeGrokAuthFileToken merges a refreshed token back into ~/.grok/auth.json,
// preserving all other fields of the entry. Best-effort and atomic.
func writeGrokAuthFileToken(entryKey, access, refresh string, expiresIn int) {
	p := grokAuthPath()
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var m map[string]map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	entry, ok := m[entryKey]
	if !ok {
		return
	}
	entry["key"] = access
	entry["refresh_token"] = refresh
	if expiresIn > 0 {
		entry["expires_at"] = time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, out, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

func borrowGrokToken(ctx context.Context) (string, error) {
	grokAuthMu.Lock()
	defer grokAuthMu.Unlock()
	return grokTokenLocked(ctx)
}

func grokTokenLocked(ctx context.Context) (string, error) {
	// Fast path: a cached access token that is still valid.
	if w, ok := loadGrokCache(); ok && grokTokenValid(w.AccessToken) {
		return w.AccessToken, nil
	}
	// Authoritative source: the token grok keeps fresh in ~/.grok/auth.json.
	// Always re-read it here — never cling to a stale cached refresh token,
	// since xAI rotates refresh tokens and grok may have already refreshed.
	entryKey, e, ok := loadGrokBestEntry()
	if ok && e.Key != "" && grokTokenValid(e.Key) {
		exp := int64(0)
		if t, okExp := util.JWTExpiry(e.Key); okExp {
			exp = t.UnixMilli()
		}
		cid := e.ClientID
		if cid == "" {
			cid = grokDefaultClientID
		}
		saveGrokCache(grokWorking{AccessToken: e.Key, RefreshToken: e.RefreshToken, ExpiresAt: exp, ClientID: cid})
		return e.Key, nil
	}
	// Access token expired everywhere → refresh with the freshest refresh token
	// available (prefer the file's, fall back to the cache's).
	refresh, clientID := "", grokDefaultClientID
	if ok {
		refresh = e.RefreshToken
		if e.ClientID != "" {
			clientID = e.ClientID
		}
	}
	if refresh == "" {
		if w, okCache := loadGrokCache(); okCache {
			refresh = w.RefreshToken
			if w.ClientID != "" {
				clientID = w.ClientID
			}
		}
	}
	if refresh == "" {
		return "", errors.New("未找到有效 Grok 令牌(access 过期且无 refresh_token;请在本机运行一次 grok 刷新登录)")
	}
	return grokRefreshAndCache(ctx, entryKey, refresh, clientID)
}

func grokTokenValid(token string) bool {
	if token == "" {
		return false
	}
	exp, ok := util.JWTExpiry(token)
	if !ok {
		return true
	}
	return time.Now().Add(grokRefreshSkew).Before(exp)
}

type grokWorking struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at_ms,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
}

func grokCachePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ferridex", "grok-creds.json")
}

func loadGrokCache() (grokWorking, bool) {
	b, err := os.ReadFile(grokCachePath())
	if err != nil {
		return grokWorking{}, false
	}
	var w grokWorking
	if json.Unmarshal(b, &w) != nil || w.AccessToken == "" {
		return grokWorking{}, false
	}
	return w, true
}

func saveGrokCache(w grokWorking) {
	p := grokCachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.MarshalIndent(w, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

type grokRefreshResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func refreshGrokToken(ctx context.Context, refreshToken, clientID string) (*grokRefreshResp, error) {
	if clientID == "" {
		clientID = grokDefaultClientID
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grokTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok token refresh network error: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("grok token refresh HTTP %d: %s", resp.StatusCode, util.Truncate(string(rb), 500))
	}
	var out grokRefreshResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("grok refresh: response had no access_token")
	}
	if out.ExpiresIn == 0 {
		out.ExpiresIn = 3600
	}
	return &out, nil
}
