package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsCursorConnectPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/agent.v1.AgentService/Run", true},
		{"/aiserver.v1.ChatService/StreamUnifiedChatWithTools", true},
		{"/aiserver.v1.AiService/AvailableModels", true},
		{"/auth/usage", true},
		{"/oauth/token", true},
		{"/v1/traces", true},
		{"/v1/responses", false},
		{"/v1/messages", false},
		{"/healthz", false},
		{"/api/status", false},
	}
	for _, tt := range tests {
		if got := IsCursorConnectPath(tt.path); got != tt.want {
			t.Fatalf("IsCursorConnectPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestCursorUpstreamBase(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/agent.v1.AgentService/Run", cursorAgentBase},
		{"/aiserver.v1.ChatService/StreamUnifiedChatWithTools", cursorAgentBase},
		{"/aiserver.v1.AiService/AvailableModels", cursorAPI2Base},
		{"/auth/usage", cursorAPI2Base},
	}
	for _, tt := range tests {
		if got := cursorUpstreamBase(tt.path); got != tt.want {
			t.Fatalf("cursorUpstreamBase(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestIsCursorRelayPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/agent.v1.AgentService/Run", nil)
	if !IsCursorRelayPath(req) {
		t.Fatal("expected agent run path to relay")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if IsCursorRelayPath(req) {
		t.Fatal("claude path should not relay via cursor")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	if IsCursorRelayPath(req) {
		t.Fatal("dashboard API should not relay via cursor")
	}
}

func TestCursorTokenValid(t *testing.T) {
	if cursorTokenValid("") {
		t.Fatal("empty token should be invalid")
	}
	// exp far in the future (year 2286)
	token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjk5OTk5OTk5OTl9.sig"
	if !cursorTokenValid(token) {
		t.Fatal("future exp token should be valid")
	}
	// exp in the past
	token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjF9.sig"
	if cursorTokenValid(token) {
		t.Fatal("expired token should be invalid")
	}
}

func TestIsCursorLANPath(t *testing.T) {
	if !IsCursorLANPath("/oauth/token") {
		t.Fatal("oauth path should be allowed on LAN")
	}
}
