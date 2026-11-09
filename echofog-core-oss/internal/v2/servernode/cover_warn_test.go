package servernode_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/servernode"
)

// TestEmptyCoverSiteWarnsTheOperator: an existing but empty site folder is the first thing a
// prober's second request notices, so the listener's warning must reach the operator status
// instead of being dropped (T2.6e finding 6).
func TestEmptyCoverSiteWarnsTheOperator(t *testing.T) {
	port := freePort(t)
	cfg := servernode.DefaultConfig()
	cfg.Roles = []string{"relay"}
	cfg.Listen = []servernode.ListenSpec{{Suite: "reality2-tcp", Bind: "127.0.0.1:" + strconv.Itoa(port), Advertise: "127.0.0.1"}}
	cfg.Cover = "example.org:" + strconv.Itoa(port)
	cfg.SiteDir = t.TempDir() // exists, has no pages
	cfg.CoverCert = writeCertKey(t, "example.org", t.TempDir())
	dir := filepath.Join(t.TempDir(), "node")
	if _, err := servernode.Init(dir, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv, _ := startServer(t, dir)
	st := srv.Status()
	warned := false
	for _, w := range st.Warnings {
		warned = warned || strings.Contains(w, "cover")
	}
	if !warned {
		t.Fatalf("an empty cover site produced no operator warning: %v", st.Warnings)
	}
}
