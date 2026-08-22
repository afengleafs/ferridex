package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ferridex/internal/provider"
)

const testClaudeProfileToken = "sk-or-test-token-abcdef"

type fakeClaudeSubscriptionProvider struct{}

func (fakeClaudeSubscriptionProvider) Name() string { return "claude" }

func (fakeClaudeSubscriptionProvider) Relay(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (fakeClaudeSubscriptionProvider) Status() provider.ProviderStatus {
	return provider.ProviderStatus{
		Name:     "claude",
		Title:    "Test Claude",
		LoggedIn: true,
		Endpoint: "/v1/messages",
	}
}

// Loopback base URLs keep every test off the real network.
const testClaudeProfilesEnv = `[gateway]
ANTHROPIC_BASE_URL="http://127.0.0.1:8082"
ANTHROPIC_AUTH_TOKEN="` + testClaudeProfileToken + `"
ANTHROPIC_DEFAULT_SONNET_MODEL="stealth/ox-alpha"

[broken]
ANTHROPIC_BASE_URL="not a url"
`

func newTestClaudeUpstreamManager(t *testing.T) (*claudeUpstreamManager, string, string) {
	t.Helper()
	subscription := fakeClaudeSubscriptionProvider{}
	runtime := provider.NewClaudeUpstreamProvider(subscription)
	configPath := filepath.Join(t.TempDir(), ".ferridex", "config.json")
	profilesPath := filepath.Join(t.TempDir(), "ferridex-profiles.env")
	manager := &claudeUpstreamManager{
		runtime:      runtime,
		subscription: subscription,
		persist: func(update func(*config) error) error {
			return updateConfigFile(configPath, update)
		},
		readPersisted: func() (config, error) {
			return readConfigFile(configPath)
		},
		profilesPath: profilesPath,
	}
	return manager, configPath, profilesPath
}

func writeClaudeProfiles(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write profiles file: %v", err)
	}
}

func persistedClaudeUpstream(t *testing.T, path string) string {
	t.Helper()
	cfg, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return cfg.ClaudeUpstream
}

func TestClaudeUpstreamGetRedactsCredentials(t *testing.T) {
	manager, _, profilesPath := newTestClaudeUpstreamManager(t)
	writeClaudeProfiles(t, profilesPath, testClaudeProfilesEnv)
	manager.loadProfiles()

	mux := http.NewServeMux()
	registerClaudeUpstreamAPI(mux, manager)
	response := upstreamAPIRequest(t, mux, http.MethodGet, "/api/claude-upstream", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"has_auth_token":true`) {
		t.Fatalf("missing has_auth_token flag: %s", body)
	}
	if strings.Contains(body, testClaudeProfileToken) {
		t.Fatalf("response leaked the profile token: %s", body)
	}
	if strings.Contains(body, `"auth_token":`) || strings.Contains(body, `"api_key":`) {
		t.Fatalf("response exposed secret fields: %s", body)
	}
	var decoded struct {
		Active   string `json:"active"`
		Profiles []struct {
			Name     string `json:"name"`
			BaseURL  string `json:"base_url"`
			AuthMode string `json:"auth_mode"`
			Valid    bool   `json:"valid"`
		} `json:"profiles"`
		File string `json:"file"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Active != "" || len(decoded.Profiles) != 2 || !decoded.Profiles[0].Valid ||
		decoded.Profiles[0].AuthMode != "bearer" || decoded.Profiles[0].BaseURL != "http://127.0.0.1:8082" {
		t.Fatalf("unexpected view: %#v", decoded)
	}
	if decoded.File != profilesPath {
		t.Fatalf("file = %q, want %q", decoded.File, profilesPath)
	}
}

func TestClaudeUpstreamActivatePersistsAndSwitches(t *testing.T) {
	manager, configPath, profilesPath := newTestClaudeUpstreamManager(t)
	writeClaudeProfiles(t, profilesPath, testClaudeProfilesEnv)
	manager.loadProfiles()

	mux := http.NewServeMux()
	registerClaudeUpstreamAPI(mux, manager)

	response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/claude-upstream/activate", `{"name":"gateway"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("activate status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := persistedClaudeUpstream(t, configPath); got != "gateway" {
		t.Fatalf("persisted = %q, want gateway", got)
	}
	if snapshot := manager.runtime.Snapshot(); snapshot.Active != provider.ClaudeUpstreamProfile || snapshot.ProfileName != "gateway" {
		t.Fatalf("snapshot = %#v", snapshot)
	}

	response = upstreamAPIRequest(t, mux, http.MethodPost, "/api/claude-upstream/activate", `{"name":""}`)
	if response.Code != http.StatusOK {
		t.Fatalf("back-to-subscription status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := persistedClaudeUpstream(t, configPath); got != "" {
		t.Fatalf("persisted = %q, want empty", got)
	}
	if snapshot := manager.runtime.Snapshot(); snapshot.Active != provider.ClaudeUpstreamSubscription {
		t.Fatalf("active = %q, want subscription", snapshot.Active)
	}
}

func TestClaudeUpstreamActivateUnknownDoesNotPersist(t *testing.T) {
	manager, configPath, profilesPath := newTestClaudeUpstreamManager(t)
	writeClaudeProfiles(t, profilesPath, testClaudeProfilesEnv)
	manager.loadProfiles()

	mux := http.NewServeMux()
	registerClaudeUpstreamAPI(mux, manager)
	response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/claude-upstream/activate", `{"name":"nope"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := persistedClaudeUpstream(t, configPath); got != "" {
		t.Fatalf("persisted = %q, want untouched", got)
	}
}

func TestClaudeUpstreamReloadPicksUpEditsAndFallsBack(t *testing.T) {
	manager, configPath, profilesPath := newTestClaudeUpstreamManager(t)
	writeClaudeProfiles(t, profilesPath, testClaudeProfilesEnv)
	manager.loadProfiles()

	mux := http.NewServeMux()
	registerClaudeUpstreamAPI(mux, manager)
	if response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/claude-upstream/activate", `{"name":"gateway"}`); response.Code != http.StatusOK {
		t.Fatalf("activate status = %d", response.Code)
	}

	// The active profile disappears from the file: runtime falls back to the
	// subscription while the persisted name is kept for a later reload.
	writeClaudeProfiles(t, profilesPath, "[other]\nANTHROPIC_BASE_URL=\"http://127.0.0.1:9009\"\nANTHROPIC_API_KEY=\"k\"\n")
	response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/claude-upstream/reload", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("reload status = %d, body = %s", response.Code, response.Body.String())
	}
	var decoded struct {
		Active       string `json:"active"`
		StaleProfile string `json:"stale_profile"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Active != "" || decoded.StaleProfile != "gateway" {
		t.Fatalf("view = %#v, want fallback with stale marker", decoded)
	}
	if got := persistedClaudeUpstream(t, configPath); got != "gateway" {
		t.Fatalf("persisted = %q, want retained", got)
	}
	if snapshot := manager.runtime.Snapshot(); snapshot.Active != provider.ClaudeUpstreamSubscription {
		t.Fatalf("active = %q, want subscription after fallback", snapshot.Active)
	}

	// Re-adding the profile and reloading restores it without a restart.
	writeClaudeProfiles(t, profilesPath, testClaudeProfilesEnv)
	response = upstreamAPIRequest(t, mux, http.MethodPost, "/api/claude-upstream/reload", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("reload status = %d", response.Code)
	}
	decoded = struct {
		Active       string `json:"active"`
		StaleProfile string `json:"stale_profile"`
	}{}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Active != "gateway" || decoded.StaleProfile != "" {
		t.Fatalf("view = %#v, want gateway restored", decoded)
	}
}

func TestConfigureClaudeProvidersWithConfig(t *testing.T) {
	tests := []struct {
		name        string
		persisted   string
		wantActive  provider.ClaudeUpstream
		wantProfile string
		wantStale   string
	}{
		{name: "empty restores subscription", persisted: "", wantActive: provider.ClaudeUpstreamSubscription},
		{
			name:        "known profile restored",
			persisted:   "gateway",
			wantActive:  provider.ClaudeUpstreamProfile,
			wantProfile: "gateway",
		},
		{
			name:       "unknown profile falls back but stays stale",
			persisted:  "ghost",
			wantActive: provider.ClaudeUpstreamSubscription,
			wantStale:  "ghost",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			profilesPath := filepath.Join(dir, "ferridex-profiles.env")
			writeClaudeProfiles(t, profilesPath, testClaudeProfilesEnv)

			configured, manager := configureClaudeProvidersWithConfig(
				config{ClaudeUpstream: test.persisted},
				func(func(*config) error) error { return nil },
				profilesPath,
				fakeClaudeSubscriptionProvider{},
			)
			if manager == nil {
				t.Fatal("manager not created")
			}
			switcher, ok := configured[0].(*provider.ClaudeUpstreamProvider)
			if !ok {
				t.Fatalf("configured[0] type = %T", configured[0])
			}
			snapshot := switcher.Snapshot()
			if snapshot.Active != test.wantActive {
				t.Fatalf("active = %q, want %q", snapshot.Active, test.wantActive)
			}
			if snapshot.ProfileName != test.wantProfile {
				t.Fatalf("profile = %q, want %q", snapshot.ProfileName, test.wantProfile)
			}
			if manager.staleProfile != test.wantStale {
				t.Fatalf("stale = %q, want %q", manager.staleProfile, test.wantStale)
			}

			// A fallback must keep relaying through the subscription (not 503).
			if test.wantActive == provider.ClaudeUpstreamSubscription {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
				configured[0].Relay(recorder, request)
				if recorder.Code != http.StatusNoContent {
					t.Fatalf("relay status = %d, want subscription relay", recorder.Code)
				}
			}
		})
	}
}

func TestPublicMuxDoesNotExposeClaudeManagementAPI(t *testing.T) {
	manager, _, profilesPath := newTestClaudeUpstreamManager(t)
	writeClaudeProfiles(t, profilesPath, testClaudeProfilesEnv)
	manager.loadProfiles()

	publicHandler := buildMux(false, false, "", false, nil, nil, nil, manager, manager.runtime)
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/claude-upstream"},
		{method: http.MethodPost, path: "/api/claude-upstream/activate", body: `{}`},
		{method: http.MethodPost, path: "/api/claude-upstream/reload", body: `{}`},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			recorder := httptest.NewRecorder()
			publicHandler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	dashboardHandler := buildMux(true, false, "", false, nil, nil, nil, manager, manager.runtime)
	recorder := httptest.NewRecorder()
	dashboardHandler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/claude-upstream", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestEnsureClaudeProfilesFileCreatesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ferridex-profiles.env")
	if err := ensureClaudeProfilesFile(path); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fileParsed, err := provider.ParseClaudeProfiles(content)
	if err != nil {
		t.Fatalf("template must parse: %v", err)
	}
	if len(fileParsed.Profiles) != 0 || len(fileParsed.Warnings) != 0 {
		t.Fatalf("template parse = %#v, want clean empty", fileParsed)
	}

	// An existing file is never overwritten.
	if err := os.WriteFile(path, []byte("[keep]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeProfilesFile(path); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "[keep]\n" {
		t.Fatalf("existing file was overwritten: %q", content)
	}
}
