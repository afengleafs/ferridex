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

type fakeResponsesSubscription struct{}

func (fakeResponsesSubscription) Name() string { return "codex" }

func (fakeResponsesSubscription) Relay(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (fakeResponsesSubscription) Status() provider.ProviderStatus {
	return provider.ProviderStatus{
		Name:     "codex",
		Title:    "Test subscription",
		LoggedIn: true,
		Models:   []string{"subscription-model"},
		Endpoint: "/v1/responses",
	}
}

func testCodexProfiles(baseURL string) string {
	return `[gateway]
OPENAI_BASE_URL=` + baseURL + `
OPENAI_API_KEY=` + testResponsesAPIKey + `
OPENAI_DEFAULT_MODEL=model-one

[broken]
OPENAI_BASE_URL=not-a-url
OPENAI_API_KEY=secret
OPENAI_DEFAULT_MODEL=bad-model
`
}

func TestResponsesUpstreamGetRedactsFileSuppliers(t *testing.T) {
	manager, _, profilesPath := newTestResponsesManager(t)
	writeCodexProfiles(t, profilesPath, testCodexProfiles("http://127.0.0.1:8080/v1"))
	manager.loadProfiles(provider.CustomResponsesConfig{})

	mux := http.NewServeMux()
	registerResponsesUpstreamAPI(mux, manager)
	response := upstreamAPIRequest(t, mux, http.MethodGet, "/api/responses-upstream", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	assertResponseDoesNotContainSecret(t, response.Body.String())
	var decoded struct {
		Active   string `json:"active"`
		Profiles []struct {
			Name      string `json:"name"`
			Valid     bool   `json:"valid"`
			HasAPIKey bool   `json:"has_api_key"`
		} `json:"profiles"`
		File string `json:"file"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Active != "" || len(decoded.Profiles) != 2 || !decoded.Profiles[0].Valid || !decoded.Profiles[0].HasAPIKey || decoded.Profiles[1].Valid {
		t.Fatalf("view = %#v", decoded)
	}
	if decoded.File != profilesPath {
		t.Fatalf("file = %q, want %q", decoded.File, profilesPath)
	}
}

func TestResponsesUpstreamActivatePersistsSupplierAndSubscription(t *testing.T) {
	manager, configPath, profilesPath := newTestResponsesManager(t)
	writeCodexProfiles(t, profilesPath, testCodexProfiles("http://127.0.0.1:8080/v1"))
	manager.loadProfiles(provider.CustomResponsesConfig{})
	mux := http.NewServeMux()
	registerResponsesUpstreamAPI(mux, manager)

	response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/activate", `{"name":"gateway"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("activate status = %d, body = %s", response.Code, response.Body.String())
	}
	if snapshot := manager.runtime.Snapshot(); snapshot.ActiveSource != provider.ResponsesSourceCustom || snapshot.Custom.Name != "gateway" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	persisted, err := readConfigFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.CodexUpstream != "gateway" || persisted.ResponsesSource != provider.ResponsesSourceCustom {
		t.Fatalf("persisted = %#v", persisted)
	}

	response = upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/activate", `{"name":""}`)
	if response.Code != http.StatusOK {
		t.Fatalf("subscription status = %d, body = %s", response.Code, response.Body.String())
	}
	persisted, _ = readConfigFile(configPath)
	if persisted.CodexUpstream != "" || persisted.ResponsesSource != provider.ResponsesSourceSubscription {
		t.Fatalf("subscription persisted = %#v", persisted)
	}
}

func TestResponsesUpstreamReloadMissingSupplierFailsClosedAndRestores(t *testing.T) {
	manager, configPath, profilesPath := newTestResponsesManager(t)
	writeCodexProfiles(t, profilesPath, testCodexProfiles("http://127.0.0.1:8080/v1"))
	manager.loadProfiles(provider.CustomResponsesConfig{})
	mux := http.NewServeMux()
	registerResponsesUpstreamAPI(mux, manager)
	if response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/activate", `{"name":"gateway"}`); response.Code != http.StatusOK {
		t.Fatalf("activate = %d: %s", response.Code, response.Body.String())
	}

	writeCodexProfiles(t, profilesPath, "[other]\nOPENAI_BASE_URL=http://127.0.0.1:8081/v1\nOPENAI_API_KEY=key\nOPENAI_DEFAULT_MODEL=model\n")
	response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/reload", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("reload = %d: %s", response.Code, response.Body.String())
	}
	if snapshot := manager.runtime.Snapshot(); snapshot.ActiveSource != provider.ResponsesSourceCustom || snapshot.Configured {
		t.Fatalf("missing supplier did not fail closed: %#v", snapshot)
	}
	recorder := httptest.NewRecorder()
	manager.runtime.Relay(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("relay = %d, want 503", recorder.Code)
	}
	persisted, _ := readConfigFile(configPath)
	if persisted.CodexUpstream != "gateway" {
		t.Fatalf("persisted supplier lost: %#v", persisted)
	}

	writeCodexProfiles(t, profilesPath, testCodexProfiles("http://127.0.0.1:8080/v1"))
	response = upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/reload", `{}`)
	if response.Code != http.StatusOK || manager.runtime.Snapshot().Custom.Name != "gateway" {
		t.Fatalf("supplier was not restored: %d %s %#v", response.Code, response.Body.String(), manager.runtime.Snapshot())
	}
}

func TestResponsesUpstreamTestsNamedSupplierWithoutActivating(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+testResponsesAPIKey {
			t.Fatalf("unexpected upstream request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	defer upstream.Close()

	manager, _, profilesPath := newTestResponsesManager(t)
	writeCodexProfiles(t, profilesPath, testCodexProfiles(upstream.URL+"/v1"))
	manager.loadProfiles(provider.CustomResponsesConfig{})
	mux := http.NewServeMux()
	registerResponsesUpstreamAPI(mux, manager)
	response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/test", `{"name":"gateway"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatalf("test = %d: %s", response.Code, response.Body.String())
	}
	if manager.runtime.Snapshot().ActiveSource != provider.ResponsesSourceSubscription {
		t.Fatal("testing a supplier changed active routing")
	}
}

func TestCodexProviderFileMigratesLegacyAndPreservesNonEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, codexProfilesFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := provider.CustomResponsesConfig{
		Name:         "Legacy] Supplier",
		BaseURL:      "https://api.example.com/v1",
		APIKey:       testResponsesAPIKey,
		DefaultModel: "legacy-model",
	}
	name, migrated, err := ensureCodexProfilesFile(path, legacy)
	if err != nil || !migrated || name != "Legacy- Supplier" {
		t.Fatalf("migration = %q/%v, err = %v", name, migrated, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	content, _ := os.ReadFile(path)
	parsed, err := provider.ParseCodexProfiles(content)
	if err != nil || len(parsed.Profiles) != 1 {
		t.Fatalf("parsed = %#v, err = %v", parsed, err)
	}

	if err := os.WriteFile(path, []byte("[keep]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, migrated, err = ensureCodexProfilesFile(path, legacy)
	content, _ = os.ReadFile(path)
	if err != nil || migrated || string(content) != "[keep]\n" {
		t.Fatalf("non-empty file changed: migrated=%v err=%v content=%q", migrated, err, content)
	}
}

func TestResponsesFileManagedEndpointsAndPublicIsolation(t *testing.T) {
	manager, _, profilesPath := newTestResponsesManager(t)
	writeCodexProfiles(t, profilesPath, testCodexProfiles("http://127.0.0.1:8080/v1"))
	manager.loadProfiles(provider.CustomResponsesConfig{})
	mux := http.NewServeMux()
	registerResponsesUpstreamAPI(mux, manager)
	for _, path := range []string{"/api/responses-upstream/save", "/api/responses-upstream/clear"} {
		response := upstreamAPIRequest(t, mux, http.MethodPost, path, `{}`)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), codexProfilesFileName) {
			t.Fatalf("%s = %d: %s", path, response.Code, response.Body.String())
		}
	}

	publicHandler := buildMux(false, false, "", false, nil, nil, manager, nil, manager.runtime)
	for _, path := range []string{"/api/responses-upstream", "/api/responses-upstream/reload"} {
		recorder := httptest.NewRecorder()
		publicHandler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("public %s = %d", path, recorder.Code)
		}
	}
}

func TestLocalNetinfoProvidesDownstreamKeyForRemoteClientConfigs(t *testing.T) {
	handler := buildMux(true, false, "test-downstream-key", false, nil, nil, nil, nil, fakeResponsesSubscription{})
	request := httptest.NewRequest(http.MethodGet, "/api/netinfo", nil)
	request.RemoteAddr = "127.0.0.1:43210"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("netinfo status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		LANEnabled bool   `json:"lan_enabled"`
		LANKey     string `json:"lan_key"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.LANEnabled || body.LANKey != "test-downstream-key" {
		t.Fatalf("unexpected netinfo: %#v", body)
	}
}

func newTestResponsesManager(t *testing.T) (*responsesUpstreamManager, string, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".ferridex", "config.json")
	profilesPath := filepath.Join(dir, codexProfilesFileName)
	subscription := fakeResponsesSubscription{}
	manager := &responsesUpstreamManager{
		runtime:      provider.NewResponsesProvider(subscription),
		subscription: subscription,
		profilesPath: profilesPath,
		persist: func(update func(*config) error) error {
			return updateConfigFile(configPath, update)
		},
		readPersisted: func() (config, error) {
			return readConfigFile(configPath)
		},
	}
	return manager, configPath, profilesPath
}

func writeCodexProfiles(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func upstreamAPIRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func assertResponseDoesNotContainSecret(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, testResponsesAPIKey) || strings.Contains(body, `"api_key"`) {
		t.Fatalf("management response leaked the API key: %s", body)
	}
}
