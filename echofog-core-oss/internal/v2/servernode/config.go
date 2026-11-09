// Package servernode is the headless server node of EchoFog v2 (T12.4a): the profile on disk, the
// configuration, the operator API with tokens and the `echofog-node v2` commands. The protocol is
// the runtime of internal/v2/node; this package only wires a server profile around it.
//
// A server is exposed to the internet, so the defaults are closed: the operator API listens on a
// loopback IP literal only and answers only to a bearer token and loopback Host names; there is no
// debug or profiling endpoint; the CDN clock check stays off (the runtime has no HTTP client
// unless one with a uTLS hello is supplied); keys live in the profile directory with mode 0600
// and in no log.
package servernode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
)

// ErrConfig is a configuration that is refused.
var ErrConfig = errors.New("servernode: bad configuration")

// MaxConfigSize bounds the configuration file.
const MaxConfigSize = 64 << 10

const maxListeners = 8

// ListenSpec is one carrier listener of the node.
type ListenSpec struct {
	// Suite is the carrier suite: noise3, reality2-quic, reality2-tcp or ws2. The camouflaged
	// suites (reality2-*, ws2) need a cover (T2.6e).
	Suite string `json:"suite"`
	// Bind is the local host:port. Advertise is the IP literal written into the card.
	Bind      string `json:"bind"`
	Advertise string `json:"advertise"`
}

// OperatorSpec configures the operator API.
type OperatorSpec struct {
	// Addr is a loopback IP literal with a port ("127.0.0.1:0" picks a free one).
	Addr string `json:"addr"`
}

// CoverACMESpec is how the cover obtains its certificate from a CA (RFC 8555) when cover_cert
// is empty. AcceptTOS records the operator's acceptance of the CA's terms of service.
type CoverACMESpec struct {
	// DirectoryURL is the ACME directory; empty means the production CA.
	DirectoryURL string `json:"directory,omitempty"`
	// Email is the optional contact of the account.
	Email string `json:"email,omitempty"`
	// AcceptTOS records that the operator accepted the CA's terms of service.
	AcceptTOS bool `json:"accept_tos"`
	// Challenge is "tls-alpn-01", "http-01" or empty (the CA's choice, tls-alpn-01 first).
	Challenge string `json:"challenge,omitempty"`
}

// Config is server.json in the data directory.
type Config struct {
	Version int          `json:"version"`
	Listen  []ListenSpec `json:"listen"`
	// InviteListen is the separate listener that invitations point to. Without it invitations are
	// off.
	InviteListen *ListenSpec `json:"invite_listen,omitempty"`
	// Roles are relay, exit, mailbox and dht-server.
	Roles []string `json:"roles"`
	// Cover is host:port of the site the camouflaged suites imitate. Without a camouflaged listener
	// the node has no public card (D37) and tells the operator.
	Cover string `json:"cover,omitempty"`
	// SiteDir is the folder with the site the cover imitates (T12.5); CoverCert is the certificate
	// file of the cover domain, which the doctor watches for expiry.
	SiteDir   string `json:"site_dir,omitempty"`
	CoverCert string `json:"cover_cert,omitempty"`
	// CoverACME configures the certificate when CoverCert is empty (T2.6e).
	CoverACME *CoverACMESpec `json:"cover_acme,omitempty"`
	Operator  OperatorSpec   `json:"operator"`
	// Bootstrap are the entry nodes this server joins the network through, one T5.5 line each
	// (T5.6): a card link or reality2-*/noise3 address. The server does not trust a line until the
	// runtime verifies it.
	Bootstrap []string `json:"bootstrap,omitempty"`
}

var allRoles = map[string]bool{"relay": true, "exit": true, "mailbox": true, "dht-server": true}

var allSuites = map[string]bool{"noise3": true, "reality2-quic": true, "reality2-tcp": true, "ws2": true}

// camouflaged reports a suite that speaks to a stranger through the real cover site. Until the
// own-cover mode exists for it, such a listener needs the site folder: a foreign site is not
// served (T2.6e, НС-3 fail-closed).
func camouflaged(suite string) bool { return suite != "noise3" }

// hasCamouflaged reports whether any listener of the configuration is camouflaged.
func (c *Config) hasCamouflaged() bool {
	for _, l := range c.Listen {
		if camouflaged(l.Suite) {
			return true
		}
	}
	return c.InviteListen != nil && camouflaged(c.InviteListen.Suite)
}

// hasReality2 reports whether the node serves reality2 and therefore needs a certificate for the
// cover. ws2 terminates TLS at the CDN, so it alone does not.
func (c *Config) hasReality2() bool {
	check := func(l ListenSpec) bool { return l.Suite == "reality2-quic" || l.Suite == "reality2-tcp" }
	for _, l := range c.Listen {
		if check(l) {
			return true
		}
	}
	return c.InviteListen != nil && check(*c.InviteListen)
}

// DefaultConfig is a server with the default roles and the operator API on loopback. It has no
// listener: the operator must say where the node is reachable.
func DefaultConfig() Config {
	return Config{Version: 1, Roles: []string{"relay", "mailbox", "dht-server"}, Operator: OperatorSpec{Addr: "127.0.0.1:0"}}
}

// ParseConfig reads and validates a configuration. Unknown fields, trailing data and anything
// over MaxConfigSize are refused.
func ParseConfig(b []byte) (*Config, error) {
	if len(b) > MaxConfigSize {
		return nil, fmt.Errorf("%w: %d bytes, at most %d", ErrConfig, len(b), MaxConfigSize)
	}
	b = stripComments(b)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: data after the configuration", ErrConfig)
	}
	// An absent "roles" means the defaults; a present one, even empty, is what the operator chose
	// (T12.4a acceptance M1). null is neither.
	var probe map[string]json.RawMessage
	_ = json.Unmarshal(b, &probe)
	if raw, present := probe["roles"]; !present {
		c.Roles = DefaultConfig().Roles
	} else if string(bytes.TrimSpace(raw)) == "null" {
		return nil, fmt.Errorf("%w: roles is null; use [] to switch every role off", ErrConfig)
	} else if c.Roles == nil {
		c.Roles = []string{}
	}
	if c.Operator.Addr == "" {
		c.Operator.Addr = DefaultConfig().Operator.Addr
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// stripComments blanks the lines that start with // (after white space). A JSON string cannot hold
// a raw line break, so a value never loses text to this.
func stripComments(b []byte) []byte {
	nl := []byte{'\n'}
	lines := bytes.Split(b, nl)
	for i, l := range lines {
		if bytes.HasPrefix(bytes.TrimSpace(l), []byte("//")) {
			lines[i] = nil
		}
	}
	return bytes.Join(lines, nl)
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConfig, fmt.Sprintf(format, a...))
}

// Validate checks a configuration.
func (c *Config) Validate() error {
	if c.Version != 1 {
		return bad("version %d, want 1", c.Version)
	}
	if len(c.Listen) == 0 || len(c.Listen) > maxListeners {
		return bad("%d listeners, want 1 to %d", len(c.Listen), maxListeners)
	}
	binds := map[string]bool{}
	check := func(what string, l ListenSpec) error {
		if err := l.validate(); err != nil {
			return bad("%s: %v", what, err)
		}
		// UDP and TCP may share a port number (reality2-quic and reality2-tcp on 443); within one
		// transport the bind must be unique.
		key := "tcp|" + l.Bind
		if udpSuite(l.Suite) {
			key = "udp|" + l.Bind
		}
		if binds[key] {
			return bad("%s: bind %s is used twice on the same transport", what, l.Bind)
		}
		binds[key] = true
		return nil
	}
	for i, l := range c.Listen {
		if err := check(fmt.Sprintf("listener %d", i+1), l); err != nil {
			return err
		}
	}
	if c.InviteListen != nil {
		if err := check("invite listener", *c.InviteListen); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, r := range c.Roles {
		if !allRoles[r] {
			return bad("role %q: want relay, exit, mailbox or dht-server", r)
		}
		if seen[r] {
			return bad("role %q twice", r)
		}
		seen[r] = true
	}
	if len(c.Bootstrap) > bootstrap.MaxEntries {
		return bad("%d bootstrap entries, want at most %d", len(c.Bootstrap), bootstrap.MaxEntries)
	}
	for i, line := range c.Bootstrap {
		if strings.TrimSpace(line) == "" || len(line) > bootstrap.MaxLineBytes {
			return bad("bootstrap entry %d: an empty or oversized line", i+1)
		}
	}
	if c.Cover != "" {
		host, port, err := net.SplitHostPort(c.Cover)
		if p, perr := strconv.ParseUint(port, 10, 16); err != nil || host == "" || len(host) > 253 || perr != nil || p == 0 {
			return bad("cover %q: want host:port", c.Cover)
		}
	}
	for what, v := range map[string]string{"site_dir": c.SiteDir, "cover_cert": c.CoverCert} {
		if len(v) > 4096 || strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
			return bad("%s: a path of at most 4096 bytes without NUL", what)
		}
	}
	if c.CoverACME != nil {
		switch c.CoverACME.Challenge {
		case "", "http-01", "tls-alpn-01":
		default:
			return bad("cover_acme.challenge %q: want http-01 or tls-alpn-01", c.CoverACME.Challenge)
		}
		if d := c.CoverACME.DirectoryURL; d != "" {
			u, err := url.Parse(d)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return bad("cover_acme.directory %q: want an http(s) URL", d)
			}
		}
		if len(c.CoverACME.Email) > 254 || strings.ContainsAny(c.CoverACME.Email, " \t\r\n") {
			return bad("cover_acme.email: at most 254 characters without spaces")
		}
	}
	// An empty cover_cert means ACME (T2.6e, T12.5). Accepting the CA's terms is the operator's
	// act, never the code's: without it the operator must provide a certificate. ws2 alone needs
	// none: its inner TLS lives inside the WebSocket and the CDN holds the outer certificate.
	if c.hasReality2() && c.CoverCert == "" && (c.CoverACME == nil || !c.CoverACME.AcceptTOS) {
		return bad("cover %q: cover_cert is empty and the CA terms are not accepted; set cover_acme.accept_tos or provide cover_cert", c.Cover)
	}
	// НС-3 (T2.6e): only the node's own cover is served. A camouflaged listener without a cover,
	// and a cover without the operator's site folder, would either refuse strangers or serve a
	// site we did not verify (T2.6b-3): both are configuration errors, not runtime surprises.
	if c.hasCamouflaged() && c.Cover == "" {
		return bad("a camouflaged listener needs cover: the node serves the operator's own site")
	}
	if c.Cover != "" && c.SiteDir == "" {
		return bad("cover %q: site_dir is required (T2.6e): a foreign cover must not be served before T2.6b-3", c.Cover)
	}
	ap, err := netip.ParseAddrPort(c.Operator.Addr)
	if err != nil || !ap.Addr().IsLoopback() || ap.Addr().Is4In6() {
		return bad("operator address %q: want a loopback IP literal with a port", c.Operator.Addr)
	}
	return nil
}

func (l ListenSpec) validate() error {
	if !allSuites[l.Suite] {
		return fmt.Errorf("suite %q: want noise3, reality2-quic, reality2-tcp or ws2", l.Suite)
	}
	host, port, err := net.SplitHostPort(l.Bind)
	if err != nil {
		return fmt.Errorf("bind %q: want host:port", l.Bind)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return fmt.Errorf("bind host %q: want an IP literal", host)
	}
	if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
		return fmt.Errorf("bind port %q", port)
	}
	adv, err := netip.ParseAddr(l.Advertise)
	if err != nil || adv.Unmap().IsUnspecified() || adv.IsMulticast() || adv.Unmap() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return fmt.Errorf("advertise %q: want a unicast IP literal", l.Advertise)
	}
	return nil
}

// ParseDuration reads a term such as "30d", "12h", "90m" or "1d12h". It must be positive and at
// most ten years.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("empty term")
	}
	var total time.Duration
	rest := s
	for _, unit := range []struct {
		suffix byte
		d      time.Duration
	}{{'d', 24 * time.Hour}, {'h', time.Hour}, {'m', time.Minute}} {
		i := strings.IndexByte(rest, unit.suffix)
		if i < 0 {
			continue
		}
		num := rest[:i]
		if num == "" || len(num) > 5 || strings.Trim(num, "0123456789") != "" {
			return 0, fmt.Errorf("term %q: want like 30d, 12h or 1d12h", s)
		}
		n, _ := strconv.Atoi(num)
		total += time.Duration(n) * unit.d
		rest = rest[i+1:]
	}
	if rest != "" || total <= 0 || total > 10*365*24*time.Hour {
		return 0, fmt.Errorf("term %q: want a positive term of at most ten years, like 30d", s)
	}
	return total, nil
}
