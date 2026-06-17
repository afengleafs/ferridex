package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type fakeClock struct {
	nanos atomic.Int64
}

func newFakeClock(now time.Time) *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(now.UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time {
	return time.Unix(0, c.nanos.Load())
}

func (c *fakeClock) Add(d time.Duration) {
	c.nanos.Add(int64(d))
}

func newTestClaudeProvider(clock *fakeClock, transport http.RoundTripper) *ClaudeProvider {
	p := NewClaudeProvider()
	p.client = &http.Client{Transport: transport}
	p.accessToken = func(context.Context) (string, error) { return "test-token", nil }
	p.now = clock.Now
	return p
}

type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header)}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(code int) {
	r.code = code
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}

func (r *responseRecorder) Flush() {}

func relayClaude(p *ClaudeProvider) *responseRecorder {
	req, err := http.NewRequest(http.MethodPost, "http://ferridex.test/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":1,"messages":[]}`,
	))
	if err != nil {
		panic(err)
	}
	rec := newResponseRecorder()
	p.Relay(rec, req)
	return rec
}

func response(status int, header http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     header.Clone(),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestClaudeQuotaReset(t *testing.T) {
	now := time.Date(2026, time.June, 15, 10, 0, 0, 0, time.UTC)
	unix := func(d time.Duration) string { return timeToUnix(now.Add(d)) }

	tests := []struct {
		name      string
		header    http.Header
		wantOK    bool
		wantReset time.Time
		wantLabel string
	}{
		{
			name: "five hour surpassed",
			header: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Surpassed-Threshold": {"true"},
				"Anthropic-Ratelimit-Unified-5h-Reset":               {unix(2 * time.Hour)},
			},
			wantOK:    true,
			wantReset: now.Add(2 * time.Hour),
			wantLabel: "Claude 5 小时限额已耗尽",
		},
		{
			name: "seven day utilization",
			header: http.Header{
				"Anthropic-Ratelimit-Unified-7d-Utilization": {"1.0"},
				"Anthropic-Ratelimit-Unified-7d-Reset":       {unix(24 * time.Hour)},
			},
			wantOK:    true,
			wantReset: now.Add(24 * time.Hour),
			wantLabel: "Claude 7 天限额已耗尽",
		},
		{
			name: "both exhausted chooses later reset",
			header: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Surpassed-Threshold": {"true"},
				"Anthropic-Ratelimit-Unified-5h-Reset":               {unix(2 * time.Hour)},
				"Anthropic-Ratelimit-Unified-7d-Surpassed-Threshold": {"true"},
				"Anthropic-Ratelimit-Unified-7d-Reset":               {unix(24 * time.Hour)},
			},
			wantOK:    true,
			wantReset: now.Add(24 * time.Hour),
			wantLabel: "Claude 7 天限额已耗尽",
		},
		{
			name: "reset without exhausted signal",
			header: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Reset": {unix(2 * time.Hour)},
			},
			wantOK:    true,
			wantReset: now.Add(2 * time.Hour),
			wantLabel: "Claude 5 小时限额已耗尽",
		},
		{
			name: "aggregate reset fallback",
			header: http.Header{
				"Anthropic-Ratelimit-Unified-Reset": {unix(90 * time.Minute)},
			},
			wantOK:    true,
			wantReset: now.Add(90 * time.Minute),
			wantLabel: "Claude 限额已耗尽",
		},
		{
			name: "retry after only is transient",
			header: http.Header{
				"Retry-After": {"30"},
			},
		},
		{
			name: "expired reset",
			header: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Surpassed-Threshold": {"true"},
				"Anthropic-Ratelimit-Unified-5h-Reset":               {unix(-time.Minute)},
			},
		},
		{
			name: "invalid reset",
			header: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Surpassed-Threshold": {"true"},
				"Anthropic-Ratelimit-Unified-5h-Reset":               {"invalid"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reset, label, ok := claudeQuotaReset(tt.header, now)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if !reset.Equal(tt.wantReset) {
				t.Fatalf("reset = %v, want %v", reset, tt.wantReset)
			}
			if label != tt.wantLabel {
				t.Fatalf("label = %q, want %q", label, tt.wantLabel)
			}
		})
	}
}

func TestClaudeRelayQuotaExhaustedCircuitBreaker(t *testing.T) {
	now := time.Date(2026, time.June, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	var calls atomic.Int64
	body := `{"type":"error","error":{"type":"rate_limit_error","message":"You've hit your session limit"}}`
	header := http.Header{
		"Content-Type": {"application/json"},
		"X-Request-Id": {"req_test"},
		"Anthropic-Ratelimit-Unified-5h-Surpassed-Threshold": {"true"},
		"Anthropic-Ratelimit-Unified-5h-Reset":               {timeToUnix(now.Add(2 * time.Hour))},
		"Anthropic-Ratelimit-Unified-5h-Utilization":         {"1.0"},
		"Anthropic-Ratelimit-Unified-5h-Status":              {"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Surpassed-Threshold": {"false"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":         {"0.5"},
		"Anthropic-Ratelimit-Unified-7d-Reset":               {timeToUnix(now.Add(24 * time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Status":              {"allowed"},
		"Anthropic-Organization-Id":                          {"must-not-pass"},
		"Connection":                                         {"close"},
		"Content-Length":                                     {"999"},
	}
	p := newTestClaudeProvider(clock, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusTooManyRequests, header, body), nil
	}))

	for i := 0; i < 2; i++ {
		rec := relayClaude(p)
		if rec.code != http.StatusTooManyRequests {
			t.Fatalf("response %d status = %d", i, rec.code)
		}
		if rec.body.String() != body {
			t.Fatalf("response %d body = %q", i, rec.body.String())
		}
		if rec.Header().Get("X-Should-Retry") != "false" {
			t.Fatalf("response %d missing X-Should-Retry: false", i)
		}
		if rec.Header().Get("Retry-After") != "7200" {
			t.Fatalf("response %d Retry-After = %q", i, rec.Header().Get("Retry-After"))
		}
		if rec.Header().Get("X-Request-Id") != "req_test" {
			t.Fatalf("response %d X-Request-Id = %q", i, rec.Header().Get("X-Request-Id"))
		}
		if rec.Header().Get("Anthropic-Organization-Id") != "" {
			t.Fatalf("response %d leaked disallowed header", i)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	if detail := p.Status().Detail; !strings.Contains(detail, "Claude 5 小时限额已耗尽") {
		t.Fatalf("status detail = %q", detail)
	}

	clock.Add(2*time.Hour + time.Second)
	relayClaude(p)
	if calls.Load() != 2 {
		t.Fatalf("upstream calls after reset = %d, want 2", calls.Load())
	}
}

func TestClaudeRelayTransient429NegativeCache(t *testing.T) {
	now := time.Date(2026, time.June, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	var calls atomic.Int64
	p := newTestClaudeProvider(clock, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusTooManyRequests, http.Header{
			"Content-Type": {"application/json"},
			"Retry-After":  {"2"},
		}, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`), nil
	}))

	for i := 0; i < 2; i++ {
		rec := relayClaude(p)
		if rec.Header().Get("X-Should-Retry") != "" {
			t.Fatalf("transient response %d disabled retries", i)
		}
		if rec.Header().Get("Retry-After") != "2" {
			t.Fatalf("transient response %d Retry-After = %q", i, rec.Header().Get("Retry-After"))
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}

	clock.Add(claudeTransientRateLimitCooldown + time.Millisecond)
	relayClaude(p)
	if calls.Load() != 2 {
		t.Fatalf("upstream calls after cooldown = %d, want 2", calls.Load())
	}
}

func TestClaudeQuotaBreakerCannotBeShortenedByTransient429(t *testing.T) {
	now := time.Date(2026, time.June, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	p := newTestClaudeProvider(clock, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, nil
	}))
	quotaReset := now.Add(2 * time.Hour)

	p.setRateLimit(&claudeRateLimitState{
		until:       quotaReset,
		body:        []byte(`{"type":"error","error":{"message":"quota"}}`),
		header:      make(http.Header),
		label:       "Claude 5 小时限额已耗尽",
		stopRetries: true,
	})
	p.setRateLimit(&claudeRateLimitState{
		until:  now.Add(claudeTransientRateLimitCooldown),
		body:   []byte(`{"type":"error","error":{"message":"transient"}}`),
		header: make(http.Header),
		label:  "Claude 临时限流",
	})

	limit, ok := p.activeRateLimit()
	if !ok {
		t.Fatal("expected active quota breaker")
	}
	if !limit.stopRetries || !limit.until.Equal(quotaReset) {
		t.Fatalf("breaker was shortened: %+v", limit)
	}
	if !strings.Contains(string(limit.body), "quota") {
		t.Fatalf("breaker body was replaced: %s", limit.body)
	}
}

func TestClaudeRelayCopiesAllowedResponseHeaders(t *testing.T) {
	clock := newFakeClock(time.Date(2026, time.June, 15, 10, 0, 0, 0, time.UTC))
	p := newTestClaudeProvider(clock, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, http.Header{
			"Content-Type": {"application/json"},
			"X-Request-Id": {"req_ok"},
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.5"},
			"X-Secret-Upstream-Header":                   {"must-not-pass"},
		}, `{"type":"message"}`), nil
	}))

	rec := relayClaude(p)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d", rec.code)
	}
	if rec.Header().Get("X-Request-Id") != "req_ok" {
		t.Fatalf("X-Request-Id = %q", rec.Header().Get("X-Request-Id"))
	}
	if rec.Header().Get("Anthropic-Ratelimit-Unified-5h-Utilization") != "0.5" {
		t.Fatalf("rate limit header = %q", rec.Header().Get("Anthropic-Ratelimit-Unified-5h-Utilization"))
	}
	if rec.Header().Get("X-Secret-Upstream-Header") != "" {
		t.Fatal("disallowed upstream header was copied")
	}
}

func TestClaudeRateLimitConcurrentReads(t *testing.T) {
	now := time.Date(2026, time.June, 15, 10, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	var calls atomic.Int64
	p := newTestClaudeProvider(clock, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusTooManyRequests, http.Header{
			"Content-Type": {"application/json"},
			"Anthropic-Ratelimit-Unified-5h-Surpassed-Threshold": {"true"},
			"Anthropic-Ratelimit-Unified-5h-Reset":               {timeToUnix(now.Add(time.Hour))},
		}, `{"type":"error"}`), nil
	}))

	relayClaude(p)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := relayClaude(p)
			if rec.code != http.StatusTooManyRequests {
				t.Errorf("status = %d", rec.code)
			}
			_ = p.Status()
		}()
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func timeToUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
