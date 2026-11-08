package servernode_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

var docNow = time.Unix(1_800_000_000, 0)

func writeCert(t *testing.T, dir string, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.org"}, DNSNames: []string{"example.org"},
		NotBefore: notBefore, NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "cover.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type docRig struct {
	deps servernode.DoctorDeps
	dir  string
}

// newDocRig is a healthy node: a real profile, a good certificate, DNS that points here, a free
// port, a running node with peers and plenty of disk.
func newDocRig(t *testing.T) *docRig {
	t.Helper()
	dir, cfg := profile(t, false)
	cfg.Cover = "example.org:443"
	cfg.SiteDir = dir
	cfg.CoverCert = writeCert(t, t.TempDir(), docNow.Add(-24*time.Hour), docNow.Add(90*24*time.Hour))
	cfg.Listen[0].Advertise = "203.0.113.10"
	return &docRig{dir: dir, deps: servernode.DoctorDeps{
		DataDir: dir, Config: &cfg, Now: func() time.Time { return docNow }, Version: "0.3.0",
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil
		},
		FreeBytes: func(string) (uint64, error) { return 50 << 30, nil },
		ListenUDP: func(string) error { return nil },
		NodeStatus: func(context.Context) (*servernode.StatusInfo, error) {
			return &servernode.StatusInfo{Sessions: 4, PublicCard: true}, nil
		},
	}}
}

func (r *docRig) levels() map[string]servernode.Level {
	out := map[string]servernode.Level{}
	for _, res := range servernode.RunDoctor(context.Background(), r.deps) {
		out[res.ID] = res.Level
	}
	return out
}

func TestDoctorHealthyNode(t *testing.T) {
	r := newDocRig(t)
	res := servernode.RunDoctor(context.Background(), r.deps)
	if len(res) < 8 {
		t.Fatalf("%d checks", len(res))
	}
	for _, x := range res {
		if x.Level == servernode.LevelFail || x.Level == servernode.LevelWarn {
			t.Errorf("%s: %s", x.ID, x.Text("en"))
		}
		for _, lang := range []string{"ru", "en"} {
			if x.Text(lang) == "" {
				t.Errorf("%s has no %s text", x.ID, lang)
			}
		}
	}
	if servernode.DoctorExit(res) != 0 {
		t.Error("a healthy node gives a nonzero exit code")
	}
}

// The task: the doctor catches deliberately broken DNS, port and certificate.
func TestDoctorCatchesBrokenThings(t *testing.T) {
	cases := map[string]struct {
		break_ func(*docRig, *testing.T)
		id     string
		want   servernode.Level
	}{
		"DNS points elsewhere": {func(r *docRig, _ *testing.T) {
			r.deps.Resolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("198.51.100.99")}, nil
			}
		}, "dns", servernode.LevelFail},
		"DNS has no record": {func(r *docRig, _ *testing.T) {
			r.deps.Resolve = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
		}, "dns", servernode.LevelFail},
		"DNS has the address among others": {func(r *docRig, _ *testing.T) {
			r.deps.Resolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("198.51.100.99"), netip.MustParseAddr("203.0.113.10")}, nil
			}
		}, "dns", servernode.LevelWarn},
		"port is taken": {func(r *docRig, _ *testing.T) {
			r.deps.NodeStatus = func(context.Context) (*servernode.StatusInfo, error) { return nil, errors.New("not running") }
			r.deps.ListenUDP = func(string) error { return errors.New("address already in use") }
		}, "port", servernode.LevelFail},
		"no peers yet": {func(r *docRig, _ *testing.T) {
			r.deps.NodeStatus = func(context.Context) (*servernode.StatusInfo, error) { return &servernode.StatusInfo{Sessions: 0}, nil }
		}, "reach", servernode.LevelWarn},
		"certificate expired": {func(r *docRig, t *testing.T) {
			r.deps.Config.CoverCert = writeCert(t, t.TempDir(), docNow.Add(-90*24*time.Hour), docNow.Add(-time.Hour))
		}, "cert", servernode.LevelFail},
		"certificate expires soon": {func(r *docRig, t *testing.T) {
			r.deps.Config.CoverCert = writeCert(t, t.TempDir(), docNow.Add(-60*24*time.Hour), docNow.Add(5*24*time.Hour))
		}, "cert", servernode.LevelWarn},
		"certificate is not a certificate": {func(r *docRig, t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.pem")
			_ = os.WriteFile(p, []byte("not pem"), 0o600)
			r.deps.Config.CoverCert = p
		}, "cert", servernode.LevelFail},
		"certificate file is missing": {func(r *docRig, t *testing.T) {
			r.deps.Config.CoverCert = filepath.Join(t.TempDir(), "gone.pem")
		}, "cert", servernode.LevelFail},
		"disk is almost full": {func(r *docRig, _ *testing.T) {
			r.deps.FreeBytes = func(string) (uint64, error) { return 100 << 20, nil }
		}, "disk", servernode.LevelFail},
		"disk is getting low": {func(r *docRig, _ *testing.T) {
			r.deps.FreeBytes = func(string) (uint64, error) { return 600 << 20, nil }
		}, "disk", servernode.LevelWarn},
		"the clock is off": {func(r *docRig, _ *testing.T) {
			r.deps.NodeStatus = func(context.Context) (*servernode.StatusInfo, error) {
				return &servernode.StatusInfo{Sessions: 2, Warnings: []string{"clock-warning: the clock differs"}}, nil
			}
		}, "clock", servernode.LevelWarn},
		"identity file is world readable": {func(r *docRig, t *testing.T) {
			if os.PathSeparator != '/' {
				t.Skip("file modes")
			}
			_ = os.Chmod(filepath.Join(r.dir, "server-identity.json"), 0o644)
		}, "files", servernode.LevelWarn},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newDocRig(t)
			c.break_(r, t)
			got := r.levels()[c.id]
			if got != c.want {
				t.Fatalf("%s: level %v, want %v", c.id, got, c.want)
			}
			if c.want == servernode.LevelFail && servernode.DoctorExit(servernode.RunDoctor(context.Background(), r.deps)) != 1 {
				t.Error("a failed check must give exit code 1")
			}
		})
	}
}

// Offline by default: no resolver, no probes, and the check says so instead of guessing.
func TestDoctorStaysOfflineWithoutProbes(t *testing.T) {
	r := newDocRig(t)
	r.deps.Resolve = nil
	for _, x := range servernode.RunDoctor(context.Background(), r.deps) {
		if x.ID == "dns" && x.Level != servernode.LevelInfo {
			t.Errorf("dns without a resolver: %v", x.Level)
		}
	}
	// A node that is not running is not an error: the doctor is also used before the first start.
	r.deps.NodeStatus = nil
	for _, x := range servernode.RunDoctor(context.Background(), r.deps) {
		if x.Level == servernode.LevelFail {
			t.Errorf("%s fails for a stopped node: %s", x.ID, x.Text("en"))
		}
	}
}

// The doctor never reveals a secret.
func TestDoctorRevealsNoSecrets(t *testing.T) {
	r := newDocRig(t)
	tok, _ := os.ReadFile(filepath.Join(r.dir, "operator.token"))
	idf, _ := os.ReadFile(filepath.Join(r.dir, "server-identity.json"))
	var words []string
	for _, line := range strings.Split(string(idf), "\n") {
		if strings.Contains(line, `"mnemonic"`) {
			words = strings.Fields(strings.Trim(strings.SplitN(line, ":", 2)[1], ` ",`))
		}
	}
	if len(words) != 12 {
		t.Fatalf("test setup: %d words", len(words))
	}
	r.deps.Config.CoverCert = filepath.Join(r.dir, "server-identity.json") // a file that is not a certificate
	var all strings.Builder
	for _, x := range servernode.RunDoctor(context.Background(), r.deps) {
		for _, lang := range []string{"ru", "en"} {
			all.WriteString(x.Text(lang) + "\n")
		}
	}
	text := all.String()
	if strings.Contains(text, strings.TrimSpace(string(tok))) {
		t.Error("the operator token is in the doctor output")
	}
	for _, w := range words[:6] {
		if strings.Contains(text, w+" ") && len(w) > 5 {
			t.Errorf("a word of the recovery phrase (%q) is in the doctor output", w)
		}
	}
	for _, k := range []string{"device_seed", "hs_private", "mask_private"} {
		if strings.Contains(text, k) {
			t.Errorf("%s in the doctor output", k)
		}
	}
}

func TestCLIDoctorAndStatus(t *testing.T) {
	dir, _ := profile(t, true)
	startServer(t, dir)

	code, out, errs := runCLI(nil, "doctor", "--data", dir)
	if code != 0 && code != 1 {
		t.Fatalf("doctor: %d %q", code, errs)
	}
	if !strings.Contains(out, "OK") && !strings.Contains(out, "ok") {
		t.Fatalf("doctor output:\n%s", out)
	}
	for _, bad := range []string{"device_seed", "hs_private"} {
		if strings.Contains(out, bad) {
			t.Errorf("doctor printed %s", bad)
		}
	}
	// Russian on request.
	if _, ruOut, _ := runCLI(nil, "doctor", "--data", dir, "--lang", "ru"); !hasCyrillic(ruOut) {
		t.Errorf("doctor --lang ru:\n%s", ruOut)
	}

	// status --json is one JSON object and carries no token.
	code, out, errs = runCLI(nil, "status", "--data", dir, "--json")
	if code != 0 {
		t.Fatalf("status --json: %d %s", code, errs)
	}
	var st servernode.StatusInfo
	if err := jsonUnmarshal(out, &st); err != nil || st.NodeID == "" || len(st.Roles) == 0 {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	tok, _ := os.ReadFile(filepath.Join(dir, "operator.token"))
	if strings.Contains(out, strings.TrimSpace(string(tok))) {
		t.Error("the token is in the status")
	}
	// The screen shows the version, the uptime and the invitations.
	code, out, _ = runCLI(nil, "status", "--data", dir)
	if code != 0 || !strings.Contains(out, "version") {
		t.Fatalf("status screen: %d\n%s", code, out)
	}
	if _, o, _ := runCLI(nil, "invite", "create", "--data", dir, "--access", "30d"); !strings.Contains(o, "echofog://") {
		t.Fatalf("invite: %q", o)
	}
	_, out, _ = runCLI(nil, "status", "--data", dir, "--json")
	_ = jsonUnmarshal(out, &st)
	if st.Invites != 1 {
		t.Errorf("status reports %d invitations, want 1", st.Invites)
	}
}
