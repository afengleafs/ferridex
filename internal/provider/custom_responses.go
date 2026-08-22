package provider

// This file implements a transparent OpenAI Responses upstream and a small
// runtime switch which can select it instead of the local ChatGPT subscription.
// Unlike CodexProvider, the custom upstream does not patch request payloads or
// translate response streams: it relays the Responses protocol byte-for-byte.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ResponsesSource identifies the upstream used for /v1/responses.
type ResponsesSource string

const (
	ResponsesSourceSubscription ResponsesSource = "subscription"
	ResponsesSourceCustom       ResponsesSource = "custom"
)

var (
	// ErrCustomResponsesNotConfigured is returned when custom is selected or
	// tested before a custom upstream has been saved.
	ErrCustomResponsesNotConfigured = errors.New("custom Responses upstream is not configured")
	// ErrSubscriptionResponsesNotConfigured is only possible when a switcher
	// was constructed without its subscription provider.
	ErrSubscriptionResponsesNotConfigured = errors.New("subscription Responses upstream is not configured")
	// ErrResponsesUsageUnavailable is returned instead of silently querying the
	// subscription while the custom upstream is active.
	ErrResponsesUsageUnavailable = errors.New("usage is unavailable for the active custom Responses upstream")
)

// CustomResponsesConfig is the persisted configuration for one OpenAI
// Responses-compatible upstream. BaseURL is the API root (for example,
// https://example.com/v1), not the full /responses endpoint.
type CustomResponsesConfig struct {
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	APIKey       string `json:"api_key"`
	DefaultModel string `json:"default_model"`
}

// ResponsesSnapshot is an immutable copy of the switcher's current state.
// Custom intentionally does not participate in JSON encoding: callers serving
// a management API must explicitly construct a redacted view rather than risk
// serializing the upstream API key.
type ResponsesSnapshot struct {
	ActiveSource ResponsesSource       `json:"active_source"`
	Custom       CustomResponsesConfig `json:"-"`
	Configured   bool                  `json:"configured"`
	HasAPIKey    bool                  `json:"has_api_key"`
}

// CustomResponsesTestResult summarizes an explicit connection test. HTTP
// errors are represented as a result (with OK=false); only request construction
// or transport failures are returned as Go errors.
type CustomResponsesTestResult struct {
	OK         bool          `json:"ok"`
	StatusCode int           `json:"status"`
	Duration   time.Duration `json:"-"`
	DurationMS int64         `json:"duration_ms"`
	Message    string        `json:"message,omitempty"`
}

// CustomResponsesProvider transparently relays the OpenAI Responses wire API.
// Its normalized config is immutable, which makes it safe to retain for a
// request while another goroutine replaces the switcher's active config.
type CustomResponsesProvider struct {
	config CustomResponsesConfig
	client *http.Client
}

// NewCustomResponsesProvider validates cfg and constructs a standalone custom
// Responses provider. Most callers should use ResponsesProvider.SaveCustom.
func NewCustomResponsesProvider(cfg CustomResponsesConfig) (*CustomResponsesProvider, error) {
	return newCustomResponsesProvider(cfg, newResponsesHTTPClient())
}

func newCustomResponsesProvider(cfg CustomResponsesConfig, client *http.Client) (*CustomResponsesProvider, error) {
	normalized, err := normalizeCustomResponsesConfig(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = newResponsesHTTPClient()
	}
	return &CustomResponsesProvider{config: normalized, client: client}, nil
}

func newResponsesHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Do not let net/http add gzip and transparently decompress the response;
	// the downstream client, not Ferridex, owns content negotiation.
	transport.DisableCompression = true
	return &http.Client{
		Transport: transport,
		// A redirect is an upstream response, not permission to send a company
		// credential to another endpoint. Preserve it for the downstream client.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidateCustomResponsesConfig applies the same strict validation used by
// SaveCustom and NewCustomResponsesProvider.
func ValidateCustomResponsesConfig(cfg CustomResponsesConfig) error {
	_, err := normalizeCustomResponsesConfig(cfg)
	return err
}

func normalizeCustomResponsesConfig(cfg CustomResponsesConfig) (CustomResponsesConfig, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.DefaultModel = strings.TrimSpace(cfg.DefaultModel)

	fields := []struct {
		name  string
		value string
	}{
		{"name", cfg.Name},
		{"base_url", cfg.BaseURL},
		{"api_key", cfg.APIKey},
		{"default_model", cfg.DefaultModel},
	}
	for _, field := range fields {
		if field.value == "" {
			return CustomResponsesConfig{}, fmt.Errorf("%s is required", field.name)
		}
		if containsControl(field.value) {
			return CustomResponsesConfig{}, fmt.Errorf("%s contains control characters", field.name)
		}
	}
	if len(cfg.Name) > 128 {
		return CustomResponsesConfig{}, errors.New("name is too long")
	}
	if len(cfg.BaseURL) > 2048 {
		return CustomResponsesConfig{}, errors.New("base_url is too long")
	}
	if len(cfg.APIKey) > 16*1024 {
		return CustomResponsesConfig{}, errors.New("api_key is too long")
	}
	if len(cfg.DefaultModel) > 256 {
		return CustomResponsesConfig{}, errors.New("default_model is too long")
	}

	u, err := normalizeAPIBaseURL(cfg.BaseURL, "base_url")
	if err != nil {
		return CustomResponsesConfig{}, err
	}
	if strings.EqualFold(pathLastSegment(u.Path), "responses") {
		return CustomResponsesConfig{}, errors.New("base_url must be the API root, not a /responses endpoint")
	}
	cfg.BaseURL = strings.TrimRight(u.String(), "/")
	return cfg, nil
}

// normalizeAPIBaseURL holds the security rules shared by every custom-upstream
// flavor (Responses, Claude): absolute http(s) URL, https unless loopback, no
// user info/query/fragment/control characters. The returned URL has its path
// trimmed of trailing slashes; callers still decide which endpoint segment
// (e.g. "responses", "messages") must not already be the last path segment.
func normalizeAPIBaseURL(value, field string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New(field + " scheme must be https (or http for localhost)")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New(field + " must be an absolute HTTP URL")
	}
	if u.User != nil {
		return nil, errors.New(field + " must not contain user info")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New(field + " must not contain a query")
	}
	if u.Fragment != "" {
		return nil, errors.New(field + " must not contain a fragment")
	}
	if containsControl(u.Host) || containsControl(u.Path) {
		return nil, errors.New(field + " contains control characters")
	}
	if u.Scheme == "http" && !isLoopbackHostname(u.Hostname()) {
		return nil, errors.New("http " + field + " is only allowed for localhost or a loopback IP")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func containsControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

func isLoopbackHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func pathLastSegment(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (c *CustomResponsesProvider) Name() string { return "codex" }

func (c *CustomResponsesProvider) Status() ProviderStatus {
	return ProviderStatus{
		Name:     "codex",
		Title:    c.config.Name,
		LoggedIn: true,
		Detail:   "自定义 Responses 上游 · " + c.config.BaseURL,
		Models:   []string{c.config.DefaultModel},
		Endpoint: "/v1/responses",
	}
}

func (c *CustomResponsesProvider) Relay(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		defer r.Body.Close()
	}

	upstreamReq, err := c.newRequest(r.Context(), r.Method, r.URL.RawQuery, r.Body, r.Header, r.ContentLength)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "request_build_error", "failed to create upstream request")
		return
	}
	upstreamResp, err := c.client.Do(upstreamReq)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_request_failed", "upstream request failed: "+err.Error())
		return
	}
	defer upstreamResp.Body.Close()

	copyResponseHeaders(w.Header(), upstreamResp.Header)
	w.WriteHeader(upstreamResp.StatusCode)
	flush := strings.Contains(strings.ToLower(upstreamResp.Header.Get("Content-Type")), "text/event-stream")
	_ = copyResponseBody(w, upstreamResp.Body, flush)
}

func (c *CustomResponsesProvider) newRequest(ctx context.Context, method, rawQuery string, body io.Reader, headers http.Header, contentLength int64) (*http.Request, error) {
	target := c.config.BaseURL + "/responses"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = contentLength
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	removeConnectionHeaders(req.Header)
	removeDownstreamIdentityHeaders(req.Header)
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	return req, nil
}

func removeDownstreamIdentityHeaders(h http.Header) {
	for _, key := range []string{
		"Authorization", "Proxy-Authorization", "X-API-Key", "Cookie", "Cookie2",
		"Forwarded", "Via", "X-Real-IP", "True-Client-IP", "CF-Connecting-IP",
		"Fastly-Client-IP", "X-Client-IP", "X-Cluster-Client-IP", "CDN-Loop",
	} {
		h.Del(key)
	}
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {
			h.Del(key)
		}
	}
}

func removeConnectionHeaders(h http.Header) {
	for _, key := range strings.Split(h.Get("Connection"), ",") {
		if key = strings.TrimSpace(key); key != "" {
			h.Del(key)
		}
	}
	for _, key := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(key)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	copy := src.Clone()
	removeConnectionHeaders(copy)
	// Alt-Svc describes the upstream origin, not the Ferridex endpoint.
	copy.Del("Alt-Svc")
	for key, values := range copy {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseBody(dst http.ResponseWriter, src io.Reader, flush bool) error {
	buf := make([]byte, 32*1024)
	controller := http.NewResponseController(dst)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			if flush {
				// A real net/http ResponseWriter supports Flush. An unsupported test
				// or embedding writer can still receive the complete response body.
				_ = controller.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func writeResponsesError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    code,
		},
	})
}

func (c *CustomResponsesProvider) test(ctx context.Context) (CustomResponsesTestResult, error) {
	payload, _ := json.Marshal(map[string]any{
		"model":             c.config.DefaultModel,
		"input":             "Respond with OK.",
		"stream":            false,
		"max_output_tokens": 16,
	})
	req, err := c.newRequest(ctx, http.MethodPost, "", bytes.NewReader(payload), http.Header{
		"Content-Type": []string{"application/json"},
		"Accept":       []string{"application/json"},
	}, int64(len(payload)))
	if err != nil {
		return CustomResponsesTestResult{}, err
	}
	started := time.Now()
	resp, err := c.client.Do(req)
	duration := time.Since(started)
	result := CustomResponsesTestResult{Duration: duration, DurationMS: duration.Milliseconds()}
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode
	result.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if !result.OK {
		result.Message = responseErrorMessage(body)
		result.Message = strings.ReplaceAll(result.Message, c.config.APIKey, "[REDACTED]")
	}
	return result, nil
}

func responseErrorMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.Error.Message != "" {
			return truncateUTF8(envelope.Error.Message, 500)
		}
		if envelope.Message != "" {
			return truncateUTF8(envelope.Message, 500)
		}
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		return "upstream returned an empty error response"
	}
	return truncateUTF8(message, 500)
}

func truncateUTF8(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}

// ResponsesProvider switches /v1/responses between the local subscription and
// one custom OpenAI Responses-compatible upstream.
type ResponsesProvider struct {
	mu           sync.RWMutex
	subscription Provider
	custom       *CustomResponsesProvider
	active       ResponsesSource
	client       *http.Client
}

func NewResponsesProvider(subscription Provider) *ResponsesProvider {
	return newResponsesProvider(subscription, newResponsesHTTPClient())
}

func newResponsesProvider(subscription Provider, client *http.Client) *ResponsesProvider {
	if client == nil {
		client = newResponsesHTTPClient()
	}
	return &ResponsesProvider{
		subscription: subscription,
		active:       ResponsesSourceSubscription,
		client:       client,
	}
}

func (p *ResponsesProvider) Name() string {
	if p.subscription != nil {
		return p.subscription.Name()
	}
	return "codex"
}

func (p *ResponsesProvider) Relay(w http.ResponseWriter, r *http.Request) {
	source, selected := p.selected()
	if selected == nil {
		code := "subscription_not_configured"
		message := ErrSubscriptionResponsesNotConfigured.Error()
		if source == ResponsesSourceCustom {
			code = "custom_not_configured"
			message = ErrCustomResponsesNotConfigured.Error()
		}
		writeResponsesError(w, http.StatusServiceUnavailable, code, message)
		return
	}
	// selected is a stable pointer. SaveCustom/Activate can proceed without
	// affecting this request and without holding a mutex across network I/O.
	selected.Relay(w, r)
}

func (p *ResponsesProvider) Status() ProviderStatus {
	_, selected := p.selected()
	if selected == nil {
		return ProviderStatus{
			Name:     p.Name(),
			Title:    "Responses",
			Detail:   "当前 Responses 上游未配置",
			Endpoint: "/v1/responses",
		}
	}
	return selected.Status()
}

func (p *ResponsesProvider) selected() (ResponsesSource, Provider) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.active == ResponsesSourceCustom {
		if p.custom == nil {
			return p.active, nil
		}
		return p.active, p.custom
	}
	return p.active, p.subscription
}

// SaveCustom validates and atomically replaces the custom upstream. When
// custom is active, new requests immediately use cfg; in-flight requests keep
// their previous immutable provider snapshot.
func (p *ResponsesProvider) SaveCustom(cfg CustomResponsesConfig) error {
	custom, err := newCustomResponsesProvider(cfg, p.client)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.custom = custom
	p.mu.Unlock()
	return nil
}

// ClearCustom removes the upstream credential and atomically returns routing to
// the subscription provider.
func (p *ResponsesProvider) ClearCustom() {
	p.mu.Lock()
	p.custom = nil
	p.active = ResponsesSourceSubscription
	p.mu.Unlock()
}

// Activate switches future requests to source. It never silently falls back to
// subscription when custom is missing or later fails.
func (p *ResponsesProvider) Activate(source ResponsesSource) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch source {
	case ResponsesSourceSubscription:
		if p.subscription == nil {
			return ErrSubscriptionResponsesNotConfigured
		}
	case ResponsesSourceCustom:
		if p.custom == nil {
			return ErrCustomResponsesNotConfigured
		}
	default:
		return fmt.Errorf("invalid Responses source %q", source)
	}
	p.active = source
	return nil
}

// RestoreSource restores the user's persisted routing choice without silently
// changing it when the corresponding configuration is unavailable. In
// particular, restoring custom with no valid custom config leaves custom active
// and returns ErrCustomResponsesNotConfigured; Relay will then return 503
// instead of falling back to the subscription. Interactive callers should use
// Activate, which rejects an unavailable source without changing active state.
func (p *ResponsesProvider) RestoreSource(source ResponsesSource) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch source {
	case ResponsesSourceSubscription:
		p.active = source
		if p.subscription == nil {
			return ErrSubscriptionResponsesNotConfigured
		}
	case ResponsesSourceCustom:
		p.active = source
		if p.custom == nil {
			return ErrCustomResponsesNotConfigured
		}
	default:
		return fmt.Errorf("invalid Responses source %q", source)
	}
	return nil
}

func (p *ResponsesProvider) Snapshot() ResponsesSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	snapshot := ResponsesSnapshot{ActiveSource: p.active}
	if p.custom != nil {
		snapshot.Custom = p.custom.config
		snapshot.Configured = true
		snapshot.HasAPIKey = p.custom.config.APIKey != ""
	}
	return snapshot
}

// TestCustom performs one explicit minimal, non-streaming Responses request
// against the saved custom upstream. Callers should give ctx a finite deadline
// and make the potential small model charge clear in the UI.
func (p *ResponsesProvider) TestCustom(ctx context.Context) (CustomResponsesTestResult, error) {
	p.mu.RLock()
	custom := p.custom
	p.mu.RUnlock()
	if custom == nil {
		return CustomResponsesTestResult{}, ErrCustomResponsesNotConfigured
	}
	return custom.test(ctx)
}

// QueryUsage deliberately delegates only when subscription is active. This
// method keeps the existing on-demand usage API available after wrapping Codex.
func (p *ResponsesProvider) QueryUsage(ctx context.Context) ([]UsageWindow, error) {
	source, selected := p.selected()
	if source != ResponsesSourceSubscription {
		return nil, ErrResponsesUsageUnavailable
	}
	querier, ok := selected.(UsageQuerier)
	if !ok {
		return nil, errors.New("active subscription does not support usage queries")
	}
	return querier.QueryUsage(ctx)
}
