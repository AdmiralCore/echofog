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
	case PresetRelay, PresetExitFree, PresetHomeEntry:
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
	if domain == "" {
		// No cover: the node works for its own people only, over noise3 on the chosen port.
		cfg.Listen = []ListenSpec{{Suite: "noise3", Bind: fmt.Sprintf("0.0.0.0:%d", o.Port), Advertise: o.Advertise}}
	} else {
		// The own cover preset (T2.6e): reality2-quic and reality2-tcp on the public port, so a
		// stranger and a probe see the operator's site on 443; noise3 moves to the spare port.
		if o.Port+1 > 65535 {
			return Config{}, nil, badPreset("port %d leaves no spare port for noise3", o.Port)
		}
		cfg.Listen = []ListenSpec{
			{Suite: "reality2-quic", Bind: fmt.Sprintf("0.0.0.0:%d", o.Port), Advertise: o.Advertise},
			{Suite: "reality2-tcp", Bind: fmt.Sprintf("0.0.0.0:%d", o.Port), Advertise: o.Advertise},
			{Suite: "noise3", Bind: fmt.Sprintf("0.0.0.0:%d", o.Port+1), Advertise: o.Advertise},
		}
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
	case PresetHomeEntry:
		invitePort := o.Port + 1
		if domain != "" {
			invitePort = o.Port + 2
		}
		if invitePort > 65535 {
			return Config{}, nil, badPreset("port %d leaves no port for invitations", o.Port)
		}
		cfg.InviteListen = &ListenSpec{Suite: "noise3", Bind: fmt.Sprintf("0.0.0.0:%d", invitePort), Advertise: o.Advertise}
		notes = append(notes, Note{"warn", "preset.home-entry-risk"})
		if verifiedEntry(o.Rules, o.Advertise) {
			notes = append(notes, Note{"info", "preset.home-entry-verified"})
		} else {
			notes = append(notes, Note{"info", "preset.home-entry-wired"})
		}
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
