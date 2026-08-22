package server

// publicTunnelManager runs and supervises an `ngrok http` child process, so the
// dashboard can start/stop it. The tunnel points at ferridex's dedicated public
// relay listener (a loopback port that only serves the AI endpoints and always
// requires the key), never at the dashboard port — so the panel and /api/* are
// never reachable over the internet. ngrok tunnels over 443/TLS (and honours an
// HTTP proxy), which gets through restrictive transparent-proxy/VPN setups that
// block cloudflared's 7844 edge port.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ngrokPublicURL matches the public address ngrok prints once the tunnel is up
// (e.g. https://foo-bar-baz.ngrok-free.app, or .ngrok.app / .ngrok.io on paid).
var ngrokPublicURL = regexp.MustCompile(`https://[a-z0-9.-]+\.ngrok(?:-free)?\.(?:app|io)`)

// publicTunnelStartTimeout bounds how long we wait for ngrok to publish a URL
// before giving up and killing it. ngrok normally connects in a couple seconds.
const publicTunnelStartTimeout = 20 * time.Second

// lookupNgrok finds the ngrok binary via PATH, falling back to the Homebrew
// install locations. GUI apps launched from Finder/Dock inherit launchd's
// minimal PATH (no /opt/homebrew/bin), so PATH lookup alone fails inside the
// desktop .app even when ngrok is installed.
func lookupNgrok() (string, error) {
	if bin, err := exec.LookPath("ngrok"); err == nil {
		return bin, nil
	}
	for _, p := range []string{
		"/opt/homebrew/bin/ngrok", // Homebrew on Apple Silicon
		"/usr/local/bin/ngrok",    // Homebrew on Intel
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("ngrok not found")
}

type publicTunnelStatus struct {
	Running   bool   `json:"running"`
	URL       string `json:"url,omitempty"`
	Pid       int    `json:"pid"`
	UptimeSec int64  `json:"uptime_sec"`
	LastError string `json:"last_error"`
}

type publicTunnelManager struct {
	mu         sync.Mutex
	port       string // the loopback public relay port ngrok forwards to
	cmd        *exec.Cmd
	errBuf     *lastLines
	url        string
	startedAt  time.Time
	lastErr    string
	running    bool
	starting   bool // a Start probe is in flight; lock is released during it
	manualStop bool // set by Stop so supervise doesn't report the kill as an error
}

func newPublicTunnelManager(port string) *publicTunnelManager {
	return &publicTunnelManager{port: port}
}

// Start spawns `ngrok http 127.0.0.1:<port>` and waits for it to print its
// public ngrok URL. The blocking wait runs WITHOUT the lock (guarded by
// `starting`) so Status/Stop stay responsive.
func (t *publicTunnelManager) Start() error {
	t.mu.Lock()
	if t.running {
		pid := t.cmd.Process.Pid
		t.mu.Unlock()
		return fmt.Errorf("公网隧道已在运行(pid %d)", pid)
	}
	if t.starting {
		t.mu.Unlock()
		return fmt.Errorf("公网隧道正在启动中,请稍候")
	}
	port := t.port
	t.starting = true
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		t.starting = false
		t.mu.Unlock()
	}()

	bin, err := lookupNgrok()
	if err != nil {
		msg := "未找到 ngrok。请先安装:macOS `brew install ngrok`,然后 `ngrok config add-authtoken <token>`(token 见 https://dashboard.ngrok.com)"
		t.setLastErr(msg)
		return fmt.Errorf("%s", msg)
	}

	errBuf := &lastLines{}
	// `--log stdout` disables the interactive TUI and streams logfmt lines we can
	// scan for the public URL; the process stays alive until killed.
	cmd := exec.Command(bin, "http", "--log", "stdout", "--log-format", "logfmt", "127.0.0.1:"+port)
	cmd.Stdout = errBuf
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		t.setLastErr(err.Error())
		return err
	}

	// A single goroutine owns cmd.Wait(); both the startup probe below and the
	// long-lived supervisor read its result from this channel.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ctx, cancel := context.WithTimeout(context.Background(), publicTunnelStartTimeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			// ngrok exited before publishing a URL: report its output.
			msg := errBuf.String()
			if msg == "" {
				msg = "ngrok 已退出"
			}
			msg = withAuthtokenHint(msg)
			t.setLastErr(msg)
			return fmt.Errorf("%s", msg)
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			msg := "ngrok 启动超时,未获取到公网地址"
			if tail := errBuf.String(); tail != "" {
				msg += ":\n\n" + tail
			}
			msg = withAuthtokenHint(msg)
			t.setLastErr(msg)
			return fmt.Errorf("%s", msg)
		case <-ticker.C:
			if url := ngrokPublicURL.FindString(errBuf.String()); url != "" {
				t.mu.Lock()
				t.cmd = cmd
				t.errBuf = errBuf
				t.url = url
				t.startedAt = time.Now()
				t.running = true
				t.lastErr = ""
				t.manualStop = false
				t.mu.Unlock()
				t.supervise(cmd, errBuf, done)
				return nil
			}
		}
	}
}

func (t *publicTunnelManager) setLastErr(msg string) {
	t.mu.Lock()
	t.lastErr = msg
	t.mu.Unlock()
}

// withAuthtokenHint appends a setup hint when ngrok failed for lack of a
// configured authtoken (its most common first-run failure).
func withAuthtokenHint(msg string) string {
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "authtoken") || strings.Contains(lower, "err_ngrok_4018") {
		return msg + "\n\nngrok 需要先配置 authtoken:运行 `ngrok config add-authtoken <token>`(token 见 https://dashboard.ngrok.com)。"
	}
	return msg
}

// supervise records why the tunnel eventually stopped, reusing the Wait result
// delivered on done (started in Start) so cmd.Wait() is never called twice.
func (t *publicTunnelManager) supervise(cmd *exec.Cmd, errBuf *lastLines, done <-chan error) {
	go func() {
		err := <-done
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.cmd != cmd { // a newer tunnel took over; ignore this exit
			return
		}
		t.running = false
		t.url = ""
		if t.manualStop { // Stop() killed it on purpose: not an error
			t.manualStop = false
			t.lastErr = ""
			return
		}
		if msg := errBuf.String(); msg != "" {
			t.lastErr = msg
		} else if err != nil {
			t.lastErr = err.Error()
		}
	}()
}

func (t *publicTunnelManager) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running || t.cmd == nil || t.cmd.Process == nil {
		return fmt.Errorf("公网隧道未运行")
	}
	t.manualStop = true
	err := t.cmd.Process.Kill()
	t.running = false
	t.url = ""
	return err
}

func (t *publicTunnelManager) Status() publicTunnelStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := publicTunnelStatus{Running: t.running, URL: t.url, LastError: t.lastErr}
	if t.running && t.cmd != nil && t.cmd.Process != nil {
		st.Pid = t.cmd.Process.Pid
		st.UptimeSec = int64(time.Since(t.startedAt).Seconds())
	}
	return st
}
