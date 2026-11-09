package clock

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// helloRecorder keeps the ClientHello of the test server's TLS connections.
type helloRecorder struct {
	mu    sync.Mutex
	suite []uint16
}

func (r *helloRecorder) get(chi *tls.ClientHelloInfo) (*tls.Config, error) {
	r.mu.Lock()
	r.suite = append([]uint16(nil), chi.CipherSuites...)
	r.mu.Unlock()
	return nil, nil
}

// sawGREASE reports whether the last ClientHello offered a GREASE cipher suite, which the
// Go TLS stack never sends and the Chrome presets of uTLS always do.
func (r *helloRecorder) sawGREASE() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.suite {
		if id&0x0f0f == 0x0a0a {
			return true
		}
	}
	return false
}

// newClockServer starts a TLS server that answers every request with date in the Date header.
func newClockServer(t *testing.T, date time.Time) (*httptest.Server, *helloRecorder) {
	t.Helper()
	rec := &helloRecorder{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", date.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{GetConfigForClient: rec.get}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, rec
}

func serverEndpoints(srv *httptest.Server) []string {
	return []string{srv.URL + "/1", srv.URL + "/2", srv.URL + "/3"}
}

// trustServer makes client accept the test server's certificate the same way the production
// client reads its roots: from the transport's TLS configuration.
func trustServer(t *testing.T, client *http.Client, srv *httptest.Server) {
	t.Helper()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
}

// TestStartupConfigRunsTheCDNCheck: the configuration the products start with must carry the
// browser-shaped HTTP client, so MaybeCheckCDN reaches the endpoints instead of reporting
// ErrNoHTTPClient (D36 in the product, PA-FIX-4 ЧР-1).
func TestStartupConfigRunsTheCDNCheck(t *testing.T) {
	fc := newFakeClock(base)
	srv, rec := newClockServer(t, base.Add(2*time.Minute))

	cfg := StartupConfig(nil, 2*time.Second)
	if cfg.HTTP == nil {
		t.Fatal("StartupConfig leaves Config.HTTP nil: the product reports ErrNoHTTPClient and D36 never runs")
	}
	cfg.Now = fc.Now
	cfg.Endpoints = serverEndpoints(srv)
	trustServer(t, cfg.HTTP.(*http.Client), srv)

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.MaybeCheckCDN(t.Context(), false)
	if errors.Is(err, ErrNoHTTPClient) {
		t.Fatalf("MaybeCheckCDN: %v with the startup client", err)
	}
	if err != nil {
		t.Fatalf("MaybeCheckCDN: %v", err)
	}
	if !res.Ran || res.Answers != 3 {
		t.Fatalf("Result = %+v, want a run with 3 answers", res)
	}
	if got := c.Offset(); got != 2*time.Minute {
		t.Fatalf("Offset() = %v, want 2m from the endpoints", got)
	}
	if !rec.sawGREASE() {
		t.Fatal("the ClientHello has no GREASE: the client is not browser-shaped (D36, audit M1 B-4)")
	}
}

// TestClockTenMinutesBehindRecoversAfterRestart: a node whose raw clock is 10 minutes behind
// comes back to real time with the startup client (the offset lives in memory only, so a
// restart starts from the wrong raw clock again).
func TestClockTenMinutesBehindRecoversAfterRestart(t *testing.T) {
	raw := newFakeClock(base.Add(-10 * time.Minute))
	srv, _ := newClockServer(t, base)

	cfg := StartupConfig(nil, 2*time.Second)
	cfg.Now = raw.Now
	cfg.Endpoints = serverEndpoints(srv)
	if cfg.HTTP != nil {
		trustServer(t, cfg.HTTP.(*http.Client), srv)
	}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Now(); !got.Equal(base.Add(-10 * time.Minute)) {
		t.Fatalf("raw Now() = %v, want the clock 10m behind", got)
	}
	if _, err := c.MaybeCheckCDN(t.Context(), false); err != nil {
		t.Fatalf("MaybeCheckCDN: %v", err)
	}
	if got := c.Now(); !got.Equal(base) {
		t.Fatalf("Now() after the CDN check = %v, want %v", got, base)
	}
}
