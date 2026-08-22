package provider

// This file implements a custom Anthropic Messages upstream (BYOK gateways such
// as OpenRouter) plus a runtime switch which can select it instead of the local
// Anthropic subscription. Unlike ClaudeProvider, the custom upstream does not
// refresh OAuth tokens, inject the Claude Code fingerprint, or run the
// subscription quota breaker: it rewrites only the request model field and
// relays everything else transparently.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// ClaudeUpstream identifies the upstream used for /v1/messages.
type ClaudeUpstream string

const (
	ClaudeUpstreamSubscription ClaudeUpstream = "subscription"
	ClaudeUpstreamProfile      ClaudeUpstream = "profile"
)

var (
	// ErrClaudeProfileNotConfigured is returned when a profile is selected or
	// restored before one has been loaded from ferridex-profiles.env.
	ErrClaudeProfileNotConfigured = errors.New("custom Claude upstream profile is not configured")
	// ErrClaudeSubscriptionNotConfigured is only possible when a switcher was
	// constructed without its subscription provider.
	ErrClaudeSubscriptionNotConfigured = errors.New("subscription Claude upstream is not configured")
	// ErrClaudeUsageUnavailable is returned instead of silently querying the
	// subscription while a custom profile is active.
	ErrClaudeUsageUnavailable = errors.New("usage is unavailable for the active custom Claude upstream")
)

// ClaudeProfileConfig describes one Anthropic-compatible upstream. BaseURL is
// the API root (for example, https://openrouter.ai/api), not the full
// /v1/messages endpoint. Secrets intentionally do not participate in JSON
// encoding: callers serving a management API must explicitly construct a
// redacted view rather than risk serializing the upstream credential.
type ClaudeProfileConfig struct {
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	AuthToken   string `json:"-"` // ANTHROPIC_AUTH_TOKEN -> Authorization: Bearer
	APIKey      string `json:"-"` // ANTHROPIC_API_KEY    -> x-api-key
	OpusModel   string `json:"opus_model,omitempty"`
	SonnetModel string `json:"sonnet_model,omitempty"`
	HaikuModel  string `json:"haiku_model,omitempty"`
	// SubagentModel mirrors CLAUDE_CODE_SUBAGENT_MODEL for display in generated
	// client configs. The proxy never rewrites requests based on it.
	SubagentModel string `json:"subagent_model,omitempty"`
}

// ClaudeUpstreamSnapshot is an immutable copy of the switcher's current state.
type ClaudeUpstreamSnapshot struct {
	Active        ClaudeUpstream      `json:"active"`
	ProfileName   string              `json:"profile_name,omitempty"`
	Profile       ClaudeProfileConfig `json:"-"`
	Configured    bool                `json:"configured"`
	HasCredential bool                `json:"has_credential"`
}

// NormalizeClaudeProfile validates and normalizes one profile. Both credential
// flavors may be set (the env parser tolerates pasted launch configs), but a
// request carries Bearer when AuthToken is present — first-party Anthropic
// rejects requests with two credentials.
func NormalizeClaudeProfile(cfg ClaudeProfileConfig) (ClaudeProfileConfig, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.AuthToken = strings.TrimSpace(cfg.AuthToken)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.OpusModel = strings.TrimSpace(cfg.OpusModel)
	cfg.SonnetModel = strings.TrimSpace(cfg.SonnetModel)
	cfg.HaikuModel = strings.TrimSpace(cfg.HaikuModel)
	cfg.SubagentModel = strings.TrimSpace(cfg.SubagentModel)

	if cfg.Name == "" {
		return ClaudeProfileConfig{}, errors.New("name is required")
	}
	if containsControl(cfg.Name) {
		return ClaudeProfileConfig{}, errors.New("name contains control characters")
	}
	if len(cfg.Name) > 128 {
		return ClaudeProfileConfig{}, errors.New("name is too long")
	}
	if strings.EqualFold(cfg.Name, string(ClaudeUpstreamSubscription)) {
		return ClaudeProfileConfig{}, errors.New(`name "subscription" is reserved for the built-in Anthropic login`)
	}
	if cfg.BaseURL == "" {
		return ClaudeProfileConfig{}, errors.New("base_url is required")
	}
	if len(cfg.BaseURL) > 2048 {
		return ClaudeProfileConfig{}, errors.New("base_url is too long")
	}
	u, err := normalizeAPIBaseURL(cfg.BaseURL, "base_url")
	if err != nil {
		return ClaudeProfileConfig{}, err
	}
	// Users habitually paste the versioned root ("https://host/api/v1"); strip
	// one trailing /v1 so both spellings produce /v1/messages downstream.
	if strings.EqualFold(pathLastSegment(u.Path), "v1") {
		u.Path = strings.TrimRight(strings.TrimSuffix(u.Path, pathLastSegment(u.Path)), "/")
	}
	if strings.EqualFold(pathLastSegment(u.Path), "messages") {
		return ClaudeProfileConfig{}, errors.New("base_url must be the API root, not a /messages endpoint")
	}
	cfg.BaseURL = strings.TrimRight(u.String(), "/")

	for _, field := range []struct {
		name  string
		value string
	}{
		{"ANTHROPIC_AUTH_TOKEN", cfg.AuthToken},
		{"ANTHROPIC_API_KEY", cfg.APIKey},
	} {
		if containsControl(field.value) {
			return ClaudeProfileConfig{}, fmt.Errorf("%s contains control characters", field.name)
		}
		if len(field.value) > 16*1024 {
			return ClaudeProfileConfig{}, fmt.Errorf("%s is too long", field.name)
		}
	}
	if cfg.AuthToken == "" && cfg.APIKey == "" {
		return ClaudeProfileConfig{}, errors.New("ANTHROPIC_AUTH_TOKEN 或 ANTHROPIC_API_KEY 必须设置其一")
	}

	for _, field := range []struct {
		name  string
		value string
	}{
		{"opus_model", cfg.OpusModel},
		{"sonnet_model", cfg.SonnetModel},
		{"haiku_model", cfg.HaikuModel},
		{"subagent_model", cfg.SubagentModel},
	} {
		if containsControl(field.value) {
			return ClaudeProfileConfig{}, fmt.Errorf("%s contains control characters", field.name)
		}
		if len(field.value) > 256 {
			return ClaudeProfileConfig{}, fmt.Errorf("%s is too long", field.name)
		}
	}
	return cfg, nil
}

// mapClaudeModel applies the profile's tier mapping: a model name containing
// opus/sonnet/haiku (case-insensitive) maps to the configured value; anything
// else passes through unchanged. An empty target passes through too — silently
// rerouting across tiers would change cost and capability invisibly.
func mapClaudeModel(model string, cfg ClaudeProfileConfig) string {
	lower := strings.ToLower(model)
	switch {
	case cfg.OpusModel != "" && strings.Contains(lower, "opus"):
		return cfg.OpusModel
	case cfg.SonnetModel != "" && strings.Contains(lower, "sonnet"):
		return cfg.SonnetModel
	case cfg.HaikuModel != "" && strings.Contains(lower, "haiku"):
		return cfg.HaikuModel
	}
	return model
}

// patchClaudeProfileBody rewrites only the top-level model field. Unlike the
// subscription path it deliberately does NOT inject the Claude Code system
// identity or metadata.user_id: those are Anthropic subscription fingerprints,
// and third-party upstreams see them as prompt noise that churns cache prefixes.
func patchClaudeProfileBody(raw []byte, cfg ClaudeProfileConfig) []byte {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return raw
	}
	model, ok := payload["model"].(string)
	if !ok {
		return raw
	}
	mapped := mapClaudeModel(model, cfg)
	if mapped == model {
		return raw
	}
	payload["model"] = mapped
	out, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return out
}

// CustomClaudeProvider transparently relays the Anthropic Messages wire API to
// one custom upstream. Its normalized config is immutable, which makes it safe
// to retain for a request while another goroutine replaces the switcher's
// active profile.
type CustomClaudeProvider struct {
	config ClaudeProfileConfig
	client *http.Client
}

// NewCustomClaudeProvider validates cfg and constructs a standalone custom
// Claude provider. Most callers should use ClaudeUpstreamProvider.SetProfile.
func NewCustomClaudeProvider(cfg ClaudeProfileConfig) (*CustomClaudeProvider, error) {
	return newCustomClaudeProvider(cfg, newResponsesHTTPClient())
}

func newCustomClaudeProvider(cfg ClaudeProfileConfig, client *http.Client) (*CustomClaudeProvider, error) {
	normalized, err := NormalizeClaudeProfile(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = newResponsesHTTPClient()
	}
	return &CustomClaudeProvider{config: normalized, client: client}, nil
}

func (c *CustomClaudeProvider) Name() string { return "claude" }

func (c *CustomClaudeProvider) Status() ProviderStatus {
	// Dedupe: profiles commonly map several tiers to the same upstream model,
	// and the dashboard renders every entry as a tag.
	models := make([]string, 0, 3)
	seen := map[string]bool{}
	for _, m := range []string{c.config.OpusModel, c.config.SonnetModel, c.config.HaikuModel} {
		if m != "" && !seen[m] {
			seen[m] = true
			models = append(models, m)
		}
	}
	return ProviderStatus{
		Name:     "claude",
		Title:    c.config.Name,
		LoggedIn: true,
		Detail:   "自定义上游 " + c.config.Name + " · " + c.config.BaseURL,
		Models:   models,
		Endpoint: "/v1/messages",
	}
}

func (c *CustomClaudeProvider) Relay(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		defer r.Body.Close()
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeClaudeUpstreamError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	patched := patchClaudeProfileBody(rawBody, c.config)

	upstreamReq, err := c.newRequest(r.Context(), r.URL.RawQuery, bytes.NewReader(patched), r.Header, int64(len(patched)))
	if err != nil {
		writeClaudeUpstreamError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}
	upstreamResp, err := c.client.Do(upstreamReq)
	if err != nil {
		writeClaudeUpstreamError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer upstreamResp.Body.Close()

	// Anthropic native protocol on both ends — same header whitelist and
	// always-flush copy as the subscription path, so SSE streams through.
	writeClaudeResponseHeaders(w.Header(), upstreamResp.Header)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(upstreamResp.StatusCode)
	streamCopy(w, upstreamResp.Body)
}

func (c *CustomClaudeProvider) newRequest(ctx context.Context, rawQuery string, body io.Reader, headers http.Header, contentLength int64) (*http.Request, error) {
	target := c.config.BaseURL + "/v1/messages"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
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
	if c.config.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.AuthToken)
		req.Header.Del("X-API-Key")
	} else {
		req.Header.Set("X-API-Key", c.config.APIKey)
		req.Header.Del("Authorization")
	}
	// anthropic-beta passes through verbatim on purpose: claudeBetas exists for
	// Anthropic's subscription gating, and the client already sends betas that
	// match its own body fields.
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", anthropicVersion)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	return req, nil
}

func writeClaudeUpstreamError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "upstream_error", "message": message},
	})
}

// ClaudeUpstreamProvider switches /v1/messages between the local Anthropic
// subscription and one custom profile loaded from ferridex-profiles.env.
type ClaudeUpstreamProvider struct {
	mu           sync.RWMutex
	subscription Provider
	profile      *CustomClaudeProvider
	active       ClaudeUpstream
	client       *http.Client
}

func NewClaudeUpstreamProvider(subscription Provider) *ClaudeUpstreamProvider {
	return &ClaudeUpstreamProvider{
		subscription: subscription,
		active:       ClaudeUpstreamSubscription,
		client:       newResponsesHTTPClient(),
	}
}

func (p *ClaudeUpstreamProvider) Name() string {
	if p.subscription != nil {
		return p.subscription.Name()
	}
	return "claude"
}

func (p *ClaudeUpstreamProvider) Relay(w http.ResponseWriter, r *http.Request) {
	source, selected := p.selected()
	if selected == nil {
		message := ErrClaudeSubscriptionNotConfigured.Error()
		if source == ClaudeUpstreamProfile {
			message = ErrClaudeProfileNotConfigured.Error()
		}
		writeClaudeUpstreamError(w, http.StatusServiceUnavailable, message)
		return
	}
	// selected is a stable pointer. SetProfile/Activate can proceed without
	// affecting this request and without holding a mutex across network I/O.
	selected.Relay(w, r)
}

func (p *ClaudeUpstreamProvider) Status() ProviderStatus {
	_, selected := p.selected()
	if selected == nil {
		return ProviderStatus{
			Name:     p.Name(),
			Title:    "Claude",
			Detail:   "当前 Claude 上游未配置",
			Endpoint: "/v1/messages",
		}
	}
	return selected.Status()
}

func (p *ClaudeUpstreamProvider) selected() (ClaudeUpstream, Provider) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.active == ClaudeUpstreamProfile {
		if p.profile == nil {
			return p.active, nil
		}
		return p.active, p.profile
	}
	return p.active, p.subscription
}

// SetProfile validates and atomically loads (or replaces) the switchable
// profile without changing which upstream is active.
func (p *ClaudeUpstreamProvider) SetProfile(cfg ClaudeProfileConfig) error {
	custom, err := newCustomClaudeProvider(cfg, p.client)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.profile = custom
	p.mu.Unlock()
	return nil
}

// ActivateProfile switches future requests to the named profile.
func (p *ClaudeUpstreamProvider) ActivateProfile(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.profile == nil || p.profile.config.Name != name {
		return ErrClaudeProfileNotConfigured
	}
	p.active = ClaudeUpstreamProfile
	return nil
}

// ActivateSubscription switches future requests back to the local login.
func (p *ClaudeUpstreamProvider) ActivateSubscription() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.subscription == nil {
		return ErrClaudeSubscriptionNotConfigured
	}
	p.active = ClaudeUpstreamSubscription
	return nil
}

// RestoreActive restores the user's persisted routing choice at startup. Like
// ResponsesProvider.RestoreSource it records the requested state even when the
// corresponding upstream is unavailable, so Relay fails closed (503) instead of
// silently falling back.
func (p *ClaudeUpstreamProvider) RestoreActive(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if name == "" {
		p.active = ClaudeUpstreamSubscription
		if p.subscription == nil {
			return ErrClaudeSubscriptionNotConfigured
		}
		return nil
	}
	p.active = ClaudeUpstreamProfile
	if p.profile == nil || p.profile.config.Name != name {
		return ErrClaudeProfileNotConfigured
	}
	return nil
}

func (p *ClaudeUpstreamProvider) Snapshot() ClaudeUpstreamSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	snapshot := ClaudeUpstreamSnapshot{Active: p.active}
	if p.profile != nil {
		snapshot.Profile = p.profile.config
		snapshot.Configured = true
		snapshot.HasCredential = p.profile.config.AuthToken != "" || p.profile.config.APIKey != ""
		if p.active == ClaudeUpstreamProfile {
			snapshot.ProfileName = p.profile.config.Name
		}
	}
	return snapshot
}

// QueryUsage deliberately delegates only when the subscription is active. This
// method keeps the existing on-demand usage API available after wrapping Claude.
func (p *ClaudeUpstreamProvider) QueryUsage(ctx context.Context) ([]UsageWindow, error) {
	source, selected := p.selected()
	if source != ClaudeUpstreamSubscription {
		return nil, ErrClaudeUsageUnavailable
	}
	querier, ok := selected.(UsageQuerier)
	if !ok {
		return nil, errors.New("active subscription does not support usage queries")
	}
	return querier.QueryUsage(ctx)
}
