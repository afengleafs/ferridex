package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func validCustomResponsesConfig(baseURL string) CustomResponsesConfig {
	return CustomResponsesConfig{
		Name:         "VOD GPT",
		BaseURL:      baseURL,
		APIKey:       "company-secret",
		DefaultModel: "gpt-5.6-sol",
	}
}

func TestValidateCustomResponsesConfig(t *testing.T) {
	valid := []string{
		"https://example.com/v1",
		"https://example.com/nested/api/",
		"http://localhost:8080/v1",
		"http://localhost.:8080/v1",
		"http://127.0.0.1:8080/v1",
		"http://[::1]:8080/v1",
	}
	for _, baseURL := range valid {
		t.Run("valid_"+strings.ReplaceAll(baseURL, "/", "_"), func(t *testing.T) {
			if err := ValidateCustomResponsesConfig(validCustomResponsesConfig(baseURL)); err != nil {
				t.Fatalf("ValidateCustomResponsesConfig(%q): %v", baseURL, err)
			}
		})
	}

	invalid := map[string]string{
		"remote HTTP":       "http://example.com/v1",
		"relative":          "/v1",
		"wrong scheme":      "ftp://example.com/v1",
		"userinfo":          "https://user:pass@example.com/v1",
		"query":             "https://example.com/v1?a=b",
		"empty query":       "https://example.com/v1?",
		"fragment":          "https://example.com/v1#part",
		"full endpoint":     "https://example.com/v1/responses",
		"endpoint slash":    "https://example.com/v1/RESPONSES/",
		"escaped endpoint":  "https://example.com/v1%2Fresponses",
		"encoded control":   "https://example.com/v1/%0A",
		"malformed port":    "https://example.com:bad/v1",
		"missing hostname":  "https:///v1",
		"non-loopback IPv4": "http://192.168.1.3/v1",
	}
	for name, baseURL := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCustomResponsesConfig(validCustomResponsesConfig(baseURL)); err == nil {
				t.Fatalf("ValidateCustomResponsesConfig(%q) unexpectedly succeeded", baseURL)
			}
		})
	}

	for name, mutate := range map[string]func(*CustomResponsesConfig){
		"missing name":  func(c *CustomResponsesConfig) { c.Name = "" },
		"missing key":   func(c *CustomResponsesConfig) { c.APIKey = "" },
		"missing model": func(c *CustomResponsesConfig) { c.DefaultModel = "" },
		"control key":   func(c *CustomResponsesConfig) { c.APIKey = "secret\r\nInjected: yes" },
		"control name":  func(c *CustomResponsesConfig) { c.Name = "bad\x00name" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validCustomResponsesConfig("https://example.com/v1")
			mutate(&cfg)
			if err := ValidateCustomResponsesConfig(cfg); err == nil {
				t.Fatal("validation unexpectedly succeeded")
			}
		})
	}
}

func TestResponsesHTTPClientDoesNotAlterContentEncoding(t *testing.T) {
	client := newResponsesHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || !transport.DisableCompression {
		t.Fatalf("custom Responses transport must disable transparent compression: %#v", client.Transport)
	}
}

func TestSaveCustomNormalizesAndSnapshotDoesNotSerializeSecret(t *testing.T) {
	p := NewResponsesProvider(&responsesStubProvider{})
	cfg := validCustomResponsesConfig(" http://localhost:8080/v1/ ")
	cfg.Name = "  VOD GPT  "
	cfg.APIKey = " company-secret "
	if err := p.SaveCustom(cfg); err != nil {
		t.Fatal(err)
	}
	snapshot := p.Snapshot()
	if !snapshot.Configured || !snapshot.HasAPIKey {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Custom.BaseURL != "http://localhost:8080/v1" || snapshot.Custom.Name != "VOD GPT" || snapshot.Custom.APIKey != "company-secret" {
		t.Fatalf("config was not normalized: %+v", snapshot.Custom)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "company-secret") || strings.Contains(string(encoded), `"api_key":`) {
		t.Fatalf("snapshot JSON leaked API key: %s", encoded)
	}
	configJSON, err := json.Marshal(snapshot.Custom)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configJSON), `"api_key":"company-secret"`) {
		t.Fatalf("persisted config is missing api_key JSON tag: %s", configJSON)
	}
}

func TestCustomResponsesRelayIsTransparentAndIsolatesCredentials(t *testing.T) {
	type observedRequest struct {
		path, query, method, body string
		header                    http.Header
	}
	observed := make(chan observedRequest, 1)
	client := &http.Client{Transport: responsesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		observed <- observedRequest{r.URL.Path, r.URL.RawQuery, r.Method, string(body), r.Header.Clone()}
		return responsesHTTPResponse(r, http.StatusTooManyRequests, http.Header{
			"Content-Type":       []string{"application/json"},
			"X-Upstream-Request": []string{"visible"},
			"Alt-Svc":            []string{`h3=":443"`},
		}, `{"error":{"message":"quota"}}`), nil
	})}
	subscription := &responsesStubProvider{}
	p := newResponsesProvider(subscription, client)
	if err := p.SaveCustom(validCustomResponsesConfig("https://upstream.example/v1/")); err != nil {
		t.Fatal(err)
	}
	if err := p.Activate(ResponsesSourceCustom); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"gpt-5.6-sol","stream":true,"input":"unchanged"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?trace=abc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer downstream-key")
	req.Header.Set("X-API-Key", "downstream-x-key")
	req.Header.Set("Cookie", "session=private")
	req.Header.Set("Forwarded", "for=203.0.113.9")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "remove-me")
	req.Header.Set("X-Custom", "keep-me")
	recorder := httptest.NewRecorder()
	p.Relay(recorder, req)

	got := <-observed
	if got.path != "/v1/responses" || got.query != "trace=abc" || got.method != http.MethodPost || got.body != body {
		t.Fatalf("upstream request changed: %+v", got)
	}
	if got.header.Get("Authorization") != "Bearer company-secret" {
		t.Fatalf("unexpected upstream auth: %q", got.header.Get("Authorization"))
	}
	for _, key := range []string{"X-API-Key", "Cookie", "Forwarded", "X-Forwarded-For", "X-Real-IP", "X-Hop"} {
		if value := got.header.Get(key); value != "" {
			t.Errorf("sensitive/hop header %s leaked upstream: %q", key, value)
		}
	}
	if got.header.Get("X-Custom") != "keep-me" {
		t.Errorf("ordinary request header was not preserved: %q", got.header.Get("X-Custom"))
	}
	if recorder.Code != http.StatusTooManyRequests || recorder.Body.String() != `{"error":{"message":"quota"}}` {
		t.Fatalf("response was not transparent: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "application/json" || recorder.Header().Get("X-Upstream-Request") != "visible" {
		t.Fatalf("API response headers were not preserved: %v", recorder.Header())
	}
	if recorder.Header().Get("Alt-Svc") != "" {
		t.Fatalf("upstream Alt-Svc leaked to downstream: %q", recorder.Header().Get("Alt-Svc"))
	}
	if subscription.relayCount.Load() != 0 {
		t.Fatal("custom relay unexpectedly fell back to subscription")
	}
	status := p.Status()
	if status.Title != "VOD GPT" || !status.LoggedIn || status.Detail == "" {
		t.Fatalf("status does not describe active custom upstream: %+v", status)
	}
}

func TestCustomResponsesRelayPreservesSSEAndFlushes(t *testing.T) {
	want := "event: response.output_text.delta\ndata: {\"delta\":\"a\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	client := &http.Client{Transport: responsesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return responsesHTTPResponse(r, http.StatusCreated, http.Header{
			"Content-Type": []string{"text/event-stream; charset=utf-8"},
		}, want), nil
	})}
	p := newResponsesProvider(&responsesStubProvider{}, client)
	if err := p.SaveCustom(validCustomResponsesConfig("https://stream.example/v1")); err != nil {
		t.Fatal(err)
	}
	if err := p.Activate(ResponsesSourceCustom); err != nil {
		t.Fatal(err)
	}
	recorder := &responsesFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	p.Relay(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`)))

	if recorder.Code != http.StatusCreated || recorder.Body.String() != want {
		t.Fatalf("SSE changed: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if recorder.flushes == 0 {
		t.Fatal("SSE relay never flushed downstream")
	}
}

func TestResponsesProviderSwitchesImmediatelyAndDoesNotFallback(t *testing.T) {
	subscription := &responsesStubProvider{responseBody: "subscription"}
	client := &http.Client{Transport: responsesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := "first"
		if r.URL.Host == "second.example" {
			body = "second"
		}
		return responsesHTTPResponse(r, http.StatusOK, nil, body), nil
	})}
	p := newResponsesProvider(subscription, client)
	assertRelayBody(t, p, "subscription")
	if err := p.Activate(ResponsesSourceCustom); !errors.Is(err, ErrCustomResponsesNotConfigured) {
		t.Fatalf("Activate(custom) without config error = %v", err)
	}
	if got := p.Snapshot().ActiveSource; got != ResponsesSourceSubscription {
		t.Fatalf("failed interactive activation changed source to %q", got)
	}
	if err := p.SaveCustom(validCustomResponsesConfig("https://first.example/v1")); err != nil {
		t.Fatal(err)
	}
	if err := p.Activate(ResponsesSourceCustom); err != nil {
		t.Fatal(err)
	}
	assertRelayBody(t, p, "first")

	secondCfg := validCustomResponsesConfig("https://second.example/v1")
	secondCfg.Name = "Second"
	if err := p.SaveCustom(secondCfg); err != nil {
		t.Fatal(err)
	}
	assertRelayBody(t, p, "second")
	if got := p.Snapshot().ActiveSource; got != ResponsesSourceCustom {
		t.Fatalf("SaveCustom changed active source to %q", got)
	}

	p.ClearCustom()
	if snapshot := p.Snapshot(); snapshot.ActiveSource != ResponsesSourceSubscription || snapshot.Configured || snapshot.HasAPIKey || snapshot.Custom.APIKey != "" {
		t.Fatalf("ClearCustom left state behind: %+v", snapshot)
	}
	assertRelayBody(t, p, "subscription")

	failingClient := &http.Client{Transport: responsesRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	failing := newResponsesProvider(subscription, failingClient)
	if err := failing.SaveCustom(validCustomResponsesConfig("https://upstream.example/v1")); err != nil {
		t.Fatal(err)
	}
	if err := failing.Activate(ResponsesSourceCustom); err != nil {
		t.Fatal(err)
	}
	before := subscription.relayCount.Load()
	recorder := httptest.NewRecorder()
	failing.Relay(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusBadGateway || subscription.relayCount.Load() != before {
		t.Fatalf("failed custom request fell back: status=%d subscription before=%d after=%d", recorder.Code, before, subscription.relayCount.Load())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"upstream_request_failed"`) {
		t.Fatalf("expected Responses-compatible error, got %q", recorder.Body.String())
	}
}

func TestResponsesProviderRestoreMissingCustomPreservesIntentWithoutFallback(t *testing.T) {
	subscription := &responsesStubProvider{responseBody: "subscription"}
	p := NewResponsesProvider(subscription)

	err := p.RestoreSource(ResponsesSourceCustom)
	if !errors.Is(err, ErrCustomResponsesNotConfigured) {
		t.Fatalf("RestoreSource(custom) error = %v", err)
	}
	if snapshot := p.Snapshot(); snapshot.ActiveSource != ResponsesSourceCustom || snapshot.Configured {
		t.Fatalf("persisted custom choice was not retained: %+v", snapshot)
	}

	recorder := httptest.NewRecorder()
	p.Relay(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("Relay status = %d, want 503; body=%q", recorder.Code, recorder.Body.String())
	}
	if subscription.relayCount.Load() != 0 {
		t.Fatalf("missing persisted custom upstream fell back to subscription %d times", subscription.relayCount.Load())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"custom_not_configured"`) {
		t.Fatalf("unexpected 503 response: %q", recorder.Body.String())
	}
}

func TestResponsesProviderTestCustomAndUsageDelegation(t *testing.T) {
	var sawTest atomic.Bool
	client := &http.Client{Transport: responsesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if r.Header.Get("Authorization") != "Bearer company-secret" || payload["model"] != "gpt-5.6-sol" || payload["stream"] != false || payload["max_output_tokens"] != float64(16) {
			t.Errorf("unexpected test request: auth=%q payload=%v", r.Header.Get("Authorization"), payload)
		}
		sawTest.Store(true)
		return responsesHTTPResponse(r, http.StatusUnauthorized, http.Header{
			"Content-Type": []string{"application/json"},
		}, `{"error":{"message":"bad key company-secret"}}`), nil
	})}

	subscription := &responsesUsageStub{responsesStubProvider: responsesStubProvider{}, windows: []UsageWindow{{Label: "weekly", Utilization: .25}}}
	p := newResponsesProvider(subscription, client)
	if _, err := p.TestCustom(context.Background()); !errors.Is(err, ErrCustomResponsesNotConfigured) {
		t.Fatalf("TestCustom without config error = %v", err)
	}
	windows, err := p.QueryUsage(context.Background())
	if err != nil || len(windows) != 1 || windows[0].Label != "weekly" {
		t.Fatalf("subscription usage = %+v, %v", windows, err)
	}
	if err := p.SaveCustom(validCustomResponsesConfig("https://test.example/v1")); err != nil {
		t.Fatal(err)
	}
	result, err := p.TestCustom(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sawTest.Load() || result.OK || result.StatusCode != http.StatusUnauthorized || result.Duration < 0 {
		t.Fatalf("unexpected test result: %+v", result)
	}
	if result.Message != "bad key [REDACTED]" {
		t.Fatalf("test result did not redact key: %q", result.Message)
	}
	if err := p.Activate(ResponsesSourceCustom); err != nil {
		t.Fatal(err)
	}
	if _, err := p.QueryUsage(context.Background()); !errors.Is(err, ErrResponsesUsageUnavailable) {
		t.Fatalf("custom usage error = %v", err)
	}
}

func TestResponsesProviderConcurrentSwitching(t *testing.T) {
	client := &http.Client{Transport: responsesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return responsesHTTPResponse(r, http.StatusOK, nil, "custom"), nil
	})}
	p := newResponsesProvider(&responsesStubProvider{responseBody: "subscription"}, client)
	cfg := validCustomResponsesConfig("https://concurrent.example/v1")
	if err := p.SaveCustom(cfg); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = p.Activate(ResponsesSourceCustom)
			} else {
				_ = p.Activate(ResponsesSourceSubscription)
			}
			_ = p.SaveCustom(cfg)
		}(i)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			p.Relay(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
			if recorder.Code != http.StatusOK {
				t.Errorf("concurrent relay status = %d", recorder.Code)
			}
		}()
	}
	wg.Wait()
}

func assertRelayBody(t *testing.T, p Provider, want string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	p.Relay(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != want {
		t.Fatalf("relay: status=%d body=%q, want 200 %q", recorder.Code, recorder.Body.String(), want)
	}
}

type responsesStubProvider struct {
	relayCount   atomic.Int64
	responseBody string
}

func (p *responsesStubProvider) Name() string { return "codex" }

func (p *responsesStubProvider) Relay(w http.ResponseWriter, _ *http.Request) {
	p.relayCount.Add(1)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, p.responseBody)
}

func (p *responsesStubProvider) Status() ProviderStatus {
	return ProviderStatus{Name: "codex", Title: "ChatGPT · Codex", LoggedIn: true, Endpoint: "/v1/responses"}
}

type responsesUsageStub struct {
	responsesStubProvider
	windows []UsageWindow
}

func (p *responsesUsageStub) QueryUsage(context.Context) ([]UsageWindow, error) {
	return append([]UsageWindow(nil), p.windows...), nil
}

type responsesRoundTripFunc func(*http.Request) (*http.Response, error)

func (f responsesRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func responsesHTTPResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode:    status,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

type responsesFlushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *responsesFlushRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}
