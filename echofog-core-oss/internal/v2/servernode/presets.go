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
	cfg.Listen = []ListenSpec{{Suite: "noise3", Bind: fmt.Sprintf("0.0.0.0:%d", o.Port), Advertise: o.Advertise}}
	if domain != "" {
		cfg.Cover = domain + ":443"
		cfg.SiteDir = o.SiteDir
	}
	var notes []Note
	switch p {
	case PresetExitFree:
		cfg.Roles = append(cfg.Roles, "exit")
		notes = append(notes, Note{"warn", "preset.exit-responsibility"})
	case PresetHomeEntry:
		if o.Port+1 > 65535 {
			return Config{}, nil, badPreset("port %d leaves no port for invitations", o.Port)
		}
		cfg.InviteListen = &ListenSpec{Suite: "noise3", Bind: fmt.Sprintf("0.0.0.0:%d", o.Port+1), Advertise: o.Advertise}
		notes = append(notes, Note{"info", "preset.home-entry-wired"})
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
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, nil, err
	}
	return cfg, notes, nil
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
