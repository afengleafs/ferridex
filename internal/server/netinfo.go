package server

// LAN access helpers: detect this host's private IPv4 addresses, tell loopback
// from LAN callers, and manage the auto-generated key that LAN clients must
// present. Used by `serve -lan` to expose only the AI endpoints to the LAN while
// keeping the dashboard and tunnel controls local-only.

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"strings"

	"ferridex/internal/provider"
)

// portOf extracts the port from a listen address like "127.0.0.1:8788".
func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

// primaryLANIP returns the source IPv4 the OS would use for outbound traffic
// (no packets are sent). This is the address a LAN peer should connect to.
func primaryLANIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	// Only trust a private route as "the LAN IP"; a default route over a VPN
	// (e.g. 198.18.x) is not what a Wi-Fi peer should connect to.
	if ip := ua.IP.To4(); ip != nil && ip.IsPrivate() {
		return ip.String()
	}
	return ""
}

// lanIPv4s returns this host's private IPv4 addresses, most-likely-primary
// first, for telling a LAN peer which URL to hit.
func lanIPv4s() []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(primaryLANIP())
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip := ipnet.IP.To4(); ip != nil && !ip.IsLoopback() && ip.IsPrivate() {
			add(ip.String())
		}
	}
	return out
}

// isLoopbackRemote reports whether the request originates from this machine.
func isLoopbackRemote(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// randomKey returns a URL-safe random secret for LAN downstream auth.
func randomKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// ensureLANKey returns the key LAN clients must present, generating and
// persisting one on first use. A user-set downstream_key takes precedence so
// the two auth mechanisms stay consistent. When rotate is true, a fresh lan_key
// is generated even if one already exists (the `-new-key` flag).
func ensureLANKey(rotate bool) string {
	cfg := loadConfig()
	if cfg.DownstreamKey != "" {
		if rotate {
			log.Printf("已设置 downstream_key,优先生效;-new-key 对 lan_key 无效")
		}
		return cfg.DownstreamKey
	}
	if cfg.LANKey != "" && !rotate {
		return cfg.LANKey
	}
	cfg.LANKey = randomKey()
	if err := saveConfig(cfg); err != nil {
		log.Printf("无法保存局域网密钥到 %s: %v", configPath(), err)
	}
	if rotate {
		log.Printf("已重新生成局域网密钥")
	}
	return cfg.LANKey
}

// lanGate, when enabled, lets only the relay + health endpoints be reached from
// non-loopback (LAN) clients; the dashboard and /api/* stay local-only.
func lanGate(enabled bool, next http.Handler) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackRemote(r) {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		switch path {
		case "/v1/responses", "/responses", "/v1/messages", "/messages", "/healthz":
			next.ServeHTTP(w, r)
			return
		}
		if provider.IsCursorLANPath(path) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "forbidden: local-only endpoint", http.StatusForbidden)
	})
}
