package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestResponsesUpstreamSaveRedactsAndRetainsOmittedAPIKey(t *testing.T) {
	manager, configPath := newTestResponsesManager(t)
	mux := http.NewServeMux()
	registerResponsesUpstreamAPI(mux, manager)

	first := `{"name":"Test VOD","base_url":"https://api.example.com/v1","api_key":"` + testResponsesAPIKey + `","default_model":"model-one"}`
	response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/save", first)
	if response.Code != http.StatusOK {
		t.Fatalf("first save status = %d, body = %s", response.Code, response.Body.String())
	}
	assertResponseDoesNotContainSecret(t, response.Body.String())
	assertSanitizedCustomView(t, response.Body.Bytes(), true, "Test VOD", "model-one")

	// The UI deliberately omits api_key when its password field is blank. That
	// must preserve the already-saved credential instead of erasing it.
	second := `{"name":"Renamed","base_url":"https://api.example.com/v1","default_model":"model-two"}`
	response = upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/save", second)
	if response.Code != http.StatusOK {
		t.Fatalf("second save status = %d, body = %s", response.Code, response.Body.String())
	}
	assertResponseDoesNotContainSecret(t, response.Body.String())
	assertSanitizedCustomView(t, response.Body.Bytes(), true, "Renamed", "model-two")

	persisted, err := readConfigFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if persisted.CustomResponses.APIKey != testResponsesAPIKey {
		t.Fatalf("omitted api_key did not retain the saved credential")
	}
	if got := manager.runtime.Snapshot().Custom.APIKey; got != testResponsesAPIKey {
		t.Fatalf("runtime api_key was not retained")
	}

	response = upstreamAPIRequest(t, mux, http.MethodGet, "/api/responses-upstream", "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", response.Code, response.Body.String())
	}
	assertResponseDoesNotContainSecret(t, response.Body.String())
	if strings.Contains(response.Body.String(), `"api_key"`) {
		t.Fatalf("management response exposed an api_key field: %s", response.Body.String())
	}
}

func TestResponsesUpstreamActivateAndClearPersistState(t *testing.T) {
	manager, configPath := newTestResponsesManager(t)
	mux := http.NewServeMux()
	registerResponsesUpstreamAPI(mux, manager)

	saveBody := `{"name":"Test upstream","base_url":"https://api.example.com/v1","api_key":"` + testResponsesAPIKey + `","default_model":"model-one"}`
	if response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/save", saveBody); response.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", response.Code, response.Body.String())
	}

	response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/activate", `{"source":"custom"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("activate status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := manager.runtime.Snapshot().ActiveSource; got != provider.ResponsesSourceCustom {
		t.Fatalf("runtime source = %q, want custom", got)
	}
	persisted, err := readConfigFile(configPath)
	if err != nil {
		t.Fatalf("read config after activate: %v", err)
	}
	if persisted.ResponsesSource != provider.ResponsesSourceCustom {
		t.Fatalf("persisted source = %q, want custom", persisted.ResponsesSource)
	}
	if persisted.CustomResponses.APIKey != testResponsesAPIKey {
		t.Fatal("activate lost persisted custom credential")
	}

	response = upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/clear", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", response.Code, response.Body.String())
	}
	assertResponseDoesNotContainSecret(t, response.Body.String())
	snapshot := manager.runtime.Snapshot()
	if snapshot.ActiveSource != provider.ResponsesSourceSubscription || snapshot.Configured {
		t.Fatalf("runtime was not reset after clear: %#v", snapshot)
	}
	persisted, err = readConfigFile(configPath)
	if err != nil {
		t.Fatalf("read config after clear: %v", err)
	}
	if persisted.ResponsesSource != provider.ResponsesSourceSubscription {
		t.Fatalf("persisted source after clear = %q, want subscription", persisted.ResponsesSource)
	}
	if persisted.CustomResponses != (provider.CustomResponsesConfig{}) {
		t.Fatalf("custom config remained after clear: %#v", persisted.CustomResponses)
	}
}

func TestResponsesUpstreamRestoresPersistedActiveCustomSource(t *testing.T) {
	manager, configPath := newTestResponsesManager(t)
	mux := http.NewServeMux()
	registerResponsesUpstreamAPI(mux, manager)
	saveBody := `{"name":"Restored upstream","base_url":"https://api.example.com/v1","api_key":"` + testResponsesAPIKey + `","default_model":"restored-model"}`
	if response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/save", saveBody); response.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := upstreamAPIRequest(t, mux, http.MethodPost, "/api/responses-upstream/activate", `{"source":"custom"}`); response.Code != http.StatusOK {
		t.Fatalf("activate status = %d, body = %s", response.Code, response.Body.String())
	}

	persisted, err := readConfigFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	providers, restored := configureResponsesProvidersWithConfig(persisted, manager.persist, fakeResponsesSubscription{})
	if len(providers) != 1 || restored == nil {
		t.Fatalf("restored providers = %d, manager nil = %v", len(providers), restored == nil)
	}
	snapshot := restored.runtime.Snapshot()
	if snapshot.ActiveSource != provider.ResponsesSourceCustom || !snapshot.Configured {
		t.Fatalf("persisted custom source was not restored: %#v", snapshot)
	}
	if snapshot.Custom.APIKey != testResponsesAPIKey || snapshot.Custom.DefaultModel != "restored-model" {
		t.Fatal("restored custom config did not match persisted values")
	}
}

func TestResponsesUpstreamRestoreMissingCustomFailsClosed(t *testing.T) {
	for name, cfg := range map[string]config{
		"missing custom": {ResponsesSource: provider.ResponsesSourceCustom},
		"invalid source": {
			ResponsesSource: provider.ResponsesSource("unexpected"),
			CustomResponses: provider.CustomResponsesConfig{
				Name:         "must not be selected",
				BaseURL:      "https://api.example.com/v1",
				APIKey:       testResponsesAPIKey,
				DefaultModel: "model",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			providers, manager := configureResponsesProvidersWithConfig(cfg, func(func(*config) error) error { return nil }, fakeResponsesSubscription{})
			if manager == nil || manager.runtime.Snapshot().ActiveSource != provider.ResponsesSourceCustom {
				t.Fatal("unavailable persisted selection did not remain fail-closed")
			}
			recorder := httptest.NewRecorder()
			providers[0].Relay(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("relay status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestPublicMuxDoesNotExposeResponsesManagementAPI(t *testing.T) {
	manager, _ := newTestResponsesManager(t)
	handler := buildMux(false, false, "", false, nil, nil, manager, nil, manager.runtime)

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/responses-upstream"},
		{method: http.MethodPost, path: "/api/responses-upstream/save", body: `{}`},
		{method: http.MethodPost, path: "/api/responses-upstream/activate", body: `{}`},
		{method: http.MethodPost, path: "/api/responses-upstream/test", body: `{}`},
		{method: http.MethodPost, path: "/api/responses-upstream/clear", body: `{}`},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", recorder.Code, recorder.Body.String())
			}
		})
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

func newTestResponsesManager(t *testing.T) (*responsesUpstreamManager, string) {
	t.Helper()
	subscription := fakeResponsesSubscription{}
	runtime := provider.NewResponsesProvider(subscription)
	path := filepath.Join(t.TempDir(), ".ferridex", "config.json")
	manager := &responsesUpstreamManager{
		runtime:      runtime,
		subscription: subscription,
		persist: func(update func(*config) error) error {
			return updateConfigFile(path, update)
		},
	}
	return manager, path
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
	if strings.Contains(body, testResponsesAPIKey) {
		t.Fatalf("management response leaked the API key: %s", body)
	}
}

func assertSanitizedCustomView(t *testing.T, body []byte, configured bool, name, model string) {
	t.Helper()
	var decoded struct {
		Custom struct {
			Configured   bool   `json:"configured"`
			Name         string `json:"name"`
			DefaultModel string `json:"default_model"`
			HasAPIKey    bool   `json:"has_api_key"`
		} `json:"custom"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&decoded); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if decoded.Custom.Configured != configured || decoded.Custom.Name != name || decoded.Custom.DefaultModel != model || !decoded.Custom.HasAPIKey {
		t.Fatalf("unexpected sanitized custom view: %#v", decoded.Custom)
	}
}
