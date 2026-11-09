package servernode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// Preset is a ready choice of roles for the setup wizard.
type Preset string

const (
	PresetRelay       Preset = "relay"
	PresetExitFree    Preset = "exit-free"
	PresetHomeEntry   Preset = "home-entry"
	PresetExitPaid    Preset = "exit-paid"
	PresetExitPrivate Preset = "exit-private"
	// PresetOwn is the operator's own set of roles (checkboxes in the setup TUI).
	PresetOwn Preset = "own"
)

// ErrPresetUnavailable: the preset needs something this build does not have yet (payments for the
// paid exit, the hidden mode for the private one).
var ErrPresetUnavailable = errors.New("servernode: preset is not available in this build")

// PresetOptions are the answers a preset needs.
type PresetOptions struct {
	Advertise string
	Port      int
	Domain    string
	SiteDir   string
	// Rules is the signed country rules file, when the operator has one: it tells whether the
	// advertised address of a home entry is inside the verified ranges (allowed_cidrs, H14).
	Rules *routepolicy.Rules
	// Carriers is the chosen set (noise3, reality2-quic, reality2-tcp, ws2). Empty means the
	// preset's own default: reality2-quic and reality2-tcp on the public port when a cover domain
	// is given, noise3 alone otherwise (T2.6e).
	Carriers []string
	// Invites adds the separate invitation listener. Home-entry and private-exit always have it.
	Invites bool
	// Roles overrides the roles of the preset (the "own" preset takes them from here).
	Roles []string
	// Limits caps bandwidth and sessions; nil means no cap.
	Limits *LimitsSpec
	// Visibility is "visible" (default) or "hidden".
	Visibility string
}

// Note is a remark for the operator: a message code with a level ("info" or "warn").
type Note struct {
	Level string
	Code  string
}

var domainLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validDomain accepts a lower-case DNS name of at least two labels that is not an IP literal.
func validDomain(d string) bool {
	if d == "" || len(d) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(d); err == nil {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !domainLabel.MatchString(l) {
			return false
		}
	}
	return true
}

var freeDDNS = []string{
	"duckdns.org", "ddns.net", "no-ip.org", "no-ip.com", "no-ip.biz", "dyndns.org", "dyndns.com", "dynu.com", "hopto.org",
	"zapto.org", "servebeer.com", "freeddns.org", "afraid.org", "myftp.org", "myddns.me", "dnsalias.com", "ddnsking.com",
}

// IsFreeDDNS reports whether a domain belongs to a well-known free dynamic DNS service.
func IsFreeDDNS(domain string) bool {
	d := strings.ToLower(strings.TrimSuffix(domain, "."))
	for _, s := range freeDDNS {
		if d == s || strings.HasSuffix(d, "."+s) {
			return true
		}
	}
	return false
}

func badPreset(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConfig, fmt.Sprintf(format, a...))
}

// BuildPreset turns a preset and the answers into a configuration and the notes to show.
func BuildPreset(p Preset, o PresetOptions) (Config, []Note, error) {
	switch p {
	case PresetRelay, PresetExitFree, PresetHomeEntry, PresetExitPrivate, PresetOwn:
	default:
		return Config{}, nil, fmt.Errorf("%w: %q", ErrPresetUnavailable, p)
	}
	if o.Port < 1 || o.Port > 65535 {
		return Config{}, nil, badPreset("port %d", o.Port)
	}
	domain := strings.ToLower(strings.TrimSuffix(o.Domain, "."))
	if domain != "" && !validDomain(domain) {
		return Config{}, nil, badPreset("domain %q", o.Domain)
	}
	if len(o.SiteDir) > 4096 || strings.ContainsRune(o.SiteDir, 0) || !utf8.ValidString(o.SiteDir) {
		return Config{}, nil, badPreset("site folder")
	}
	cfg := DefaultConfig()
	cfg.Visibility = o.Visibility
	if cfg.Visibility == "" {
		cfg.Visibility = "visible"
	}
	cfg.Limits = o.Limits
	carriers, err := chooseCarriers(o.Carriers, domain != "")
	if err != nil {
		return Config{}, nil, err
	}
	invites := o.Invites || p == PresetHomeEntry || p == PresetExitPrivate
	if _, err := buildListeners(&cfg, carriers, domain != "", invites, o.Advertise, o.Port); err != nil {
		return Config{}, nil, err
	}
	if domain != "" {
		cfg.Cover = fmt.Sprintf("%s:%d", domain, o.Port)
		cfg.SiteDir = o.SiteDir
		// The operator chose a domain and no certificate: the node gets one from the CA. The
		// installer is the operator's act; a hand-written server.json says it with accept_tos.
		cfg.CoverACME = &CoverACMESpec{AcceptTOS: true}
	}
	var notes []Note
	switch p {
	case PresetExitFree:
		cfg.Roles = append(cfg.Roles, "exit")
		notes = append(notes, Note{"warn", "preset.exit-responsibility"})
	case PresetExitPrivate:
		cfg.Roles = append(cfg.Roles, "exit")
		cfg.Visibility = "hidden"
		notes = append(notes, Note{"warn", "preset.exit-responsibility"}, Note{"info", "preset.private-invites"})
	case PresetHomeEntry:
		notes = append(notes, Note{"warn", "preset.home-entry-risk"})
		if verifiedEntry(o.Rules, o.Advertise) {
			notes = append(notes, Note{"info", "preset.home-entry-verified"})
		} else {
			notes = append(notes, Note{"info", "preset.home-entry-wired"})
		}
	case PresetOwn:
		if len(o.Roles) == 0 {
			return Config{}, nil, badPreset("the own preset needs at least one role")
		}
		cfg.Roles = append([]string(nil), o.Roles...)
	}
	if domain == "" {
		notes = append(notes, Note{"warn", "preset.no-domain"})
	} else {
		if IsFreeDDNS(domain) {
			notes = append(notes, Note{"warn", "preset.ddns"})
		}
		if o.SiteDir == "" {
			notes = append(notes, Note{"warn", "preset.empty-site"})
		}
		notes = append(notes, Note{"info", "preset.acme"})
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, nil, err
	}
	return cfg, notes, nil
}

// chooseCarriers validates the chosen carriers and returns the preset default when none were
// given: a covered node listens on reality2-quic and reality2-tcp (the T2.6e preset); a node
// without a cover has only noise3, and ws2 (a CDN-facing listener) needs a cover domain.
func chooseCarriers(chosen []string, hasCover bool) ([]string, error) {
	if len(chosen) == 0 {
		if hasCover {
			return []string{"reality2-quic", "reality2-tcp"}, nil
		}
		return []string{"noise3"}, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range chosen {
		if !allSuites[c] {
			return nil, badPreset("carrier %q: want noise3, reality2-quic, reality2-tcp or ws2", c)
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		if c == "ws2" && !hasCover {
			return nil, badPreset("ws2 needs a cover domain")
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, badPreset("no carriers chosen")
	}
	return out, nil
}

// buildListeners lays the chosen carriers out on the port: reality2-quic and reality2-tcp share
// the public port (UDP and TCP), ws2 takes the next TCP port, noise3 the next UDP one, and the
// invitation listener one more. Ports of different transports may coincide.
func buildListeners(cfg *Config, carriers []string, hasCover bool, invites bool, advertise string, port int) (int, error) {
	next := port
	take := func() (int, error) {
		if next > 65535 {
			return 0, badPreset("port %d leaves no free port", port)
		}
		p := next
		next++
		return p, nil
	}
	var listens []ListenSpec
	hasReality := false
	udpUsed := map[int]bool{}
	tcpUsed := map[int]bool{}
	for _, c := range carriers {
		if c == "reality2-quic" || c == "reality2-tcp" {
			hasReality = true
		}
	}
	if hasReality {
		// The public port is taken by the reality2 pair on both transports.
		udpUsed[port], tcpUsed[port] = true, true
		next = port + 1
	}
	for _, c := range carriers {
		switch c {
		case "reality2-quic", "reality2-tcp":
			listens = append(listens, ListenSpec{Suite: c, Bind: fmt.Sprintf("0.0.0.0:%d", port), Advertise: advertise})
		case "ws2":
			p, err := take()
			if err != nil {
				return 0, err
			}
			for tcpUsed[p] {
				if p, err = take(); err != nil {
					return 0, err
				}
			}
			tcpUsed[p] = true
			listens = append(listens, ListenSpec{Suite: "ws2", Bind: fmt.Sprintf("0.0.0.0:%d", p), Advertise: advertise})
		case "noise3":
			p, err := take()
			if err != nil {
				return 0, err
			}
			for udpUsed[p] {
				if p, err = take(); err != nil {
					return 0, err
				}
			}
			udpUsed[p] = true
			listens = append(listens, ListenSpec{Suite: "noise3", Bind: fmt.Sprintf("0.0.0.0:%d", p), Advertise: advertise})
		}
	}
	if hasReality && !contains(carriers, "noise3") {
		// A spare noise3 port keeps the node reachable when a probe blocks TLS (T2.6e).
		p, err := take()
		if err != nil {
			return 0, err
		}
		for udpUsed[p] {
			if p, err = take(); err != nil {
				return 0, err
			}
		}
		udpUsed[p] = true
		listens = append(listens, ListenSpec{Suite: "noise3", Bind: fmt.Sprintf("0.0.0.0:%d", p), Advertise: advertise})
	}
	if !hasCover && hasReality {
		return 0, badPreset("the camouflaged carriers need a cover domain")
	}
	cfg.Listen = listens
	if invites {
		p, err := take()
		if err != nil {
			return 0, err
		}
		for udpUsed[p] {
			if p, err = take(); err != nil {
				return 0, err
			}
		}
		udpUsed[p] = true
		cfg.InviteListen = &ListenSpec{Suite: "noise3", Bind: fmt.Sprintf("0.0.0.0:%d", p), Advertise: advertise}
	}
	return next, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// verifiedEntry reports whether the advertised address lies inside the verified ranges
// (allowed_cidrs) of a rules file that has not expired. Only such an address is good for mobile
// networks that run on allow-lists (H14); any other home entry serves wired networks.
func verifiedEntry(rules *routepolicy.Rules, advertise string) bool {
	ip, err := netip.ParseAddr(advertise)
	return err == nil && rules.HasVerifiedEntries() && rules.EntryAllowed("RU", routepolicy.NetMobile, ip)
}

// RenderConfig is server.json with comments, ready to be edited by hand. Only whole lines that
// start with // are comments.
func RenderConfig(cfg Config, lang string) ([]byte, error) {
	if cfg.Roles == nil {
		cfg.Roles = []string{}
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, code := range []string{"config.header", "config.listen", "config.roles", "config.cover", "config.operator"} {
		b.WriteString("// " + Message(lang, code) + "\n")
	}
	b.Write(body)
	b.WriteByte('\n')
	return b.Bytes(), nil
}
