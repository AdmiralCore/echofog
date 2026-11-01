package tunmode

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Kill switch. Started with a hold file, the helper does not take the TUN down when the node goes
// away without saying STOP (a crash, a killed process, a lost control connection): the device and
// its routes stay, every connection still goes to the node's proxy port, and with nothing there
// nothing gets out. The computer is offline instead of quietly on its real address. The helper then
// waits for a node to adopt it again (the app restarted and its proxy is back on the same port) or
// to be told to stop, and writes how to reach it to the hold file.
//
// Control lines from the node: STOP (take the TUN down), HOLD 1 / HOLD 0 (keep blocking if the
// node goes away, or not). To a held helper: the token, then ADOPT <node UDP port> or STOP.

const (
	cmdStop  = "STOP"
	cmdHold  = "HOLD"
	cmdAdopt = "ADOPT"
	lineOK   = "OK"
)

// HoldState is what a held helper writes to the hold file: where to reach it.
type HoldState struct {
	Port  int       `json:"port"`
	Token string    `json:"token"`
	Socks string    `json:"socks"`
	PID   int       `json:"pid"`
	Since time.Time `json:"since"`
}

// DefaultHoldFile is where the app keeps the hold file: one per computer user, whatever profile.
func DefaultHoldFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "EchoFog", "killswitch.json")
}

// ReadHold reads the hold file: a helper is holding the computer's traffic.
func ReadHold(path string) (HoldState, bool) {
	var st HoldState
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &st) != nil || st.Port == 0 || st.Token == "" {
		return HoldState{}, false
	}
	return st, true
}

func writeHold(path string, st HoldState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dialHold connects to the held helper and authenticates.
func dialHold(st HoldState, timeout time.Duration) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(st.Port)), timeout)
	if err != nil {
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, st.Token+"\n"); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, bufio.NewReader(conn), nil
}

// ReleaseHold tells a held helper to take the TUN down: the computer goes back on its own network.
// A hold file whose helper is gone (the computer restarted) is removed.
func ReleaseHold(path string) error {
	st, ok := ReadHold(path)
	if !ok {
		return nil
	}
	conn, r, err := dialHold(st, 3*time.Second)
	if err != nil {
		_ = os.Remove(path) // nobody holds: the adapter went with the helper
		return nil
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, cmdStop+"\n"); err != nil {
		return err
	}
	line, err := r.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != lineOK {
		return fmt.Errorf("the helper did not confirm: %v", err)
	}
	return nil
}

// helperSession is the helper's control loop, apart from the device so tests can drive it.
type helperSession struct {
	holdFile string // "": no kill switch, the TUN goes with the control connection
	hold     bool   // keep blocking when the node goes away (HOLD 1 / HOLD 0)
	socks    string
	flush    func()               // flush the DNS cache: the fake IPs of the old node mean nothing
	rebypass func(port int) error // keep the adopting node's UDP port off the TUN (Linux)
	logf     func(format string, a ...any)
}

// run serves conn and then, while the kill switch holds, the nodes that adopt the helper; it
// returns when the TUN is to come down.
func (h *helperSession) run(conn net.Conn, stop <-chan struct{}) {
	for conn != nil {
		if h.serve(conn, stop) || !h.hold || h.holdFile == "" {
			return
		}
		conn = h.waitAdopt(stop)
	}
}

// serve reads the node's control lines until STOP (true), a stop signal (true) or the end of the
// connection (false).
func (h *helperSession) serve(conn net.Conn, stop <-chan struct{}) bool {
	lines := make(chan string)
	go func() {
		defer close(lines)
		r := bufio.NewReader(conn)
		for {
			l, err := r.ReadString('\n')
			if l = strings.TrimSpace(l); l != "" {
				lines <- l
			}
			if err != nil {
				return
			}
		}
	}()
	defer conn.Close()
	for {
		select {
		case <-stop:
			return true
		case l, ok := <-lines:
			if !ok {
				return false
			}
			switch {
			case l == cmdStop:
				return true
			case strings.HasPrefix(l, cmdHold+" "):
				h.hold = strings.TrimPrefix(l, cmdHold+" ") == "1"
			}
		}
	}
}

// waitAdopt holds: the TUN stays, the hold file says where to reach the helper, and the first node
// that comes with the token and ADOPT takes over. nil: told to stop.
func (h *helperSession) waitAdopt(stop <-chan struct{}) net.Conn {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.logf("tun-helper: cannot hold the kill switch: %v", err)
		return nil
	}
	defer ln.Close()
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	st := HoldState{Port: ln.Addr().(*net.TCPAddr).Port, Token: hex.EncodeToString(tok), Socks: h.socks, PID: os.Getpid(), Since: time.Now()}
	if err := writeHold(h.holdFile, st); err != nil {
		h.logf("tun-helper: cannot write the hold file, so nothing can release the kill switch: %v", err)
		return nil
	}
	defer os.Remove(h.holdFile)
	h.logf("tun-helper: the node went away; traffic stays blocked until it comes back (kill switch)")

	conns := make(chan net.Conn)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			conns <- c
		}
	}()
	for {
		select {
		case <-stop:
			return nil
		case c, ok := <-conns:
			if !ok {
				return nil
			}
			if next, done := h.answer(c, st.Token); done {
				return next
			}
		}
	}
}

// answer handles one connection to the held helper: (adopting conn, true), (nil, true) to stop, or
// (nil, false) to keep holding.
func (h *helperSession) answer(c net.Conn, token string) (net.Conn, bool) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	got, err := r.ReadString('\n')
	if err != nil || strings.TrimSpace(got) != token {
		c.Close() // some other local process
		return nil, false
	}
	cmd, err := r.ReadString('\n')
	if err != nil {
		c.Close()
		return nil, false
	}
	cmd = strings.TrimSpace(cmd)
	switch {
	case cmd == cmdStop:
		_ = os.Remove(h.holdFile) // before the answer: whoever reads it next must not find a helper gone
		_, _ = io.WriteString(c, lineOK+"\n")
		c.Close()
		return nil, true
	case cmd == cmdAdopt || strings.HasPrefix(cmd, cmdAdopt+" "):
		if p, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(cmd, cmdAdopt))); err == nil && p > 0 && h.rebypass != nil {
			if err := h.rebypass(p); err != nil {
				_, _ = io.WriteString(c, linePrefErr+err.Error()+"\n")
				c.Close()
				return nil, false
			}
		}
		if h.flush != nil {
			h.flush()
		}
		_ = os.Remove(h.holdFile) // adopted: nobody else is to find it
		if _, err := io.WriteString(c, lineUp+"\n"); err != nil {
			c.Close()
			return nil, false
		}
		_ = c.SetDeadline(time.Time{})
		h.logf("tun-helper: a node took the tunnel over again")
		return c, true
	}
	c.Close()
	return nil, false
}

var errNoHold = errors.New("no helper holds the tunnel")
