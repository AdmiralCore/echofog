package servernode_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// coverCertFor signs a server certificate for dns with a throwaway CA and returns the pool that
// trusts that CA.
func coverCertFor(t *testing.T, dns string, notBefore, notAfter time.Time) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test cover CA"},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dns},
		DNSNames:     []string{dns},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}, pool
}

// coverSelfSigned makes a certificate that signs itself, with the pool that trusts it: a chain
// check alone would pass, and only the self-signed rule catches it.
func coverSelfSigned(t *testing.T, dns string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: dns},
		DNSNames:     []string{dns},
		NotBefore:    now.Add(-24 * time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func coverHTML() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>cover</title>"))
	})
}

// startCover serves a TLS site; tune runs before the listener starts.
func startCover(t *testing.T, cert tls.Certificate, h http.Handler, tune func(*httptest.Server)) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if tune != nil {
		tune(srv)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func coverSite(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>cover</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mountInfoWith writes a mount table that has the given mount points, and returns its path.
func mountInfoWith(t *testing.T, points ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("22 1 0:21 / / rw,relatime - overlay overlay rw\n")
	for i, p := range points {
		fmt.Fprintf(&b, "%d 22 8:%d / %s rw,relatime - ext4 /dev/sd%d rw\n", 30+i, i+1, p, i+1)
	}
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func coverDeps(t *testing.T, srv *httptest.Server, roots *x509.CertPool, site string) servernode.CoverDeps {
	t.Helper()
	return servernode.CoverDeps{
		Cover:     "cover.test:443",
		Advertise: "203.0.113.10",
		SiteDir:   site,
		Now:       time.Now,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil
		},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		Roots:     roots,
		MountInfo: mountInfoWith(t, site),
	}
}

func coverLevels(t *testing.T, deps servernode.CoverDeps) (map[string]servernode.Level, []servernode.CheckResult) {
	t.Helper()
	res := servernode.RunCoverDoctor(context.Background(), deps)
	levels := map[string]servernode.Level{}
	for _, r := range res {
		levels[r.ID] = r.Level
	}
	return levels, res
}

// A cover that a tester would see from outside: DNS at this address, TCP open, TLS 1.3 with h2, a
// trusted certificate for the name, a 200 HTML page and index.html in a mounted folder.
func TestCoverDoctorPasses(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cert, pool := coverCertFor(t, "cover.test", now.Add(-time.Hour), now.Add(90*24*time.Hour))
	srv := startCover(t, cert, coverHTML(), nil)
	site := coverSite(t)

	levels, res := coverLevels(t, coverDeps(t, srv, pool, site))
	if len(res) < 6 {
		t.Fatalf("%d checks, want the DNS, port, TLS, site, files and mount lines", len(res))
	}
	for _, r := range res {
		if r.Level == servernode.LevelFail || r.Level == servernode.LevelWarn {
			t.Errorf("%s: %s", r.ID, r.Text("en"))
		}
		for _, lang := range []string{"ru", "en"} {
			if r.Text(lang) == "" {
				t.Errorf("%s has no %s text", r.ID, lang)
			}
		}
	}
	for _, id := range []string{"cover.dns", "cover.port", "cover.tls", "cover.site", "cover.files", "cover.mount"} {
		if levels[id] != servernode.LevelOK {
			t.Errorf("%s: %v", id, levels[id])
		}
	}
	if servernode.DoctorExit(res) != 0 {
		t.Error("a good cover gives a nonzero exit code")
	}
}

// The TLS rules: 1.3, h2, a chain to the roots, the name, not self-signed, more than 14 days.
func TestCoverDoctorCatchesTLSProblems(t *testing.T) {
	cases := map[string]struct {
		cert  func(*testing.T) (tls.Certificate, *x509.CertPool)
		tune  func(*httptest.Server)
		roots func(*x509.CertPool) *x509.CertPool
		want  servernode.Level
	}{
		"no h2 in ALPN": {
			cert: func(t *testing.T) (tls.Certificate, *x509.CertPool) {
				return coverCertFor(t, "cover.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
			},
			tune: func(s *httptest.Server) { s.TLS.NextProtos = []string{"http/1.1"} },
			want: servernode.LevelFail,
		},
		"TLS 1.2 only": {
			cert: func(t *testing.T) (tls.Certificate, *x509.CertPool) {
				return coverCertFor(t, "cover.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
			},
			tune: func(s *httptest.Server) { s.TLS.MaxVersion = tls.VersionTLS12 },
			want: servernode.LevelFail,
		},
		"certificate expired": {
			cert: func(t *testing.T) (tls.Certificate, *x509.CertPool) {
				return coverCertFor(t, "cover.test", time.Now().Add(-90*24*time.Hour), time.Now().Add(-time.Hour))
			},
			want: servernode.LevelFail,
		},
		"certificate is self-signed": {
			cert: func(t *testing.T) (tls.Certificate, *x509.CertPool) {
				return coverSelfSigned(t, "cover.test")
			},
			want: servernode.LevelFail,
		},
		"certificate is not trusted by the roots": {
			cert: func(t *testing.T) (tls.Certificate, *x509.CertPool) {
				return coverCertFor(t, "cover.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
			},
			roots: func(*x509.CertPool) *x509.CertPool { return x509.NewCertPool() },
			want:  servernode.LevelFail,
		},
		"certificate is for another name": {
			cert: func(t *testing.T) (tls.Certificate, *x509.CertPool) {
				return coverCertFor(t, "other.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
			},
			want: servernode.LevelFail,
		},
		"certificate expires soon": {
			cert: func(t *testing.T) (tls.Certificate, *x509.CertPool) {
				return coverCertFor(t, "cover.test", time.Now().Add(-60*24*time.Hour), time.Now().Add(5*24*time.Hour))
			},
			want: servernode.LevelWarn,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cert, pool := c.cert(t)
			if c.roots != nil {
				pool = c.roots(pool)
			}
			srv := startCover(t, cert, coverHTML(), c.tune)
			levels, res := coverLevels(t, coverDeps(t, srv, pool, coverSite(t)))
			if levels["cover.tls"] != c.want {
				t.Fatalf("cover.tls: %v, want %v (%d checks)", levels["cover.tls"], c.want, len(res))
			}
			if c.want == servernode.LevelFail && servernode.DoctorExit(res) != 1 {
				t.Error("a failed check must give exit code 1")
			}
		})
	}
}

// DNS: the name must resolve to the advertised address, and every record must be that address.
func TestCoverDoctorCatchesDNSProblems(t *testing.T) {
	cases := map[string]struct {
		resolve func(context.Context, string) ([]netip.Addr, error)
		adv     string
		want    servernode.Level
	}{
		"points elsewhere": {
			resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("198.51.100.99")}, nil
			},
			want: servernode.LevelFail,
		},
		"no record": {
			resolve: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") },
			want:    servernode.LevelFail,
		},
		"the address and others": {
			resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("198.51.100.99"), netip.MustParseAddr("203.0.113.10")}, nil
			},
			want: servernode.LevelWarn,
		},
		"no advertised address": {
			resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil
			},
			adv:  "",
			want: servernode.LevelFail,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cert, pool := coverCertFor(t, "cover.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
			srv := startCover(t, cert, coverHTML(), nil)
			deps := coverDeps(t, srv, pool, coverSite(t))
			deps.Resolve, deps.Advertise = c.resolve, c.adv
			if levels, _ := coverLevels(t, deps); levels["cover.dns"] != c.want {
				t.Fatalf("cover.dns: %v, want %v", levels["cover.dns"], c.want)
			}
		})
	}
}

// The port and the address: a closed port, and a cover that is not host:port.
func TestCoverDoctorCatchesPortProblems(t *testing.T) {
	t.Run("port is closed", func(t *testing.T) {
		t.Parallel()
		cert, pool := coverCertFor(t, "cover.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
		srv := startCover(t, cert, coverHTML(), nil)
		deps := coverDeps(t, srv, pool, coverSite(t))
		deps.Dial = func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("connect: connection refused")
		}
		levels, res := coverLevels(t, deps)
		if levels["cover.port"] != servernode.LevelFail {
			t.Fatalf("cover.port: %v, want fail", levels["cover.port"])
		}
		if servernode.DoctorExit(res) != 1 {
			t.Error("a closed port must give exit code 1")
		}
	})
	t.Run("cover is not host:port", func(t *testing.T) {
		t.Parallel()
		deps := servernode.CoverDeps{Cover: "cover.test", Advertise: "203.0.113.10", Now: time.Now}
		levels, res := coverLevels(t, deps)
		if levels["cover.dns"] != servernode.LevelFail || servernode.DoctorExit(res) != 1 {
			t.Fatalf("cover %q: %v", deps.Cover, levels["cover.dns"])
		}
	})
	t.Run("no cover", func(t *testing.T) {
		t.Parallel()
		levels, _ := coverLevels(t, servernode.CoverDeps{Now: time.Now})
		if levels["cover"] != servernode.LevelFail {
			t.Fatalf("no cover: %v", levels["cover"])
		}
	})
}

// The site: a 200 HTML page outside, index.html inside, a mounted folder.
func TestCoverDoctorCatchesSiteProblems(t *testing.T) {
	cases := map[string]struct {
		handler func() http.Handler
		site    func(*testing.T) string
		mount   func(*testing.T, string) string
		id      string
		want    servernode.Level
	}{
		"page is 404": {
			handler: func() http.Handler { return http.NotFoundHandler() },
			site:    coverSite,
			id:      "cover.site", want: servernode.LevelFail,
		},
		"page is not HTML": {
			handler: func() http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					_, _ = w.Write([]byte("hello"))
				})
			},
			site: coverSite,
			id:   "cover.site", want: servernode.LevelFail,
		},
		"index.html is missing": {
			handler: coverHTML,
			site:    func(t *testing.T) string { return t.TempDir() },
			id:      "cover.files", want: servernode.LevelFail,
		},
		"no site folder": {
			handler: coverHTML,
			site:    func(*testing.T) string { return "" },
			id:      "cover.files", want: servernode.LevelFail,
		},
		"folder is not mounted": {
			handler: coverHTML,
			site:    coverSite,
			mount: func(t *testing.T, _ string) string {
				return mountInfoWith(t, "/srv/other")
			},
			id: "cover.mount", want: servernode.LevelWarn,
		},
		"mount table is unreadable": {
			handler: coverHTML,
			site:    coverSite,
			mount: func(t *testing.T, _ string) string {
				return filepath.Join(t.TempDir(), "missing")
			},
			id: "cover.mount", want: servernode.LevelWarn,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cert, pool := coverCertFor(t, "cover.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
			srv := startCover(t, cert, c.handler(), nil)
			site := c.site(t)
			deps := coverDeps(t, srv, pool, site)
			if c.mount != nil {
				deps.MountInfo = c.mount(t, site)
			}
			levels, _ := coverLevels(t, deps)
			if levels[c.id] != c.want {
				t.Fatalf("%s: %v, want %v", c.id, levels[c.id], c.want)
			}
		})
	}
}

// The owner's main case: no profile yet, the cover is named on the command line.
func TestCLIDoctorCoverBeforeProfile(t *testing.T) {
	cert, _ := coverCertFor(t, "cover.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	srv := startCover(t, cert, coverHTML(), nil)
	site := coverSite(t)
	addr := srv.Listener.Addr().String()

	code, out, errs := runCLI(nil, "doctor", "--cover", addr, "--advertise", "127.0.0.1", "--site-dir", site)
	if code != 1 {
		t.Fatalf("a test certificate is not trusted by the system roots: exit %d, out %q err %q", code, out, errs)
	}
	if !strings.Contains(out, "certificate") {
		t.Errorf("the TLS line is missing:\n%s", out)
	}
	if out == "" || strings.Contains(out, "--cover needs") {
		t.Errorf("usage error instead of checks:\n%s", out)
	}
	// The port and DNS are fine here; only the certificate stops it.
	if !strings.Contains(out, "OK") {
		t.Errorf("no successful check:\n%s", out)
	}
}

// A profile whose server.json carries the cover: the bare --cover takes it from there.
func TestCLIDoctorBareCover(t *testing.T) {
	cert, _ := coverCertFor(t, "cover.test", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	srv := startCover(t, cert, coverHTML(), nil)
	site := coverSite(t)
	dir := filepath.Join(t.TempDir(), "node")
	cfg := servernode.DefaultConfig()
	cfg.Listen = []servernode.ListenSpec{{Suite: "noise3", Bind: fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)), Advertise: "127.0.0.1"}}
	cfg.Cover = srv.Listener.Addr().String()
	cfg.SiteDir = site
	if _, err := servernode.Init(dir, cfg); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(nil, "doctor", "--data", dir, "--cover")
	if code != 1 {
		t.Fatalf("exit %d, want 1 (the test certificate is untrusted):\n%s", code, out)
	}
	if strings.Contains(out, "No cover is set") {
		t.Errorf("the cover of the profile was not used:\n%s", out)
	}
	if !strings.Contains(out, "certificate") {
		t.Errorf("the checks did not run:\n%s", out)
	}
}
