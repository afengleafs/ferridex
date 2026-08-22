package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func testClaudeProfile(mutate func(*ClaudeProfileConfig)) ClaudeProfileConfig {
	cfg := ClaudeProfileConfig{
		Name:        "gateway",
		BaseURL:     "http://127.0.0.1:9",
		AuthToken:   "sk-test-token",
		SonnetModel: "stealth/ox-alpha",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func newTestCustomClaude(t *testing.T, transport http.RoundTripper, mutate func(*ClaudeProfileConfig)) *CustomClaudeProvider {
	t.Helper()
	p, err := newCustomClaudeProvider(testClaudeProfile(mutate), &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("newCustomClaudeProvider: %v", err)
	}
	return p
}

func relayClaudeProfile(p Provider, body string, headers http.Header) *responseRecorder {
	req, err := http.NewRequest(http.MethodPost, "http://ferridex.test/v1/messages", strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	rec := newResponseRecorder()
	p.Relay(rec, req)
	return rec
}

func upstreamJSONResponse(status int, contentType, body string) (*http.Response, error) {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return response(status, header, body), nil
}

func TestNormalizeClaudeProfileValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ClaudeProfileConfig)
		wantErr string
	}{
		{name: "valid minimal"},
		{name: "missing name", mutate: func(c *ClaudeProfileConfig) { c.Name = " " }, wantErr: "name is required"},
		{name: "reserved name", mutate: func(c *ClaudeProfileConfig) { c.Name = "Subscription" }, wantErr: "reserved"},
		{name: "missing credential", mutate: func(c *ClaudeProfileConfig) { c.AuthToken = ""; c.APIKey = "" }, wantErr: "ANTHROPIC_AUTH_TOKEN"},
		{name: "http non-loopback", mutate: func(c *ClaudeProfileConfig) { c.BaseURL = "http://example.com/api" }, wantErr: "loopback"},
		{name: "query rejected", mutate: func(c *ClaudeProfileConfig) { c.BaseURL = "https://openrouter.ai/api?key=1" }, wantErr: "query"},
		{name: "fragment rejected", mutate: func(c *ClaudeProfileConfig) { c.BaseURL = "https://openrouter.ai/api#frag" }, wantErr: "fragment"},
		{name: "userinfo rejected", mutate: func(c *ClaudeProfileConfig) { c.BaseURL = "https://user:pw@openrouter.ai/api" }, wantErr: "user info"},
		{name: "messages endpoint rejected", mutate: func(c *ClaudeProfileConfig) { c.BaseURL = "https://openrouter.ai/api/v1/messages" }, wantErr: "/messages endpoint"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NormalizeClaudeProfile(testClaudeProfile(test.mutate))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestNormalizeClaudeProfileStripsTrailingV1(t *testing.T) {
	normalized, err := NormalizeClaudeProfile(testClaudeProfile(func(c *ClaudeProfileConfig) {
		c.BaseURL = "https://openrouter.ai/api/v1"
	}))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if normalized.BaseURL != "https://openrouter.ai/api" {
		t.Fatalf("base_url = %q, want trailing /v1 stripped", normalized.BaseURL)
	}
}

func TestCustomClaudeStatusDedupesModels(t *testing.T) {
	p := newTestCustomClaude(t, nil, func(c *ClaudeProfileConfig) {
		c.OpusModel = "stealth/ox-alpha"
		c.SonnetModel = "stealth/ox-alpha"
		c.HaikuModel = "stealth/ox-alpha"
	})
	status := p.Status()
	if len(status.Models) != 1 || status.Models[0] != "stealth/ox-alpha" {
		t.Fatalf("models = %v, want single deduped tag", status.Models)
	}
	if status.Title != "gateway" || status.Detail == "" {
		t.Fatalf("status = %#v", status)
	}
}

func TestCustomClaudeRelayURLAuthAndMapping(t *testing.T) {
	var (
		gotMethod string
		gotURL    string
		gotAuth   string
		gotAPIKey string
		gotBody   []byte
	)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotMethod = r.Method
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-API-Key")
		gotBody, _ = io.ReadAll(r.Body)
		return upstreamJSONResponse(http.StatusOK, "application/json", `{"id":"msg_1"}`)
	})
	p := newTestCustomClaude(t, transport, func(c *ClaudeProfileConfig) {
		c.BaseURL = "http://127.0.0.1:9/api"
		c.OpusModel = "stealth/ox-opus"
		c.SonnetModel = "stealth/ox-alpha"
		c.HaikuModel = "stealth/ox-haiku"
	})

	body := `{"model":"Claude-Sonnet-4-5","max_tokens":1,"system":"keep me","messages":[{"role":"user","content":"hi"}]}`
	rec := relayClaudeProfile(p, body, http.Header{"Content-Type": {"application/json"}})

	if rec.code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.code, rec.body.String())
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotURL != "http://127.0.0.1:9/api/v1/messages" {
		t.Fatalf("url = %s", gotURL)
	}
	if gotAuth != "Bearer sk-test-token" || gotAPIKey != "" {
		t.Fatalf("auth = %q / x-api-key = %q, want bearer only", gotAuth, gotAPIKey)
	}
	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if payload["model"] != "stealth/ox-alpha" {
		t.Fatalf("model = %v, want tier-mapped", payload["model"])
	}
	if payload["system"] != "keep me" {
		t.Fatalf("system = %v, want passthrough", payload["system"])
	}
	if _, ok := payload["metadata"]; ok {
		t.Fatalf("metadata injected: %v", payload["metadata"])
	}
}

func TestCustomClaudeRelayAPIKeyMode(t *testing.T) {
	var gotAuth, gotAPIKey string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-API-Key")
		return upstreamJSONResponse(http.StatusOK, "application/json", `{}`)
	})
	p := newTestCustomClaude(t, transport, func(c *ClaudeProfileConfig) {
		c.AuthToken = ""
		c.APIKey = "sk-api-key"
	})
	relayClaudeProfile(p, `{"model":"x"}`, nil)
	if gotAuth != "" || gotAPIKey != "sk-api-key" {
		t.Fatalf("auth = %q / x-api-key = %q, want x-api-key only", gotAuth, gotAPIKey)
	}
}

func TestMapClaudeModel(t *testing.T) {
	cfg := ClaudeProfileConfig{OpusModel: "o", SonnetModel: "s", HaikuModel: "h"}
	tests := []struct {
		model string
		want  string
	}{
		{"claude-opus-4-8", "o"},
		{"CLAUDE-SONNET-4-5", "s"},
		{"claude-3-5-haiku", "h"},
		{"gpt-x", "gpt-x"},         // no tier keyword -> pass through
		{"my-opuslike-thing", "o"}, // substring match is intentional
	}
	for _, test := range tests {
		if got := mapClaudeModel(test.model, cfg); got != test.want {
			t.Fatalf("mapClaudeModel(%q) = %q, want %q", test.model, got, test.want)
		}
	}
	// An unmapped tier passes through instead of silently crossing tiers.
	sonnetOnly := ClaudeProfileConfig{SonnetModel: "s"}
	if got := mapClaudeModel("claude-opus-4-8", sonnetOnly); got != "claude-opus-4-8" {
		t.Fatalf("unmapped tier = %q, want pass-through", got)
	}
}

func TestCustomClaudeRelayHeaderHygiene(t *testing.T) {
	var gotHeader http.Header
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotHeader = r.Header.Clone()
		return upstreamJSONResponse(http.StatusOK, "application/json", `{}`)
	})
	p := newTestCustomClaude(t, transport, nil)
	headers := http.Header{
		"Authorization":   {"Bearer downstream-secret"},
		"X-Api-Key":       {"downstream-key"},
		"Cookie":          {"session=1"},
		"X-Forwarded-For": {"203.0.113.9"},
		"Anthropic-Beta":  {"interleaved-thinking-2025-05-14"},
	}
	relayClaudeProfile(p, `{"model":"claude-sonnet-4-5"}`, headers)

	if gotHeader.Get("Authorization") != "Bearer sk-test-token" {
		t.Fatalf("Authorization = %q, want profile bearer", gotHeader.Get("Authorization"))
	}
	if gotHeader.Get("X-Api-Key") != "" {
		t.Fatal("downstream x-api-key survived")
	}
	if gotHeader.Get("Cookie") != "" || gotHeader.Get("X-Forwarded-For") != "" {
		t.Fatalf("identity headers survived: %v", gotHeader)
	}
	if gotHeader.Get("Anthropic-Beta") != "interleaved-thinking-2025-05-14" {
		t.Fatalf("anthropic-beta = %q, want verbatim passthrough", gotHeader.Get("Anthropic-Beta"))
	}
	if gotHeader.Get("anthropic-version") != anthropicVersion {
		t.Fatalf("anthropic-version = %q, want default", gotHeader.Get("anthropic-version"))
	}
	for key := range claudeMimicHeaders {
		if gotHeader.Get(key) != "" {
			t.Fatalf("mimic header %q must not be sent to custom upstreams", key)
		}
	}
}

func TestCustomClaudeRelayStreamsAndPasses429Through(t *testing.T) {
	statuses := []int{http.StatusTooManyRequests, http.StatusOK}
	var calls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		i := calls
		calls++
		if statuses[i] == http.StatusTooManyRequests {
			return response(http.StatusTooManyRequests, http.Header{
				"Content-Type": {"application/json"},
				"Retry-After":  {"12"},
			}, `{"type":"error","error":{"type":"rate_limit_error"}}`), nil
		}
		return upstreamJSONResponse(http.StatusOK, "text/event-stream", "event: message\ndata: {}\n\n")
	})
	p := newTestCustomClaude(t, transport, nil)

	first := relayClaudeProfile(p, `{"model":"x"}`, nil)
	if first.code != http.StatusTooManyRequests || !strings.Contains(first.body.String(), "rate_limit_error") {
		t.Fatalf("first relay = %d %s, want 429 passthrough", first.code, first.body.String())
	}
	// No subscription-style breaker: the very next request reaches upstream.
	second := relayClaudeProfile(p, `{"model":"x"}`, nil)
	if calls != 2 || second.code != http.StatusOK {
		t.Fatalf("calls = %d, second status = %d; 429 must not trip a breaker", calls, second.code)
	}
	if !strings.Contains(second.header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type = %q, want SSE preserved", second.header.Get("Content-Type"))
	}
	if second.body.String() != "event: message\ndata: {}\n\n" {
		t.Fatalf("streamed body = %q", second.body.String())
	}
}

// fakeClaudeSubscription doubles as both a Provider and UsageQuerier.
type fakeClaudeSubscription struct {
	Provider
	usageCalls int
	mu         sync.Mutex
}

func (f *fakeClaudeSubscription) Name() string { return "claude" }

func (f *fakeClaudeSubscription) Relay(_ http.ResponseWriter, _ *http.Request) {}

func (f *fakeClaudeSubscription) Status() ProviderStatus {
	return ProviderStatus{Name: "claude", Title: "Claude · Anthropic", LoggedIn: true}
}

func (f *fakeClaudeSubscription) QueryUsage(context.Context) ([]UsageWindow, error) {
	f.mu.Lock()
	f.usageCalls++
	f.mu.Unlock()
	return []UsageWindow{{Label: "5h"}}, nil
}

func newTestClaudeSwitcher(t *testing.T, transport http.RoundTripper) (*ClaudeUpstreamProvider, *fakeClaudeSubscription) {
	t.Helper()
	subscription := &fakeClaudeSubscription{}
	switcher := NewClaudeUpstreamProvider(subscription)
	if transport != nil {
		switcher.client = &http.Client{Transport: transport}
	}
	return switcher, subscription
}

func TestClaudeUpstreamSwitcherDelegation(t *testing.T) {
	switcher, subscription := newTestClaudeSwitcher(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return upstreamJSONResponse(http.StatusOK, "application/json", `{}`)
	}))

	if err := switcher.ActivateProfile("gateway"); !errors.Is(err, ErrClaudeProfileNotConfigured) {
		t.Fatalf("activate without profile = %v", err)
	}
	if err := switcher.SetProfile(testClaudeProfile(nil)); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	// Subscription stays active until explicitly switched.
	rec := relayClaudeProfile(switcher, `{"model":"x"}`, nil)
	_ = rec
	subscription.mu.Lock()
	usageBefore := subscription.usageCalls
	subscription.mu.Unlock()
	if _, err := switcher.QueryUsage(context.Background()); err != nil {
		t.Fatalf("usage while subscription active: %v", err)
	}
	subscription.mu.Lock()
	if subscription.usageCalls != usageBefore+1 {
		t.Fatal("usage query did not reach the subscription")
	}
	subscription.mu.Unlock()

	if err := switcher.ActivateProfile("gateway"); err != nil {
		t.Fatalf("activate profile: %v", err)
	}
	if snapshot := switcher.Snapshot(); snapshot.Active != ClaudeUpstreamProfile || snapshot.ProfileName != "gateway" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, err := switcher.QueryUsage(context.Background()); !errors.Is(err, ErrClaudeUsageUnavailable) {
		t.Fatalf("usage while profile active = %v, want ErrClaudeUsageUnavailable", err)
	}
	if status := switcher.Status(); !strings.Contains(status.Detail, "自定义上游 gateway") {
		t.Fatalf("status detail = %q", status.Detail)
	}
	if err := switcher.ActivateSubscription(); err != nil {
		t.Fatalf("activate subscription: %v", err)
	}
	if snapshot := switcher.Snapshot(); snapshot.Active != ClaudeUpstreamSubscription {
		t.Fatalf("active = %q after switching back", snapshot.Active)
	}
}

func TestClaudeUpstreamRestoreUnknownFailsClosed(t *testing.T) {
	switcher, _ := newTestClaudeSwitcher(t, nil)
	if err := switcher.RestoreActive("missing"); !errors.Is(err, ErrClaudeProfileNotConfigured) {
		t.Fatalf("restore unknown = %v", err)
	}
	rec := relayClaudeProfile(switcher, `{"model":"x"}`, nil)
	if rec.code != http.StatusServiceUnavailable {
		t.Fatalf("relay status = %d, want fail-closed 503", rec.code)
	}
}

func TestClaudeUpstreamInFlightRequestKeepsOldProfile(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var firstURL string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() {
			mu.Lock()
			firstURL = r.URL.String()
			mu.Unlock()
			close(started)
			<-release
		})
		return upstreamJSONResponse(http.StatusOK, "application/json", `{}`)
	})
	switcher, _ := newTestClaudeSwitcher(t, transport)
	if err := switcher.SetProfile(testClaudeProfile(func(c *ClaudeProfileConfig) {
		c.BaseURL = "http://127.0.0.1:9001"
	})); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	if err := switcher.ActivateProfile("gateway"); err != nil {
		t.Fatalf("activate: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		relayClaudeProfile(switcher, `{"model":"x"}`, nil)
	}()
	<-started

	// Swap the profile mid-flight; the request must keep its immutable snapshot.
	if err := switcher.SetProfile(testClaudeProfile(func(c *ClaudeProfileConfig) {
		c.BaseURL = "http://127.0.0.1:9002"
	})); err != nil {
		t.Fatalf("swap profile: %v", err)
	}
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if firstURL != "http://127.0.0.1:9001/v1/messages" {
		t.Fatalf("in-flight request hit %s, want the old base URL", firstURL)
	}
}
