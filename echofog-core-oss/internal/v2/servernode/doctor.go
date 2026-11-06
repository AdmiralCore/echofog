package servernode

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Level of a doctor result.
type Level int

const (
	LevelOK Level = iota
	LevelInfo
	LevelWarn
	LevelFail
)

func (l Level) code() string {
	switch l {
	case LevelOK:
		return "doctor.level.ok"
	case LevelInfo:
		return "doctor.level.info"
	case LevelWarn:
		return "doctor.level.warn"
	}
	return "doctor.level.fail"
}

// CheckResult is one line of the doctor. Code is the message code without the "doctor." prefix.
type CheckResult struct {
	ID    string
	Level Level
	Code  string
	Args  []any
}

// Text is the line for the operator: the level, then the message with its advice.
func (r CheckResult) Text(lang string) string {
	return "[" + Message(lang, r.Level.code()) + "] " + Message(lang, "doctor."+r.Code, r.Args...)
}

// DoctorDeps are the doctor's inputs. A nil probe means the check is skipped: nothing leaves the
// machine unless the operator asked for it.
type DoctorDeps struct {
	DataDir string
	Config  *Config
	Now     func() time.Time
	// Resolve looks a name up. Nil keeps the doctor offline (the DNS check is skipped).
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// FreeBytes reports the free space of the file system holding path; nil skips the check.
	FreeBytes func(path string) (uint64, error)
	// ListenUDP tries to bind addr and releases it at once; nil uses the real network stack.
	ListenUDP func(addr string) error
	// NodeStatus asks the running node; nil or an error means the node is not running.
	NodeStatus func(ctx context.Context) (*StatusInfo, error)
	Version    string
}

func listenUDP(addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	return pc.Close()
}

// Disk thresholds of the doctor.
const (
	diskFailBelow = 200 << 20
	diskWarnBelow = 1 << 30
	certWarnDays  = 21
)

// RunDoctor runs the checks. It reads local files and, through the injected probes, may ask the
// running node on loopback; it never prints a key, a token or a phrase.
func RunDoctor(ctx context.Context, d DoctorDeps) []CheckResult {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.ListenUDP == nil {
		d.ListenUDP = listenUDP
	}
	var out []CheckResult
	add := func(id string, l Level, code string, args ...any) {
		out = append(out, CheckResult{ID: id, Level: l, Code: code, Args: args})
	}

	cfg := d.Config
	if cfg == nil {
		raw, err := readLimited(filepath.Join(d.DataDir, configFileName), MaxConfigSize)
		if err == nil {
			cfg, err = ParseConfig(raw)
		}
		if err != nil {
			add("config", LevelFail, "config.bad", shortErr(err))
			cfg = nil
		}
	} else if err := cfg.Validate(); err != nil {
		add("config", LevelFail, "config.bad", shortErr(err))
	}
	if cfg != nil && (len(out) == 0 || out[0].ID != "config") {
		add("config", LevelOK, "config.ok")
	}

	if loose := looseFiles(d.DataDir); len(loose) > 0 {
		add("files", LevelWarn, "files.loose", strings.Join(loose, ", "))
	} else {
		add("files", LevelOK, "files.ok")
	}

	var st *StatusInfo
	if d.NodeStatus != nil {
		if s, err := d.NodeStatus(ctx); err == nil && s != nil {
			st = s
		}
	}

	if cfg != nil {
		level, code := LevelOK, "port.ok"
		var args []any
		for _, l := range cfg.Listen {
			switch {
			case st != nil:
				level, code, args = LevelOK, "port.node", []any{l.Bind}
			default:
				if err := d.ListenUDP(l.Bind); err != nil {
					level, code, args = LevelFail, "port.busy", []any{l.Bind, shortErr(err)}
				} else if level != LevelFail {
					args = []any{l.Bind}
				}
			}
		}
		if len(cfg.Listen) > 0 {
			add("port", level, code, args...)
		}
	}

	switch {
	case st == nil:
		add("reach", LevelInfo, "reach.skipped")
	case st.Sessions > 0:
		add("reach", LevelOK, "reach.ok", st.Sessions)
	default:
		add("reach", LevelWarn, "reach.none")
	}

	if cfg != nil {
		out = append(out, dnsCheck(ctx, d, cfg))
		out = append(out, certCheck(d, cfg))
		if cfg.Cover == "" {
			add("cover", LevelInfo, "cover.none")
		}
	}

	switch {
	case st == nil:
		add("clock", LevelInfo, "clock.unknown")
	case hasWarning(st.Warnings, "clock-warning"):
		add("clock", LevelWarn, "clock.warn")
	default:
		add("clock", LevelOK, "clock.ok")
	}

	if d.FreeBytes == nil {
		add("disk", LevelInfo, "disk.unknown")
	} else if free, err := d.FreeBytes(d.DataDir); err != nil {
		add("disk", LevelInfo, "disk.unknown")
	} else {
		mb := int(free >> 20)
		switch {
		case free < diskFailBelow:
			add("disk", LevelFail, "disk.full", mb)
		case free < diskWarnBelow:
			add("disk", LevelWarn, "disk.low", mb)
		default:
			add("disk", LevelOK, "disk.ok", mb)
		}
	}

	add("version", LevelInfo, "version", d.Version)
	return out
}

func hasWarning(ws []string, prefix string) bool {
	for _, w := range ws {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}

// shortErr keeps an error to one short line without file contents.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func looseFiles(dir string) []string {
	if os.PathSeparator != '/' {
		return nil // file modes mean nothing on Windows
	}
	var out []string
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		out = append(out, filepath.Base(dir))
	}
	for _, name := range []string{identityFileName, configFileName, tokenFileName} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().Perm()&0o077 != 0 {
			out = append(out, name)
		}
	}
	return out
}

func dnsCheck(ctx context.Context, d DoctorDeps, cfg *Config) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "dns", Level: l, Code: code, Args: args}
	}
	if cfg.Cover == "" {
		return res(LevelInfo, "dns.nodomain")
	}
	if d.Resolve == nil {
		return res(LevelInfo, "dns.skipped")
	}
	host, _, _ := net.SplitHostPort(cfg.Cover)
	want, err := netip.ParseAddr(cfg.Listen[0].Advertise)
	if err != nil {
		return res(LevelFail, "dns.none", host, cfg.Listen[0].Advertise)
	}
	want = want.Unmap()
	addrs, err := d.Resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return res(LevelFail, "dns.none", host, want.String())
	}
	found := false
	for _, a := range addrs {
		found = found || a.Unmap() == want
	}
	switch {
	case found && len(addrs) == 1:
		return res(LevelOK, "dns.ok", host)
	case found:
		return res(LevelWarn, "dns.partial", host)
	}
	return res(LevelFail, "dns.mismatch", host, want.String())
}

func certCheck(d DoctorDeps, cfg *Config) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "cert", Level: l, Code: code, Args: args}
	}
	if cfg.CoverCert == "" {
		return res(LevelInfo, "cert.skipped")
	}
	f, err := os.Open(cfg.CoverCert)
	if err != nil {
		return res(LevelFail, "cert.unreadable", "cannot open the file")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return res(LevelFail, "cert.unreadable", "cannot read the file")
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return res(LevelFail, "cert.unreadable", "not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return res(LevelFail, "cert.unreadable", "not a valid certificate")
	}
	now := d.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return res(LevelFail, "cert.expired")
	}
	days := int(cert.NotAfter.Sub(now).Hours() / 24)
	if days < certWarnDays {
		return res(LevelWarn, "cert.soon", days)
	}
	return res(LevelOK, "cert.ok", days)
}

// DoctorExit is the exit code: 1 when any check failed.
func DoctorExit(rs []CheckResult) int {
	for _, r := range rs {
		if r.Level == LevelFail {
			return 1
		}
	}
	return 0
}
