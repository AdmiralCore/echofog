// Package tunmode sends the traffic of every application on the computer through the EchoFog
// exit. A small elevated helper (the same executable started with HelperFlag) owns the TUN
// device: tun2socks feeds it into the node's local SOCKS5 proxy, which answers DNS with fake IPs
// and relays TCP and UDP through the exit by domain name.
//
// The helper lives exactly as long as its control connection to the node: closing it, or the
// node dying, tears the device and the routes down.
package tunmode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HelperFlag as the first argument makes an EchoFog executable run as the TUN helper.
const HelperFlag = "--tun-helper"

const (
	lineUp      = "UP"
	linePrefErr = "ERR "
	// startTimeout covers the UAC / polkit prompt the user has to answer.
	startTimeout = 2 * time.Minute
)

var ErrUnsupported = errors.New("whole-computer mode is supported on Windows and Linux")

// Controller is the node-side handle of the helper.
type Controller struct {
	mu   sync.Mutex
	conn net.Conn
	done chan struct{}
	// OnExit, if set, is called when a running helper goes away without Stop.
	OnExit func(err error)
	// launch starts the helper; tests replace it.
	launch func(exe string, args []string) error
}

// SetOnExit installs the callback for a helper that goes away without Stop.
func (c *Controller) SetOnExit(f func(err error)) {
	c.mu.Lock()
	c.OnExit = f
	c.mu.Unlock()
}

func (c *Controller) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Start launches the elevated helper and waits until the TUN device is up. socksAddr is the
// node's local proxy; bypassUDPPort is the node's own UDP port, which must not enter the TUN.
func (c *Controller) Start(ctx context.Context, socksAddr string, bypassUDPPort int) error {
	launch := c.launch
	if launch == nil {
		if !Supported() {
			return ErrUnsupported
		}
		launch = launchElevated
	}
	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return errors.New("whole-computer mode is already on")
	}
	c.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	token := hex.EncodeToString(tok)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{HelperFlag,
		"-socks", socksAddr,
		"-ctl", ln.Addr().String(),
		"-token", token,
		"-bypass-udp-port", strconv.Itoa(bypassUDPPort),
	}
	if err := launch(exe, args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	go func() { <-ctx.Done(); ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return errors.New("the helper did not start (was the administrator prompt declined?)")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		status, err := handshake(conn, token)
		if err != nil {
			conn.Close()
			if errors.Is(err, errBadToken) {
				continue // some other local process: keep waiting for ours
			}
			return err
		}
		if status != lineUp {
			conn.Close()
			return errors.New(strings.TrimPrefix(status, linePrefErr))
		}
		_ = conn.SetDeadline(time.Time{})
		done := make(chan struct{})
		c.mu.Lock()
		c.conn, c.done = conn, done
		c.mu.Unlock()
		go c.watch(conn, done)
		return nil
	}
}

var errBadToken = errors.New("bad helper token")

func handshake(conn net.Conn, token string) (string, error) {
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(conn)
	got, err := r.ReadString('\n')
	if err != nil || strings.TrimSpace(got) != token {
		return "", errBadToken
	}
	status, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("the helper exited during start-up: %w", err)
	}
	return strings.TrimSpace(status), nil
}

func (c *Controller) watch(conn net.Conn, done chan struct{}) {
	_, err := io.Copy(io.Discard, conn)
	c.mu.Lock()
	stopped := c.conn != conn // Stop already took it
	if !stopped {
		c.conn = nil
	}
	cb := c.OnExit
	c.mu.Unlock()
	close(done)
	if !stopped && cb != nil {
		if err == nil {
			err = errors.New("the helper exited")
		}
		cb(err)
	}
}

// Stop tells the helper to remove the TUN device and waits briefly for the connection to end.
func (c *Controller) Stop() {
	c.mu.Lock()
	conn, done := c.conn, c.done
	c.conn = nil
	c.mu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}
