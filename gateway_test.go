package main

import (
	"net"
	"strconv"
	"testing"
)

func TestListenWithAutoPortDisabledReturnsOriginalError(t *testing.T) {
	occupied := occupyLocalPort(t)
	defer occupied.Close()

	addr := occupied.Addr().String()
	ln, err := listenWithAutoPort(addr, false)
	if err == nil {
		ln.Close()
		t.Fatal("expected occupied port error")
	}
}

func TestListenWithAutoPortFallsBackWhenOccupied(t *testing.T) {
	occupied := occupyLocalPort(t)
	defer occupied.Close()

	addr := occupied.Addr().String()
	ln, err := listenWithAutoPort(addr, true)
	if err != nil {
		t.Fatalf("listenWithAutoPort returned error: %v", err)
	}
	defer ln.Close()

	if listenerPort(ln) == listenerPort(occupied) {
		t.Fatalf("auto port reused occupied port %s", listenerPort(ln))
	}
}

func TestBrowserAddrFor(t *testing.T) {
	tests := []struct {
		name string
		addr string
		port string
		lan  bool
		want string
	}{
		{name: "loopback", addr: "127.0.0.1:8788", port: "8790", want: "127.0.0.1:8790"},
		{name: "wildcard", addr: "0.0.0.0:8788", port: "8790", want: "127.0.0.1:8790"},
		{name: "lan forces loopback panel", addr: "192.168.1.2:8788", port: "8790", lan: true, want: "127.0.0.1:8790"},
		{name: "ipv6", addr: "[::1]:8788", port: "8790", want: "[::1]:8790"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := browserAddrFor(tt.addr, tt.port, tt.lan); got != tt.want {
				t.Fatalf("browserAddrFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func occupyLocalPort(t *testing.T) net.Listener {
	t.Helper()
	for i := 0; i < 20; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to occupy local port: %v", err)
		}
		port := listenerPort(ln)
		n, err := strconv.Atoi(port)
		if err != nil {
			ln.Close()
			t.Fatalf("failed to parse listener port %q: %v", port, err)
		}
		if n <= 65535-autoPortSearchLimit-1 {
			return ln
		}
		ln.Close()
	}
	t.Fatal("failed to allocate an occupied port with fallback range")
	return nil
}
