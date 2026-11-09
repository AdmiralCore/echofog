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

// Text is the line for the operator: the level, then the message with its advice. The setup
// checks carry full codes ("setup.check..."), the doctor ones a short name ("config.ok").
func (r CheckResult) Text(lang string) string {
	text := Message(lang, r.Code, r.Args...)
	if text == "" {
		text = Message(lang, "doctor."+r.Code, r.Args...)
	}
	return "[" + Message(lang, r.Level.code()) + "] " + text
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
	// ListenTCP is the same for a TCP port (reality2-tcp, ws2); nil uses the real network stack.
	ListenTCP func(addr string) error
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

func listenTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// udpSuite reports the suites that bind a UDP port.
func udpSuite(suite string) bool {
	switch suite {
	case "noise3", "reality2-quic":
		return true
	}
	return false
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
	if d.ListenTCP == nil {
		d.ListenTCP = listenTCP
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

	if loose := looseFiles(d.DataDir, cfg); len(loose) > 0 {
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
		report := func(suite string) {
			for _, l := range append(append([]ListenSpec(nil), cfg.Listen...), inviteListens(cfg)...) {
				if l.Suite != suite {
					continue
				}
				if st != nil {
					level, code, args = LevelOK, "port.node", []any{l.Bind}
					continue
				}
				var err error
				if udpSuite(suite) {
					err = d.ListenUDP(l.Bind)
				} else {
					err = d.ListenTCP(l.Bind)
				}
				if err != nil {
					level, code, args = LevelFail, "port.busy", []any{l.Bind, shortErr(err)}
				} else if level != LevelFail {
					args = []any{l.Bind}
				}
			}
		}
		report("noise3")
		report("reality2-quic")
		report("reality2-tcp")
		report("ws2")
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
		out = append(out, certCheck(d, cfg, st))
		switch {
		case cfg.hasCamouflaged() && cfg.Cover == "":
			add("cover", LevelFail, "cover.missing")
		case cfg.Cover != "" && cfg.SiteDir == "":
			add("cover", LevelFail, "cover.site-missing", cfg.Cover)
		case cfg.Cover == "":
			add("cover", LevelInfo, "cover.none")
		default:
			if _, err := os.Stat(filepath.Join(cfg.SiteDir, "index.html")); err != nil {
				add("cover", LevelWarn, "cover.empty", cfg.SiteDir)
			} else {
				add("cover", LevelOK, "cover.ok", cfg.Cover, cfg.SiteDir)
			}
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

// inviteListens returns the invitation listener, if any, as a slice for the port checks.
func inviteListens(cfg *Config) []ListenSpec {
	if cfg.InviteListen == nil {
		return nil
	}
	return []ListenSpec{*cfg.InviteListen}
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

func looseFiles(dir string, cfg *Config) []string {
	if os.PathSeparator != '/' {
		return nil // file modes mean nothing on Windows
	}
	var out []string
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		out = append(out, filepath.Base(dir))
	}
	files := []string{filepath.Join(dir, identityFileName), filepath.Join(dir, configFileName), filepath.Join(dir, tokenFileName)}
	if cfg != nil && cfg.CoverCert != "" {
		files = append(files, cfg.CoverCert) // the bundle holds the private key of the cover
	}
	for _, p := range files {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
			out = append(out, filepath.Base(p))
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

func certCheck(d DoctorDeps, cfg *Config, st *StatusInfo) CheckResult {
	res := func(l Level, code string, args ...any) CheckResult {
		return CheckResult{ID: "cert", Level: l, Code: code, Args: args}
	}
	if cfg.CoverCert == "" {
		// With ACME the certificate lives in the profile; the running node reports its expiry.
		if st != nil && st.CoverCertNotAfter > 0 {
			return certTerm(res, d.Now(), time.Time{}, time.Unix(st.CoverCertNotAfter, 0))
		}
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
	return certTerm(res, d.Now(), cert.NotBefore, cert.NotAfter)
}

// certTerm turns an expiry into the doctor's term line. A zero notBefore means unknown.
func certTerm(res func(Level, string, ...any) CheckResult, now, notBefore, notAfter time.Time) CheckResult {
	if (!notBefore.IsZero() && now.Before(notBefore)) || now.After(notAfter) {
		return res(LevelFail, "cert.expired")
	}
	days := int(notAfter.Sub(now).Hours() / 24)
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
