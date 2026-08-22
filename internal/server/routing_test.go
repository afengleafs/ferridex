package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsRelayLogPath(t *testing.T) {
	if !isRelayLogPath("/agent.v1.AgentService/Run") {
		t.Fatal("cursor connect path should appear in relay logs")
	}
	if !isRelayLogPath("/v1/messages") {
		t.Fatal("claude path should appear in relay logs")
	}
	if isRelayLogPath("/api/status") {
		t.Fatal("dashboard API should not appear in relay logs")
	}
}

func TestCheckKeyRejectsEmptyConfiguredKey(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if checkKey(request, "") {
		t.Fatal("checkKey accepted an absent credential when the configured key was empty")
	}
	request.Header.Set("Authorization", "Bearer ")
	request.Header.Set("X-API-Key", "")
	if checkKey(request, "") {
		t.Fatal("checkKey accepted an explicitly empty credential")
	}
}

func TestListenSafetyOnlyAcceptsLoopbackHosts(t *testing.T) {
	for address, want := range map[string]bool{
		"127.0.0.1:8788":   true,
		"localhost:8788":   true,
		"[::1]:8788":       true,
		"0.0.0.0:8788":     false,
		":8788":            false,
		"192.168.1.2:8788": false,
		"example.com:8788": false,
	} {
		if got := isLoopbackHost(address); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", address, got, want)
		}
	}
}

func TestWithCORSRejectsMaliciousOrigin(t *testing.T) {
	called := false
	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8788/api/responses-upstream/save", nil)
	request.Header.Set("Origin", "https://attacker.example")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if called {
		t.Fatal("malicious cross-origin request reached the protected handler")
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("rejected response unexpectedly allowed origin %q", got)
	}

	// Matching Host and Origin are not enough: accepting a non-loopback host
	// would make the local API vulnerable to DNS rebinding.
	rebinding := httptest.NewRequest(http.MethodPost, "http://panel.attacker.test/api/responses-upstream/save", nil)
	rebinding.Header.Set("Origin", "http://panel.attacker.test")
	rebindingRecorder := httptest.NewRecorder()
	handler.ServeHTTP(rebindingRecorder, rebinding)
	if rebindingRecorder.Code != http.StatusForbidden {
		t.Fatalf("DNS-rebinding status = %d, want 403", rebindingRecorder.Code)
	}
}

func TestWithCORSAllowsSameOriginAndTauri(t *testing.T) {
	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	tests := []struct {
		name   string
		origin string
	}{
		{name: "same loopback origin", origin: "http://127.0.0.1:8788"},
		{name: "Tauri origin", origin: "tauri://localhost"},
		{name: "Tauri local domain", origin: "http://tauri.localhost"},
		{name: "Vite development origin", origin: "http://localhost:1420"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8788/api/status", nil)
			request.Header.Set("Origin", test.origin)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != test.origin {
				t.Fatalf("allow-origin = %q, want %q", got, test.origin)
			}
		})
	}
}

func TestLANGateKeepsResponsesManagementLocalOnly(t *testing.T) {
	handler := lanGate(true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	management := httptest.NewRequest(http.MethodGet, "http://ferridex.local/api/responses-upstream", nil)
	management.RemoteAddr = "192.168.1.23:54321"
	managementRecorder := httptest.NewRecorder()
	handler.ServeHTTP(managementRecorder, management)
	if managementRecorder.Code != http.StatusForbidden {
		t.Fatalf("LAN management status = %d, want 403", managementRecorder.Code)
	}

	relay := httptest.NewRequest(http.MethodPost, "http://ferridex.local/v1/responses", nil)
	relay.RemoteAddr = "192.168.1.23:54321"
	relayRecorder := httptest.NewRecorder()
	handler.ServeHTTP(relayRecorder, relay)
	if relayRecorder.Code != http.StatusOK {
		t.Fatalf("LAN relay status = %d, want 200", relayRecorder.Code)
	}
}
