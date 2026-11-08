// Package ks is the link between the unelevated application and the elevated helper of the kill
// switch (T10.6b). The helper owns what needs administrator rights (the firewall, the DNS policy,
// the tunnel device); the application drives it over a loopback connection with a token, as the
// tunnel helper of v1 is driven. The rules are checked again on the helper's side and the helper
// never removes filters because the connection dropped: that is the whole point of persistent
// filters.
//
// The wire is one JSON object per line. The application sends requests {id, op, ...}, the helper
// answers {id, err, kind, bool} and, on its own, {event:"lost"} when the tunnel is gone.
package ks

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

// HelperFlag as the first argument makes an EchoFog executable run as the kill switch helper.
const HelperFlag = "--ks-helper"

// ErrHelperGone is returned when the helper is not there and cannot be started.
var ErrHelperGone = errors.New("ks: the helper is gone")

// Limits.
const (
	maxLine   = 1 << 20 // the longest line either side reads
	opTimeout = 2 * time.Minute
)

// Backend is what the helper drives: the drivers of the platform.
type Backend struct {
	Firewall tun.Firewall
	Tunnel   tun.Tunnel
	DNS      tun.DNSGuard
}

type request struct {
	ID    uint64         `json:"id"`
	Op    string         `json:"op"`
	Rules []tun.Rule     `json:"rules,omitempty"`
	Tun   *tun.TunConfig `json:"tun,omitempty"`
	DNS   *tun.DNSConfig `json:"dns,omitempty"`
}

type response struct {
	ID    uint64 `json:"id"`
	Event string `json:"event,omitempty"`
	Err   string `json:"err,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Bool  bool   `json:"bool,omitempty"`
}

const (
	kindPrivilege   = "privilege"
	kindUnsupported = "unsupported"
)

func errResponse(id uint64, err error) response {
	r := response{ID: id, Err: err.Error()}
	switch {
	case errors.Is(err, tun.ErrNoPrivilege):
		r.Kind = kindPrivilege
	case errors.Is(err, tun.ErrUnsupported):
		r.Kind = kindUnsupported
	}
	return r
}

func (r response) error() error {
	switch r.Kind {
	case kindPrivilege:
		return fmt.Errorf("%w: %s", tun.ErrNoPrivilege, r.Err)
	case kindUnsupported:
		return fmt.Errorf("%w: %s", tun.ErrUnsupported, r.Err)
	}
	return errors.New(r.Err)
}

// readLine reads one line of at most maxLine bytes.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errors.New("ks: line too long")
	}
	if err != nil {
		return nil, err
	}
	return line, nil
}

// ---- the helper side ----------------------------------------------------------------------------

// Serve answers the requests of one connection until it ends. When it ends, the tunnel is taken
// down (the application is gone) and nothing else is touched. A line it cannot understand ends the
// connection.
func Serve(ctx context.Context, conn net.Conn, b Backend) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	var wmu sync.Mutex
	send := func(r response) {
		raw, _ := json.Marshal(r)
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, _ = conn.Write(append(raw, '\n'))
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	// The loss of the tunnel is told to the application as it happens.
	if ev := b.Tunnel.Events(); ev != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				case e, ok := <-ev:
					if !ok {
						return
					}
					msg := "the tunnel was lost"
					if e.Err != nil {
						msg = e.Err.Error()
					}
					send(response{Event: "lost", Err: msg})
				}
			}
		}()
	}
	var opmu sync.Mutex // operations run one at a time, in the order they came
	rd := bufio.NewReaderSize(conn, maxLine)
	var serveErr error
	for {
		line, err := readLine(rd)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
				serveErr = err
			}
			break
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil || !knownOp(req.Op) {
			serveErr = errors.New("ks: bad request")
			break
		}
		wg.Add(1)
		go func(req request) {
			defer wg.Done()
			opmu.Lock()
			defer opmu.Unlock()
			octx, ocancel := context.WithTimeout(ctx, opTimeout)
			defer ocancel()
			send(handle(octx, b, req))
		}(req)
	}
	// The application is gone: stop what is running, take the tunnel down, leave the filters.
	cancel()
	close(done)
	wg.Wait()
	dctx, dcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer dcancel()
	_ = b.Tunnel.Down(dctx)
	return serveErr
}

func knownOp(op string) bool {
	switch op {
	case "hello", "fw.install", "fw.remove", "fw.installed", "tun.up", "tun.down", "tun.alive", "dns.protect", "dns.restore":
		return true
	}
	return false
}

func handle(ctx context.Context, b Backend, r request) response {
	ok := response{ID: r.ID}
	fail := func(err error) response { return errResponse(r.ID, err) }
	switch r.Op {
	case "hello":
		return ok
	case "fw.install":
		// The helper does not take the application's word: an open set is refused here too.
		if err := tun.CheckRules(r.Rules); err != nil {
			return fail(err)
		}
		if err := b.Firewall.Install(ctx, r.Rules); err != nil {
			return fail(err)
		}
	case "fw.remove":
		if err := b.Firewall.Remove(ctx); err != nil {
			return fail(err)
		}
	case "fw.installed":
		v, err := b.Firewall.Installed(ctx)
		if err != nil {
			return fail(err)
		}
		ok.Bool = v
	case "tun.up":
		if r.Tun == nil {
			return fail(errors.New("ks: tun.up without a configuration"))
		}
		if err := b.Tunnel.Up(ctx, *r.Tun); err != nil {
			return fail(err)
		}
	case "tun.down":
		if err := b.Tunnel.Down(ctx); err != nil {
			return fail(err)
		}
	case "tun.alive":
		ok.Bool = b.Tunnel.Alive(ctx)
	case "dns.protect":
		if r.DNS == nil {
			return fail(errors.New("ks: dns.protect without a configuration"))
		}
		if err := b.DNS.Protect(ctx, *r.DNS); err != nil {
			return fail(err)
		}
	case "dns.restore":
		if err := b.DNS.Restore(ctx); err != nil {
			return fail(err)
		}
	}
	return ok
}

// ServeHelper is the main of the helper process: args follow HelperFlag (-ctl host:port -token t;
// other arguments are left to build's caller). build makes the Backend and a function to run when
// the helper ends. It returns the exit code.
func ServeHelper(args []string, build func() (Backend, func(), error)) int {
	var ctl, token string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-ctl":
			ctl = args[i+1]
		case "-token":
			token = args[i+1]
		}
	}
	host, _, err := net.SplitHostPort(ctl)
	if err != nil || host != "127.0.0.1" || token == "" {
		return 2
	}
	conn, err := net.DialTimeout("tcp", ctl, 5*time.Second)
	if err != nil {
		return 1
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, token+"\n"); err != nil {
		return 1
	}
	b, cleanup, err := build()
	if err != nil {
		_, _ = io.WriteString(conn, "err "+oneLine(err.Error())+"\n")
		return 1
	}
	if cleanup != nil {
		defer cleanup()
	}
	if _, err := io.WriteString(conn, "ok\n"); err != nil {
		return 1
	}
	if err := Serve(context.Background(), conn, b); err != nil {
		return 1
	}
	return 0
}

func oneLine(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

// ---- starting the helper ------------------------------------------------------------------------

// Launch starts the helper executable with arguments, elevated.
type Launch func(exe string, args []string) error

// Connector returns the function a Client uses to get a connection to a freshly started helper:
// it listens on the loopback, starts the helper with the address and a random token, and takes the
// first connection that sends the token. Anything else that connects is dropped. The timeout covers
// the prompt for administrator rights.
func Connector(launch Launch, exe string, extra []string, timeout time.Duration) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer ln.Close()
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		token := hex.EncodeToString(raw)
		args := append([]string{HelperFlag, "-ctl", ln.Addr().String(), "-token", token}, extra...)
		if err := launch(exe, args); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		go func() { <-ctx.Done(); ln.Close() }()
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return nil, errors.New("ks: the helper did not start (was the administrator prompt declined?)")
				}
				return nil, err
			}
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
			rd := bufio.NewReaderSize(conn, 4096)
			first, err := readLine(rd)
			if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(first))), []byte(token)) != 1 {
				conn.Close() // some other local process: keep waiting for ours
				continue
			}
			status, err := readLine(rd)
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("ks: the helper ended while starting: %w", err)
			}
			if s := strings.TrimSpace(string(status)); s != "ok" {
				conn.Close()
				return nil, fmt.Errorf("ks: the helper refused to start: %s", strings.TrimPrefix(s, "err "))
			}
			_ = conn.SetDeadline(time.Time{})
			if rd.Buffered() > 0 {
				return nil, errors.New("ks: unexpected data from the helper")
			}
			return conn, nil
		}
	}
}

// ---- the application side -----------------------------------------------------------------------

// ClientConfig wires a Client.
type ClientConfig struct {
	// Connect starts the helper and returns the connection to it (see Connector).
	Connect func(ctx context.Context) (net.Conn, error)
	// Installed answers "are there filters" without the helper: a restart of the application asks
	// before any helper (and any prompt for rights) exists. Nil: ask the helper when there is one.
	Installed func(ctx context.Context) (bool, error)
}

// Client implements tun.Firewall, tun.Tunnel and tun.DNSGuard over the helper.
type Client struct {
	cfg    ClientConfig
	events chan tun.TunEvent

	connMu sync.Mutex // one helper start at a time
	mu     sync.Mutex // guards cc and closed
	cc     *clientConn
	closed bool
}

// NewClient returns a Client. The helper is started when the first operation needs it.
func NewClient(cfg ClientConfig) *Client {
	return &Client{cfg: cfg, events: make(chan tun.TunEvent, 4)}
}

type clientConn struct {
	owner *Client
	conn  net.Conn
	wmu   sync.Mutex

	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan response
	dead    bool
}

func (c *Client) live() *clientConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cc != nil && !c.cc.isDead() {
		return c.cc
	}
	return nil
}

func (cc *clientConn) isDead() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.dead
}

// ensure returns the live connection, starting a helper if there is none.
func (c *Client) ensure(ctx context.Context) (*clientConn, error) {
	if cc := c.live(); cc != nil {
		return cc, nil
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if cc := c.live(); cc != nil {
		return cc, nil
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed || c.cfg.Connect == nil {
		return nil, ErrHelperGone
	}
	conn, err := c.cfg.Connect(ctx)
	if err != nil {
		return nil, err
	}
	cc := &clientConn{owner: c, conn: conn, pending: map[uint64]chan response{}}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		conn.Close()
		return nil, ErrHelperGone
	}
	c.cc = cc
	c.mu.Unlock()
	go cc.readLoop()
	return cc, nil
}

func (cc *clientConn) readLoop() {
	rd := bufio.NewReaderSize(cc.conn, maxLine)
	var cause error
	for {
		line, err := readLine(rd)
		if err != nil {
			cause = err
			break
		}
		var r response
		if err := json.Unmarshal(line, &r); err != nil {
			cause = err
			break
		}
		if r.Event == "lost" {
			cc.owner.emit(errors.New(r.Err))
			continue
		}
		cc.mu.Lock()
		ch := cc.pending[r.ID]
		delete(cc.pending, r.ID)
		cc.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
	cc.mu.Lock()
	cc.dead = true
	for id, ch := range cc.pending {
		delete(cc.pending, id)
		close(ch)
	}
	cc.mu.Unlock()
	_ = cc.conn.Close()
	cc.owner.mu.Lock()
	closed := cc.owner.closed
	cc.owner.mu.Unlock()
	if !closed {
		cc.owner.emit(fmt.Errorf("the helper went away: %v", cause))
	}
}

func (c *Client) emit(err error) {
	select {
	case c.events <- tun.TunEvent{Err: err}:
	default: // the Switch polls Alive as well
	}
}

func (cc *clientConn) call(ctx context.Context, req request) (response, error) {
	ch := make(chan response, 1)
	cc.mu.Lock()
	if cc.dead {
		cc.mu.Unlock()
		return response{}, ErrHelperGone
	}
	cc.next++
	req.ID = cc.next
	cc.pending[req.ID] = ch
	cc.mu.Unlock()
	raw, err := json.Marshal(req)
	if err != nil {
		return response{}, err
	}
	werr := make(chan error, 1)
	go func() {
		cc.wmu.Lock()
		defer cc.wmu.Unlock()
		_, err := cc.conn.Write(append(raw, '\n'))
		werr <- err
	}()
	forget := func() {
		cc.mu.Lock()
		delete(cc.pending, req.ID)
		cc.mu.Unlock()
	}
	select {
	case err := <-werr:
		if err != nil {
			forget()
			return response{}, fmt.Errorf("%w: %v", ErrHelperGone, err)
		}
	case <-ctx.Done():
		forget()
		return response{}, ctx.Err()
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return response{}, ErrHelperGone
		}
		return r, nil
	case <-ctx.Done():
		forget()
		return response{}, ctx.Err()
	}
}

func (c *Client) do(ctx context.Context, req request) (response, error) {
	cc, err := c.ensure(ctx)
	if err != nil {
		return response{}, err
	}
	r, err := cc.call(ctx, req)
	if err != nil {
		return response{}, err
	}
	if r.Err != "" {
		return r, r.error()
	}
	return r, nil
}

func (c *Client) Install(ctx context.Context, rules []tun.Rule) error {
	_, err := c.do(ctx, request{Op: "fw.install", Rules: rules})
	return err
}

// Remove asks the helper to take the filters away. The helper is let go of afterwards: nothing is
// left for it to do.
func (c *Client) Remove(ctx context.Context) error {
	_, err := c.do(ctx, request{Op: "fw.remove"})
	if err == nil {
		c.dropHelper()
	}
	return err
}

func (c *Client) dropHelper() {
	c.mu.Lock()
	cc := c.cc
	c.cc = nil
	c.mu.Unlock()
	if cc != nil {
		cc.mu.Lock()
		cc.dead = true
		cc.mu.Unlock()
		_ = cc.conn.Close()
	}
}

func (c *Client) Installed(ctx context.Context) (bool, error) {
	if c.cfg.Installed != nil {
		return c.cfg.Installed(ctx)
	}
	if cc := c.live(); cc != nil {
		r, err := cc.call(ctx, request{Op: "fw.installed"})
		if err != nil {
			return false, err
		}
		if r.Err != "" {
			return false, r.error()
		}
		return r.Bool, nil
	}
	return false, nil
}

func (c *Client) Up(ctx context.Context, cfg tun.TunConfig) error {
	_, err := c.do(ctx, request{Op: "tun.up", Tun: &cfg})
	return err
}

// Down asks a running helper to take the tunnel down; with no helper there is nothing to take down
// and nothing is started.
func (c *Client) Down(ctx context.Context) error {
	cc := c.live()
	if cc == nil {
		return nil
	}
	r, err := cc.call(ctx, request{Op: "tun.down"})
	if err != nil {
		if errors.Is(err, ErrHelperGone) {
			return nil
		}
		return err
	}
	if r.Err != "" {
		return r.error()
	}
	return nil
}

func (c *Client) Events() <-chan tun.TunEvent { return c.events }

// Alive asks the helper whether the tunnel is up; no helper means it is not.
func (c *Client) Alive(ctx context.Context) bool {
	cc := c.live()
	if cc == nil {
		return false
	}
	r, err := cc.call(ctx, request{Op: "tun.alive"})
	return err == nil && r.Err == "" && r.Bool
}

func (c *Client) Protect(ctx context.Context, cfg tun.DNSConfig) error {
	_, err := c.do(ctx, request{Op: "dns.protect", DNS: &cfg})
	return err
}

func (c *Client) Restore(ctx context.Context) error {
	_, err := c.do(ctx, request{Op: "dns.restore"})
	return err
}

// Close lets go of the helper. The filters stay; the helper takes the tunnel down.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	cc := c.cc
	c.cc = nil
	c.mu.Unlock()
	if cc != nil {
		_ = cc.conn.Close()
	}
	return nil
}

var (
	_ tun.Firewall = (*Client)(nil)
	_ tun.Tunnel   = (*Client)(nil)
	_ tun.DNSGuard = (*Client)(nil)
)
