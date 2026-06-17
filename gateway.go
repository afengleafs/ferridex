package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const autoPortSearchLimit = 100

// Provider is one upstream subscription channel (Codex or Claude).
type Provider interface {
	Name() string
	Relay(w http.ResponseWriter, r *http.Request)
	Status() ProviderStatus
}

// ProviderStatus is what the dashboard shows for one provider.
type ProviderStatus struct {
	Name     string   `json:"name"`
	Title    string   `json:"title"`
	LoggedIn bool     `json:"logged_in"`
	Account  string   `json:"account,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Models   []string `json:"models,omitempty"`
	Endpoint string   `json:"endpoint"`
}

// --- config (optional downstream API key) ---

type config struct {
	DownstreamKey string `json:"downstream_key"`
	LANKey        string `json:"lan_key,omitempty"`
	TunnelRemote  string `json:"tunnel_remote,omitempty"`
	TunnelKey     string `json:"tunnel_key,omitempty"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ferridex", "config.json")
}

func loadConfig() config {
	var c config
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c config) error {
	p := configPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// --- live request log hub (for the dashboard log stream) ---

type logEntry struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Dur    string `json:"dur"`
}

type logHub struct {
	mu   sync.Mutex
	ring []logEntry
	subs map[chan logEntry]struct{}
}

var hub = &logHub{subs: map[chan logEntry]struct{}{}}

func (h *logHub) add(e logEntry) {
	h.mu.Lock()
	h.ring = append(h.ring, e)
	if len(h.ring) > 200 {
		h.ring = h.ring[len(h.ring)-200:]
	}
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *logHub) subscribe() (chan logEntry, []logEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan logEntry, 32)
	h.subs[ch] = struct{}{}
	return ch, append([]logEntry(nil), h.ring...)
}

func (h *logHub) unsubscribe(ch chan logEntry) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// statusRecorder captures the response status and preserves Flush for SSE.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		e := logEntry{
			Time:   start.Format("15:04:05"),
			Method: r.Method,
			Path:   r.URL.Path,
			Status: rec.status,
			Dur:    time.Since(start).Round(time.Millisecond).String(),
		}
		log.Printf("%s %s -> %d (%s)", e.Method, e.Path, e.Status, e.Dur)
		// only surface relay traffic in the live panel (not dashboard polling/assets)
		switch r.URL.Path {
		case "/v1/responses", "/responses", "/v1/messages", "/messages":
			hub.add(e)
		}
	})
}

func checkKey(r *http.Request, key string) bool {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") && auth[7:] == key {
		return true
	}
	return r.Header.Get("X-API-Key") == key
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func buildMux(dashboard, lan bool, lanKey string, tm *tunnelManager, providers ...Provider) *http.ServeMux {
	mux := http.NewServeMux()
	cfg := loadConfig()

	relay := func(p Provider) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if lan && !isLoopbackRemote(r) {
				// LAN client: always require the auto-generated LAN key.
				if !checkKey(r, lanKey) {
					http.Error(w, "invalid or missing API key", http.StatusUnauthorized)
					return
				}
			} else if cfg.DownstreamKey != "" && !checkKey(r, cfg.DownstreamKey) {
				// Local client: only enforced when a key is configured.
				http.Error(w, "invalid or missing API key", http.StatusUnauthorized)
				return
			}
			p.Relay(w, r)
		}
	}

	for _, p := range providers {
		switch p.Name() {
		case "codex":
			mux.HandleFunc("POST /v1/responses", relay(p))
			mux.HandleFunc("POST /responses", relay(p))
		case "claude":
			mux.HandleFunc("POST /v1/messages", relay(p))
			mux.HandleFunc("POST /messages", relay(p))
		}
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	if dashboard {
		ps := providers
		mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
			sts := make([]ProviderStatus, 0, len(ps))
			for _, p := range ps {
				sts = append(sts, p.Status())
			}
			writeJSON(w, map[string]any{"providers": sts})
		})
		mux.HandleFunc("GET /api/netinfo", func(w http.ResponseWriter, r *http.Request) {
			info := map[string]any{"lan_enabled": lan}
			if lan {
				info["lan_ips"] = lanIPv4s()
				info["lan_key"] = lanKey
			}
			writeJSON(w, info)
		})
		if tm != nil {
			mux.HandleFunc("GET /api/tunnel", func(w http.ResponseWriter, r *http.Request) {
				cfg := loadConfig()
				writeJSON(w, map[string]any{"status": tm.Status(), "remote": cfg.TunnelRemote, "key": cfg.TunnelKey})
			})
			mux.HandleFunc("POST /api/tunnel/start", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Remote string `json:"remote"`
					Key    string `json:"key"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if err := tm.Start(body.Remote, body.Key); err != nil {
					writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "status": tm.Status()})
					return
				}
				cfg := loadConfig()
				cfg.TunnelRemote = strings.TrimSpace(body.Remote)
				cfg.TunnelKey = strings.TrimSpace(body.Key)
				_ = saveConfig(cfg)
				writeJSON(w, map[string]any{"ok": true, "status": tm.Status()})
			})
			mux.HandleFunc("POST /api/tunnel/stop", func(w http.ResponseWriter, r *http.Request) {
				err := tm.Stop()
				m := map[string]any{"ok": err == nil, "status": tm.Status()}
				if err != nil {
					m["error"] = err.Error()
				}
				writeJSON(w, m)
			})
		}
		mux.HandleFunc("GET /api/logs/stream", logsStreamHandler)
		mux.Handle("GET /", dashboardHandler())
	}
	return mux
}

func logsStreamHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, recent := hub.subscribe()
	defer hub.unsubscribe(ch)

	for _, e := range recent {
		fmt.Fprintf(w, "data: %s\n\n", marshalJSONLine(e))
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", marshalJSONLine(e))
			flusher.Flush()
		case <-time.After(20 * time.Second):
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// --- subcommand runners ---

func runServe(addr string, open, lan, newKey, autoPort bool, providers ...Provider) {
	if newKey && !lan {
		log.Printf("提示:-new-key 需配合 -lan 使用,本次忽略")
	}
	bindAddr := addr
	if lan {
		port := portOf(addr)
		bindAddr = "0.0.0.0:" + port
	}

	ln, err := listenWithAutoPort(bindAddr, autoPort)
	if err != nil {
		log.Fatal(err)
	}
	actualPort := listenerPort(ln)
	if actualPort == "" {
		_ = ln.Close()
		log.Fatalf("无法解析实际监听端口: %s", ln.Addr())
	}
	if requestedPort := portOf(bindAddr); autoPort && requestedPort != actualPort {
		log.Printf("端口 %s 已被占用,已自动切换到 %s", requestedPort, actualPort)
	}

	browserAddr := browserAddrFor(addr, actualPort, lan)
	browserURL := "http://" + browserAddr + "/"
	lanKey := ""
	if lan {
		lanKey = ensureLANKey(newKey)
	}

	tm := newTunnelManager(net.JoinHostPort("127.0.0.1", actualPort))
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		_ = tm.Stop()
		os.Exit(0)
	}()
	mux := buildMux(true, lan, lanKey, tm, providers...)

	log.Printf("ferridex serve")
	log.Printf("  面板   %s", browserURL)
	if lan {
		ips := lanIPv4s()
		if len(ips) == 0 {
			log.Printf("  局域网 未检测到局域网 IPv4 地址")
		}
		for _, ip := range ips {
			log.Printf("  局域网 http://%s:%s  (Codex/Claude 接口,需密钥)", ip, actualPort)
		}
		log.Printf("  局域网密钥已写入面板「远程客户端配置」,复制给另一台电脑即可")
	} else {
		log.Printf("  Codex  POST http://%s/v1/responses", browserAddr)
		log.Printf("  Claude POST http://%s/v1/messages", browserAddr)
	}
	logClaudeBreakerStatus(providers...)
	if open {
		go func() {
			time.Sleep(400 * time.Millisecond)
			openBrowser(browserURL)
		}()
	}
	if err := http.Serve(ln, logging(lanGate(lan, mux))); err != nil {
		log.Fatal(err)
	}
}

func runSingle(addr string, autoPort bool, p Provider) {
	ln, err := listenWithAutoPort(addr, autoPort)
	if err != nil {
		log.Fatal(err)
	}
	actualPort := listenerPort(ln)
	if actualPort == "" {
		_ = ln.Close()
		log.Fatalf("无法解析实际监听端口: %s", ln.Addr())
	}
	if requestedPort := portOf(addr); autoPort && requestedPort != actualPort {
		log.Printf("端口 %s 已被占用,已自动切换到 %s", requestedPort, actualPort)
	}
	browserAddr := browserAddrFor(addr, actualPort, false)
	mux := buildMux(false, false, "", nil, p)
	log.Printf("ferridex %s — POST http://%s%s", p.Name(), browserAddr, p.Status().Endpoint)
	logClaudeBreakerStatus(p)
	if err := http.Serve(ln, logging(mux)); err != nil {
		log.Fatal(err)
	}
}

func listenWithAutoPort(addr string, autoPort bool) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	if !autoPort || !isAddrInUse(err) {
		return nil, err
	}

	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return nil, err
	}
	start, parseErr := strconv.Atoi(port)
	if parseErr != nil || start <= 0 || start >= 65535 {
		return nil, err
	}

	lastErr := err
	for p := start + 1; p <= start+autoPortSearchLimit && p <= 65535; p++ {
		nextAddr := net.JoinHostPort(host, strconv.Itoa(p))
		ln, err := net.Listen("tcp", nextAddr)
		if err == nil {
			return ln, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("listen %s failed and no free port found in next %d ports: %w", addr, autoPortSearchLimit, lastErr)
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

func listenerPort(ln net.Listener) string {
	if ln == nil {
		return ""
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		return strconv.Itoa(tcp.Port)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return ""
	}
	return port
}

func browserAddrFor(addr, port string, lan bool) string {
	host := "127.0.0.1"
	if !lan {
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func logClaudeBreakerStatus(providers ...Provider) {
	for _, p := range providers {
		if p.Name() == "claude" {
			log.Printf("  Claude 限额熔断 已启用(官方 reset / 瞬态 5s)")
			return
		}
	}
}

func printStatus(providers ...Provider) {
	for _, p := range providers {
		s := p.Status()
		mark := "✗ 未登录"
		if s.LoggedIn {
			mark = "✓ 已登录"
		}
		fmt.Printf("%-8s %s", s.Name, mark)
		if s.Account != "" {
			fmt.Printf("  account=%s", s.Account)
		}
		if s.Detail != "" {
			fmt.Printf("  (%s)", s.Detail)
		}
		fmt.Println()
	}
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "darwin":
		err = exec.Command("open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		err = exec.Command("xdg-open", url).Start()
	}
	if err != nil {
		log.Printf("无法自动打开浏览器,请手动访问 %s", url)
	}
}
