package provider

// CursorProvider relays Connect-RPC traffic to Cursor's official backends using
// the local Cursor CLI subscription login (~/.config/cursor/auth.json).

import (
	"bytes"
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"ferridex/internal/util"

	"golang.org/x/net/http2"
)

const (
	cursorAPI2Base    = "https://api2.cursor.sh"
	cursorAgentBase   = "https://agentn.global.api5.cursor.sh"
	cursorTokenURL    = cursorAPI2Base + "/oauth/token"
	cursorClientID    = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"
	cursorRefreshSkew = 60 * time.Second
)

var cursorAuthMu sync.Mutex

type CursorProvider struct {
	transport *http.Transport
	client    *http.Client
}

func NewCursorProvider() *CursorProvider {
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: true,
	}
	_ = http2.ConfigureTransport(transport)
	return &CursorProvider{
		transport: transport,
		client:    &http.Client{Transport: transport, Timeout: 10 * time.Minute},
	}
}

func (c *CursorProvider) Name() string { return "cursor" }

func (c *CursorProvider) Status() ProviderStatus {
	st := ProviderStatus{
		Name:     "cursor",
		Title:    "Cursor · Agent CLI",
		Endpoint: "Connect-RPC (CURSOR_API_ENDPOINT)",
		Models:   []string{"composer-2.5", "auto"},
	}
	if w, ok := loadCursorCache(); ok && w.AccessToken != "" {
		st.LoggedIn = true
		st.Detail = "Cursor 订阅(令牌已缓存)"
		if email := cursorAccountEmail(); email != "" {
			st.Account = email
		}
		return st
	}
	access, refresh, _, err := loadCursorSeed()
	if err != nil || access == "" {
		st.Detail = "未找到 Cursor 登录(请先 cursor agent login)"
		return st
	}
	st.LoggedIn = refresh != "" || cursorTokenValid(access)
	st.Detail = "Cursor 订阅"
	if email := cursorAccountEmail(); email != "" {
		st.Account = email
	}
	return st
}

func (c *CursorProvider) Relay(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		defer r.Body.Close()
	}

	token, err := borrowCursorToken(r.Context())
	if err != nil {
		http.Error(w, "cursor auth: "+err.Error(), http.StatusUnauthorized)
		return
	}

	upstreamBase := cursorUpstreamBase(r.URL.Path)
	target, err := url.Parse(upstreamBase)
	if err != nil {
		http.Error(w, "invalid upstream: "+err.Error(), http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = c.transport
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		req.Header.Set("Authorization", "Bearer "+token)
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, e error) {
		log.Printf("cursor upstream %s %s: %v", req.Method, req.URL.Path, e)
		http.Error(rw, "upstream request failed: "+e.Error(), http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Alt-Svc")
		return nil
	}
	proxy.ServeHTTP(w, r)
}

func IsCursorRelayPath(r *http.Request) bool {
	path := r.URL.Path
	if path == "/" || path == "/healthz" || strings.HasPrefix(path, "/api/") {
		return false
	}
	switch path {
	case "/v1/responses", "/responses", "/v1/messages", "/messages":
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return false
	}
	return IsCursorConnectPath(path)
}

func IsCursorConnectPath(path string) bool {
	if strings.HasPrefix(path, "/agent.v1.") {
		return true
	}
	if strings.HasPrefix(path, "/aiserver.v1.") {
		return true
	}
	if strings.HasPrefix(path, "/auth/") {
		return true
	}
	if strings.HasPrefix(path, "/oauth/") {
		return true
	}
	if strings.HasPrefix(path, "/v1/traces") {
		return true
	}
	return false
}

func IsCursorLANPath(path string) bool {
	return IsCursorConnectPath(path)
}

func cursorUpstreamBase(path string) string {
	if strings.HasPrefix(path, "/agent.v1.") {
		return cursorAgentBase
	}
	if strings.HasPrefix(path, "/aiserver.v1.ChatService/") {
		return cursorAgentBase
	}
	return cursorAPI2Base
}

func cursorAccountEmail() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".cursor", "cli-config.json"))
	if err != nil {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	authInfo, _ := util.GetMap(doc, "authInfo")
	return util.GetString(authInfo, "email")
}

func borrowCursorToken(ctx context.Context) (string, error) {
	cursorAuthMu.Lock()
	defer cursorAuthMu.Unlock()
	return cursorTokenLocked(ctx)
}

func cursorTokenLocked(ctx context.Context) (string, error) {
	if w, ok := loadCursorCache(); ok && cursorTokenValid(w.AccessToken) {
		return w.AccessToken, nil
	}
	access, refresh, expiresAt, err := loadCursorSeed()
	if err != nil {
		return "", err
	}
	if access != "" && cursorTokenValid(access) {
		saveCursorCache(cursorWorking{AccessToken: access, RefreshToken: refresh, ExpiresAt: expiresAt})
		return access, nil
	}
	if refresh == "" {
		return "", errors.New("access token 过期且无 refreshToken(请重新 cursor agent login)")
	}
	refreshed, err := refreshCursorToken(ctx, refresh)
	if err != nil {
		return "", err
	}
	rt := refreshed.RefreshToken
	if rt == "" {
		rt = refresh
	}
	exp := time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	if refreshed.AccessToken == "" {
		return "", errors.New("cursor refresh: empty access_token")
	}
	saveCursorCache(cursorWorking{AccessToken: refreshed.AccessToken, RefreshToken: rt, ExpiresAt: exp})
	return refreshed.AccessToken, nil
}

func cursorTokenValid(token string) bool {
	if token == "" {
		return false
	}
	exp, ok := util.JWTExpiry(token)
	if !ok {
		return true
	}
	return time.Now().Add(cursorRefreshSkew).Before(exp)
}

func loadCursorSeed() (access, refresh string, expiresAt int64, err error) {
	if w, ok := loadCursorCache(); ok && w.AccessToken != "" {
		return w.AccessToken, w.RefreshToken, w.ExpiresAt, nil
	}
	for _, path := range cursorAuthPaths() {
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var doc struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		}
		if json.Unmarshal(b, &doc) != nil || doc.AccessToken == "" {
			continue
		}
		exp := int64(0)
		if t, ok := util.JWTExpiry(doc.AccessToken); ok {
			exp = t.UnixMilli()
		}
		return doc.AccessToken, doc.RefreshToken, exp, nil
	}
	if access, refresh, ok := loadCursorIDEKeychain(); ok {
		exp := int64(0)
		if t, ok := util.JWTExpiry(access); ok {
			exp = t.UnixMilli()
		}
		return access, refresh, exp, nil
	}
	if access, refresh, ok := loadCursorIDESQLite(); ok {
		exp := int64(0)
		if t, ok := util.JWTExpiry(access); ok {
			exp = t.UnixMilli()
		}
		return access, refresh, exp, nil
	}
	return "", "", 0, errors.New("未找到 Cursor 凭据(~/.config/cursor/auth.json)")
}

func cursorAuthPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var paths []string
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			paths = append(paths, filepath.Join(appData, "cursor", "auth.json"))
		}
	default:
		paths = append(paths, filepath.Join(home, ".config", "cursor", "auth.json"))
		if runtime.GOOS == "darwin" {
			paths = append(paths, filepath.Join(home, "Library", "Application Support", "cursor", "auth.json"))
		}
	}
	return paths
}

func cursorIDEStateDB() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Cursor", "User", "globalStorage", "state.vscdb")
		}
	default:
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
	return ""
}

func loadCursorIDESQLite() (access, refresh string, ok bool) {
	dbPath := cursorIDEStateDB()
	if dbPath == "" {
		return "", "", false
	}
	if _, err := os.Stat(dbPath); err != nil {
		return "", "", false
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return "", "", false
	}
	readKey := func(key string) string {
		out, err := exec.Command("sqlite3", dbPath, "SELECT value FROM ItemTable WHERE key = ?;", key).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	access = readKey("cursorAuth/accessToken")
	refresh = readKey("cursorAuth/refreshToken")
	if access == "" {
		return "", "", false
	}
	return access, refresh, true
}

func loadCursorIDEKeychain() (access, refresh string, ok bool) {
	if runtime.GOOS != "darwin" {
		return "", "", false
	}
	out, err := exec.Command("security", "find-generic-password", "-s", "cursor-access-token", "-w").Output()
	if err != nil {
		return "", "", false
	}
	access = strings.TrimSpace(string(out))
	if access == "" {
		return "", "", false
	}
	return access, "", true
}

type cursorWorking struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at_ms,omitempty"`
}

func cursorCachePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ferridex", "cursor-creds.json")
}

func loadCursorCache() (cursorWorking, bool) {
	b, err := os.ReadFile(cursorCachePath())
	if err != nil {
		return cursorWorking{}, false
	}
	var w cursorWorking
	if json.Unmarshal(b, &w) != nil || w.AccessToken == "" {
		return cursorWorking{}, false
	}
	return w, true
}

func saveCursorCache(w cursorWorking) {
	p := cursorCachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.MarshalIndent(w, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

type cursorRefreshResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	ShouldLogout bool   `json:"shouldLogout"`
}

func refreshCursorToken(ctx context.Context, refreshToken string) (*cursorRefreshResp, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     cursorClientID,
		"refresh_token": refreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorTokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursor token refresh network error: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cursor token refresh HTTP %d: %s", resp.StatusCode, util.Truncate(string(rb), 500))
	}
	var out cursorRefreshResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, err
	}
	if out.ShouldLogout {
		return nil, errors.New("cursor refresh token 已失效(请重新 cursor agent login)")
	}
	if out.AccessToken == "" {
		return nil, errors.New("cursor refresh: response had no access_token")
	}
	if out.ExpiresIn == 0 {
		out.ExpiresIn = 3600
	}
	return &out, nil
}
