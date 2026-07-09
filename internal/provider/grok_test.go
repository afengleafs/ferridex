package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsGrokPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/grok/v1/chat/completions", true},
		{"/grok/v1/responses", true},
		{"/grok/v1/models", true},
		{"/grok", false},
		{"/v1/responses", false},
		{"/v1/messages", false},
		{"/agent.v1.AgentService/Run", false},
		{"/", false},
		{"/healthz", false},
		{"/api/status", false},
	}
	for _, tt := range tests {
		if got := IsGrokPath(tt.path); got != tt.want {
			t.Fatalf("IsGrokPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestGrokUpstreamPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/grok/v1/chat/completions", "/v1/chat/completions"},
		{"/grok/v1/models", "/v1/models"},
		{"/grok/v1/responses", "/v1/responses"},
	}
	for _, tt := range tests {
		if got := grokUpstreamPath(tt.in); got != tt.want {
			t.Fatalf("grokUpstreamPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsGrokRelayPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", nil)
	if !IsGrokRelayPath(req) {
		t.Fatal("expected grok chat path to relay")
	}
	req = httptest.NewRequest(http.MethodGet, "/grok/v1/models", nil)
	if !IsGrokRelayPath(req) {
		t.Fatal("expected grok models path to relay")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if IsGrokRelayPath(req) {
		t.Fatal("codex path should not relay via grok")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if IsGrokRelayPath(req) {
		t.Fatal("claude path should not relay via grok")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	if IsGrokRelayPath(req) {
		t.Fatal("dashboard API should not relay via grok")
	}
}

func TestGrokTokenValid(t *testing.T) {
	if grokTokenValid("") {
		t.Fatal("empty token should be invalid")
	}
	// exp far in the future (year 2286)
	token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjk5OTk5OTk5OTl9.sig"
	if !grokTokenValid(token) {
		t.Fatal("future exp token should be valid")
	}
	// exp in the past
	token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjF9.sig"
	if grokTokenValid(token) {
		t.Fatal("expired token should be invalid")
	}
}

func TestIsGrokLANPath(t *testing.T) {
	if !IsGrokLANPath("/grok/v1/chat/completions") {
		t.Fatal("grok relay path should be allowed on LAN")
	}
	if IsGrokLANPath("/api/status") {
		t.Fatal("dashboard path should not be allowed on LAN via grok")
	}
}
