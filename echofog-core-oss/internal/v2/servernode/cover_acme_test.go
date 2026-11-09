package servernode_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// coverSiteDir makes a folder with the operator's site.
func coverSiteDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("cover"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestACMEIssuesTheCoverCertificate: a cover with a domain and no cover_cert goes the ACME path
// and the running node reports the certificate's expiry, so the doctor can show its term
// (T2.6e item 2, T12.5). The CA is the in-process test one; no request leaves the machine.
func TestACMEIssuesTheCoverCertificate(t *testing.T) {
	ca := newFakeACME(t)
	port := freeTCPPort(t)
	ca.resolve = func(string) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }
	old := servernode.CoverACMEHTTPClient
	servernode.CoverACMEHTTPClient = &http.Client{Timeout: 20 * time.Second}
	t.Cleanup(func() { servernode.CoverACMEHTTPClient = old })

	cfg := servernode.DefaultConfig()
	cfg.Roles = []string{"relay"}
	cfg.Listen = []servernode.ListenSpec{{Suite: "reality2-tcp", Bind: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Advertise: "127.0.0.1"}}
	cfg.Cover = "cover.test:" + strconv.Itoa(port)
	cfg.SiteDir = coverSiteDir(t)
	cfg.CoverACME = &servernode.CoverACMESpec{DirectoryURL: ca.URL(), AcceptTOS: true, Email: "ops@example.org"}
	dir := filepath.Join(t.TempDir(), "node")
	if _, err := servernode.Init(dir, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv, _ := startServer(t, dir)
	st := srv.Status()
	if st.CoverCertNotAfter <= time.Now().Unix() {
		t.Fatalf("the node reports no certificate expiry after ACME: %+v", st)
	}

	res := servernode.RunDoctor(context.Background(), servernode.DoctorDeps{
		DataDir: dir, Config: &cfg, Now: time.Now, Version: "0.3.0",
		NodeStatus: func(context.Context) (*servernode.StatusInfo, error) { s := srv.Status(); return &s, nil },
		ListenTCP:  func(string) error { return nil },
	})
	level, code := servernode.LevelFail, ""
	for _, r := range res {
		if r.ID == "cert" {
			level, code = r.Level, r.Code
		}
	}
	if level != servernode.LevelOK || code != "cert.ok" {
		t.Fatalf("doctor cert check = %v %q, want OK: the ACME certificate's term is not shown", level, code)
	}
}

// TestCoverWithoutCertificateNeedsTheTerms: an operator who leaves cover_cert empty must accept
// the CA's terms (or provide a certificate); the refusal names both options and is an
// ErrConfig, never a panic at Start.
func TestCoverWithoutCertificateNeedsTheTerms(t *testing.T) {
	cfg := servernode.DefaultConfig()
	cfg.Roles = []string{"relay"}
	cfg.Listen = []servernode.ListenSpec{{Suite: "reality2-tcp", Bind: "127.0.0.1:443", Advertise: "203.0.113.10"}}
	cfg.Cover = "example.org:443"
	cfg.SiteDir = "/srv/site"
	if err := cfg.Validate(); err == nil {
		t.Fatal("a cover without cover_cert and without accepted terms validated")
	} else if !strings.Contains(err.Error(), "cover_cert") || !strings.Contains(err.Error(), "terms") {
		t.Fatalf("the refusal does not name cover_cert and the terms: %v", err)
	}

	raw := `{"version":1,"listen":[{"suite":"reality2-tcp","bind":"127.0.0.1:443","advertise":"203.0.113.10"}],
	  "cover":"example.org:443","site_dir":"/srv/site","roles":["relay"],"operator":{"addr":"127.0.0.1:0"}}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	res := servernode.RunDoctor(context.Background(), servernode.DoctorDeps{DataDir: dir, Now: time.Now, Version: "0.3.0"})
	for _, r := range res {
		if r.ID == "config" && r.Level != servernode.LevelFail {
			t.Fatalf("the doctor does not fail a cover without a certificate or accepted terms: %s", r.Text("en"))
		}
	}
}
