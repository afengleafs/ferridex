package main

// tunnelManager runs and supervises an `ssh -N -R` reverse tunnel as a child
// process, so the dashboard can start/stop it. The forwarded port is ferridex's
// own listen port (one tunnel covers both /v1/responses and /v1/messages).

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tunnelRemotePortSearchLimit bounds how many remote listen ports we try when
// the remote refuses to bind the requested one ("remote port forwarding
// failed"). The remote forwarding port is independent of ferridex's local
// listen port, so -auto-port cannot help here; we retry the -R remote port
// ourselves.
const tunnelRemotePortSearchLimit = 20

// tunnelForwardGrace is how long we wait after launching ssh before deciding a
// candidate remote port worked. With ExitOnForwardFailure=yes, ssh exits fast
// when the remote bind fails, so a process still alive after this window is
// treated as a successful forward.
const tunnelForwardGrace = 2500 * time.Millisecond

type tunnelStatus struct {
	Running    bool   `json:"running"`
	Remote     string `json:"remote"`
	RemotePort string `json:"remote_port,omitempty"`
	Pid        int    `json:"pid"`
	UptimeSec  int64  `json:"uptime_sec"`
	LastError  string `json:"last_error"`
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
	mu         sync.Mutex
	addr       string // ferridex listen addr; the tunnel forwards this port
	cmd        *exec.Cmd
	errBuf     *lastLines
	remote     string
	remotePort string // remote listen port chosen for -R (may differ from local)
	keyPath    string
	startedAt  time.Time
	lastErr    string
	running    bool
	starting   bool // a Start probe is in flight; lock is released during it
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

// Start spawns `ssh -N -R 127.0.0.1:<rport>:127.0.0.1:<lport> ... <remote>`.
// The remote listen port <rport> is decoupled from ferridex's local listen
// port <lport>: if the remote refuses to bind it ("remote port forwarding
// failed"), Start retries on the next remote port automatically.
// BatchMode=yes => no interactive password/passphrase prompt, so the key must
// be passphrase-less or loaded in ssh-agent.
func (t *tunnelManager) Start(remote, keyPath string) error {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return fmt.Errorf("远端不能为空(填 user@host)")
	}
	keyPath = strings.TrimSpace(keyPath)

	// Claim the right to start. The blocking ssh probes below run WITHOUT the
	// lock so Status/Stop stay responsive; `starting` guards against a second
	// concurrent Start.
	t.mu.Lock()
	if t.running {
		pid := t.cmd.Process.Pid
		t.mu.Unlock()
		return fmt.Errorf("隧道已在运行(pid %d)", pid)
	}
	if t.starting {
		t.mu.Unlock()
		return fmt.Errorf("隧道正在启动中,请稍候")
	}
	localPort := t.port()
	start, err := strconv.Atoi(localPort)
	if err != nil || start <= 0 || start > 65535 {
		t.mu.Unlock()
		return fmt.Errorf("无效的本地端口 %q", localPort)
	}
	t.starting = true
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		t.starting = false
		t.mu.Unlock()
	}()

	var lastMsg string
	tried := 0
	for rport := start; rport <= start+tunnelRemotePortSearchLimit && rport <= 65535; rport++ {
		tried++
		rportStr := strconv.Itoa(rport)
		fwd := fmt.Sprintf("127.0.0.1:%s:127.0.0.1:%s", rportStr, localPort)
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
			msg := explainTunnelError(err.Error(), localPort, remote, tried)
			t.setLastErr(msg)
			return err
		}

		// A single goroutine owns cmd.Wait(); both the grace check below and the
		// long-lived supervisor read its result from this channel.
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case <-done:
			// ssh exited within the grace window. With ExitOnForwardFailure=yes
			// a remote bind failure looks like this; classify and decide.
			lastMsg = errBuf.String()
			if strings.Contains(strings.ToLower(lastMsg), "remote port forwarding failed") {
				continue // remote port busy: try the next one
			}
			// Auth failure / forwarding disabled / unreachable: don't keep trying ports.
			msg := explainTunnelError(lastMsg, rportStr, remote, tried)
			t.setLastErr(msg)
			return fmt.Errorf("%s", msg)
		case <-time.After(tunnelForwardGrace):
			// Still alive => forward established. Commit the state under the lock.
			t.mu.Lock()
			t.cmd = cmd
			t.errBuf = errBuf
			t.remote = remote
			t.remotePort = rportStr
			t.keyPath = keyPath
			t.startedAt = time.Now()
			t.running = true
			t.lastErr = ""
			t.mu.Unlock()
			t.supervise(cmd, errBuf, done, localPort, remote)
			return nil
		}
	}

	if lastMsg == "" {
		lastMsg = "remote port forwarding failed"
	}
	msg := explainTunnelError(lastMsg, localPort, remote, tried)
	t.setLastErr(msg)
	return fmt.Errorf("%s", msg)
}

func (t *tunnelManager) setLastErr(msg string) {
	t.mu.Lock()
	t.lastErr = msg
	t.mu.Unlock()
}

// supervise records why the tunnel eventually stopped. It reuses the Wait
// result delivered on done (started in Start), so cmd.Wait() is never called twice.
func (t *tunnelManager) supervise(cmd *exec.Cmd, errBuf *lastLines, done <-chan error, localPort, remote string) {
	go func() {
		err := <-done
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.cmd != cmd { // a newer tunnel took over; ignore this exit
			return
		}
		t.running = false
		if msg := errBuf.String(); msg != "" {
			t.lastErr = explainTunnelError(msg, localPort, remote, 0)
		} else if err != nil {
			t.lastErr = explainTunnelError(err.Error(), localPort, remote, 0)
		}
	}()
}

func explainTunnelError(msg, port, remote string, tried int) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "remote port forwarding failed"):
		retried := ""
		if tried > 1 {
			retried = fmt.Sprintf("已自动尝试 %d 个远端端口仍失败。", tried)
		}
		return fmt.Sprintf("%s\n\n%s远端的 127.0.0.1 端口无法绑定，通常是该端口已被旧隧道或其他服务占用。到远端 %s 执行 `lsof -nP -iTCP:%s -sTCP:LISTEN` 查占用进程，停止后重试；或换一台远端主机。", msg, retried, remote, port)
	case strings.Contains(lower, "administratively prohibited") ||
		strings.Contains(lower, "remote forwarding disabled") ||
		strings.Contains(lower, "tcp forwarding disabled"):
		return msg + "\n\n远端 SSH 服务禁止端口转发。需要在远端 sshd 配置中允许 `AllowTcpForwarding yes`，然后重启 sshd；没有管理员权限时请改用局域网直连或换一台远端主机。"
	default:
		return msg
	}
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
	st := tunnelStatus{Running: t.running, Remote: t.remote, RemotePort: t.remotePort, LastError: t.lastErr}
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
