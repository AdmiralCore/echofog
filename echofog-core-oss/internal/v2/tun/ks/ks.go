// Package ks is the link between the unelevated application and the elevated helper of the kill
// switch (T10.6b). The helper owns what needs administrator rights (the firewall, the DNS policy,
// the tunnel device); the application drives it over a loopback connection with a token, as the
// tunnel helper of v1 is driven. The rules are checked again on the helper's side and the helper
// never removes filters because the connection dropped: that is the whole point of persistent
// filters.
package ks

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

// HelperFlag as the first argument makes an EchoFog executable run as the kill switch helper.
const HelperFlag = "--ks-helper"

// ErrHelperGone is returned when the helper is not there and cannot be started.
var ErrHelperGone = errors.New("ks: the helper is gone")

// Backend is what the helper drives: the drivers of the platform.
type Backend struct {
	Firewall tun.Firewall
	Tunnel   tun.Tunnel
	DNS      tun.DNSGuard
}

// Serve answers the requests of one connection until it ends. When it ends, the tunnel is taken
// down (the application is gone) and nothing else is touched.
func Serve(ctx context.Context, conn net.Conn, b Backend) error { return errors.New("not implemented") }

// ServeHelper is the main of the helper process: args follow HelperFlag. build makes the Backend
// (and a function to run when the helper ends). It returns the exit code.
func ServeHelper(args []string, build func() (Backend, func(), error)) int { return 2 }

// Launch starts the helper executable with arguments, elevated.
type Launch func(exe string, args []string) error

// Connector returns the function a Client uses to get a connection to a freshly started helper.
func Connector(launch Launch, exe string, extra []string, timeout time.Duration) func(ctx context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) { return nil, errors.New("not implemented") }
}

// ClientConfig wires a Client.
type ClientConfig struct {
	// Connect starts the helper and returns the connection to it (see Connector).
	Connect func(ctx context.Context) (net.Conn, error)
	// Installed answers "are there filters" without the helper: a restart of the application asks
	// before any helper (and any prompt for rights) exists.
	Installed func(ctx context.Context) (bool, error)
}

// Client implements tun.Firewall, tun.Tunnel and tun.DNSGuard over the helper.
type Client struct{ cfg ClientConfig }

// NewClient returns a Client. The helper is started when the first operation needs it.
func NewClient(cfg ClientConfig) *Client { return &Client{cfg: cfg} }

func (c *Client) Install(ctx context.Context, rules []tun.Rule) error {
	return errors.New("not implemented")
}
func (c *Client) Remove(ctx context.Context) error { return errors.New("not implemented") }
func (c *Client) Installed(ctx context.Context) (bool, error) {
	return false, errors.New("not implemented")
}
func (c *Client) Up(ctx context.Context, cfg tun.TunConfig) error {
	return errors.New("not implemented")
}
func (c *Client) Down(ctx context.Context) error { return errors.New("not implemented") }
func (c *Client) Events() <-chan tun.TunEvent    { return nil }
func (c *Client) Alive(ctx context.Context) bool { return false }
func (c *Client) Protect(ctx context.Context, cfg tun.DNSConfig) error {
	return errors.New("not implemented")
}
func (c *Client) Restore(ctx context.Context) error { return errors.New("not implemented") }

// Close lets go of the helper. The filters stay.
func (c *Client) Close() error { return nil }

var (
	_ tun.Firewall = (*Client)(nil)
	_ tun.Tunnel   = (*Client)(nil)
	_ tun.DNSGuard = (*Client)(nil)
)
