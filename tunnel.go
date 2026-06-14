package main

// tunnelManager runs and supervises an `ssh -N -R` reverse tunnel as a child
// process, so the dashboard can start/stop it. The forwarded port is ferridex's
// own listen port (one tunnel covers both /v1/responses and /v1/messages).

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type tunnelStatus struct {
	Running   bool   `json:"running"`
	Remote    string `json:"remote"`
	Pid       int    `json:"pid"`
	UptimeSec int64  `json:"uptime_sec"`
	LastError string `json:"last_error"`
}

// lastLines is a thread-safe bounded sink for a child's stderr.
type lastLines struct {
	mu  sync.Mutex
	buf []byte
}

func (l *lastLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > 2048 {
		l.buf = l.buf[len(l.buf)-2048:]
	}
	l.mu.Unlock()
	return len(p), nil
}

func (l *lastLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(string(l.buf))
}

type tunnelManager struct {
	mu        sync.Mutex
	addr      string // ferridex listen addr; the tunnel forwards this port
	cmd       *exec.Cmd
	errBuf    *lastLines
	remote    string
	keyPath   string
	startedAt time.Time
	lastErr   string
	running   bool
}

func newTunnelManager(addr string) *tunnelManager {
	return &tunnelManager{addr: addr}
}

func (t *tunnelManager) port() string {
	if i := strings.LastIndex(t.addr, ":"); i >= 0 {
		return t.addr[i+1:]
	}
	return t.addr
}

// Start spawns `ssh -N -R 127.0.0.1:<port>:127.0.0.1:<port> ... <remote>`.
// BatchMode=yes => no interactive password/passphrase prompt, so the key must
// be passphrase-less or loaded in ssh-agent.
func (t *tunnelManager) Start(remote, keyPath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return fmt.Errorf("隧道已在运行(pid %d)", t.cmd.Process.Pid)
	}
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return fmt.Errorf("远端不能为空(填 user@host)")
	}
	keyPath = strings.TrimSpace(keyPath)

	port := t.port()
	fwd := fmt.Sprintf("127.0.0.1:%s:127.0.0.1:%s", port, port)
	args := []string{
		"-N",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "BatchMode=yes",
	}
	if keyPath != "" {
		args = append(args, "-o", "IdentitiesOnly=yes", "-i", keyPath)
	}
	args = append(args, "-R", fwd, remote)

	errBuf := &lastLines{}
	cmd := exec.Command("ssh", args...)
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		t.lastErr = err.Error()
		return err
	}

	t.cmd = cmd
	t.errBuf = errBuf
	t.remote = remote
	t.keyPath = keyPath
	t.startedAt = time.Now()
	t.running = true
	t.lastErr = ""

	go func() {
		err := cmd.Wait()
		t.mu.Lock()
		t.running = false
		if msg := errBuf.String(); msg != "" {
			t.lastErr = msg
		} else if err != nil {
			t.lastErr = err.Error()
		}
		t.mu.Unlock()
	}()
	return nil
}

func (t *tunnelManager) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running || t.cmd == nil || t.cmd.Process == nil {
		return fmt.Errorf("隧道未运行")
	}
	err := t.cmd.Process.Kill()
	t.running = false
	return err
}

func (t *tunnelManager) Status() tunnelStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := tunnelStatus{Running: t.running, Remote: t.remote, LastError: t.lastErr}
	if t.errBuf != nil {
		if live := t.errBuf.String(); live != "" {
			st.LastError = live
		}
	}
	if t.running && t.cmd != nil && t.cmd.Process != nil {
		st.Pid = t.cmd.Process.Pid
		st.UptimeSec = int64(time.Since(t.startedAt).Seconds())
	}
	return st
}
