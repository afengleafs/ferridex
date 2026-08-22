package server

import "testing"

func TestTunnelManagerSeparatesRelayAndRemotePorts(t *testing.T) {
	manager := newTunnelManager("127.0.0.1:8789", "8788")
	if got := manager.port(); got != "8789" {
		t.Fatalf("local relay port = %q, want 8789", got)
	}
	if got := manager.initialRemotePort(); got != "8788" {
		t.Fatalf("initial remote port = %q, want 8788", got)
	}

	legacy := newTunnelManager("127.0.0.1:9000")
	if got := legacy.initialRemotePort(); got != "9000" {
		t.Fatalf("default initial remote port = %q, want local port", got)
	}
}
