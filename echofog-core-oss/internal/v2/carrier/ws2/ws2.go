// Package ws2 is the ws2 masking suite (wire.md §4.4): reality2-tcp inside the binary messages of
// a WebSocket that reaches the node through a CDN or a reverse proxy.
//
// The outer TLS to the CDN is a browser hello (uTLS); the WebSocket path carries an unguessable
// component from the card and every other path serves an ordinary site. Inside the WebSocket
// runs the TLS of reality2-tcp with its token (package reality2 does the whole inner side); a
// connection without a valid token is closed with code 1008 after uniform(0.05, 0.5) s, as a
// generic WebSocket back end would close a message it does not understand.
package ws2

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// MaxMessage is the largest binary message either side sends or accepts (wire.md §4.4).
const MaxMessage = 16 << 10

// ParseURL returns the token address (lower-case host and port, port 443 by default) and the
// request path of a ws2 address URL. Only wss:// is accepted: the outer TLS is not optional.
func ParseURL(s string) (hostPort, path string, err error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", "", fmt.Errorf("ws2: url: %w", err)
	}
	if u.Scheme != "wss" {
		return "", "", errors.New("ws2: the url is not wss://")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || u.User != nil {
		return "", "", errors.New("ws2: the url has no host or has user info")
	}
	if _, err := netip.ParseAddr(host); err != nil && !validHostname(host) {
		return "", "", errors.New("ws2: bad host")
	}
	port := "443"
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return "", "", errors.New("ws2: bad port")
		}
		port = strconv.FormatUint(n, 10)
	}
	path = u.Path
	if path == "" {
		path = "/"
	}
	return net.JoinHostPort(host, port), path, nil
}

// Register installs the suite's dialer and listener in reg. Either may be nil.
func Register(reg *carrier.Registry, d *Dialer, l *Listener) error {
	if reg == nil {
		return errors.New("ws2: nil carrier registry")
	}
	if d == nil && l == nil {
		return errors.New("ws2: nothing to register")
	}
	if d != nil {
		reg.RegisterDialer(objects.SuiteWS2, d)
	}
	if l != nil {
		reg.RegisterListener(objects.SuiteWS2, l)
	}
	return nil
}

// validHostname accepts the characters of a DNS name; anything else is not a host of a card.
func validHostname(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
		default:
			return false
		}
	}
	return true
}
