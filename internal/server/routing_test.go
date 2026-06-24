package server

import "testing"

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
