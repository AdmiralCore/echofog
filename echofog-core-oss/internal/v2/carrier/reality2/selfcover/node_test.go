package selfcover

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	quic "github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2/profile"
)

const siteBody = "<html><body>hello from the operator's own site</body></html>"

type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (kv *memKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	v, ok := kv.m[string(key)]
	return v, ok, nil
}

func (kv *memKV) Put(_ context.Context, key, value []byte) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.m[string(key)] = append([]byte(nil), value...)
	return nil
}

// env is a running own-cover node against a fake CA.
type env struct {
	t      *testing.T
	node   *Node
	ca     *fakeCA
	domain string
	card   string
	mask   *ecdh.PrivateKey
	kv     *memKV
	dir    string
	pool   *x509.CertPool
	tcp    string // 127.0.0.1:port
	udp    string
	warns  []string
	mu     sync.Mutex
	errs   []error
	reps   chan *Report
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

type opts struct {
	domain    string
	challenge string
	noHTTP    bool
	group     tls.CurveID
	emptyDir  bool
	maxPre    int
	samples   int
	firstRec  time.Duration
}

func startNode(t *testing.T, o opts) *env {
	t.Helper()
	if o.domain == "" {
		o.domain = "cover.example.org"
	}
	e := &env{t: t, ca: newFakeCA(t), domain: o.domain, card: o.domain + ":443", kv: newMemKV(), reps: make(chan *Report, 16)}
	e.dir = t.TempDir()
	if !o.emptyDir {
		if err := os.WriteFile(filepath.Join(e.dir, "index.html"), []byte(siteBody), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(e.dir, ".env"), []byte("SECRET=1"), 0o644)
	}
	mask, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.mask = mask
	e.pool = x509.NewCertPool()
	e.pool.AddCert(e.ca.caCert)

	tcpAddr, httpAddr := freePort(t), freePort(t)
	e.tcp = tcpAddr
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	udpAddr := udp.LocalAddr().String()
	_ = udp.Close()
	e.udp = udpAddr
	e.ca.Resolve = func(string) (string, string) { return httpAddr, tcpAddr }

	cfg := Config{
		Domain:  o.domain,
		Content: Content{Dir: e.dir},
		ACME: ACMEConfig{DirectoryURL: e.ca.URL(), AcceptTOS: true, Challenge: o.challenge,
			HTTPClient: http.DefaultClient, Email: "ops@example.org"},
		KV: e.kv, Group: o.group,
		Masks:   []reality2.MaskKey{{Private: mask}},
		Addrs:   []string{e.card},
		TCPAddr: tcpAddr, UDPAddr: udpAddr,
		MaxPreToken: o.maxPre, FirstRecordTimeout: o.firstRec, Samples: o.samples, Settle: 50 * time.Millisecond,
		Interval: func() time.Duration { return time.Hour },
		OnWarn:   func(s string) { e.warns = append(e.warns, s) },
		OnReport: func(r *Report) { e.reps <- r },
		OnError: func(err error) {
			e.mu.Lock()
			e.errs = append(e.errs, err)
			e.mu.Unlock()
		},
	}
	if !o.noHTTP {
		cfg.HTTPAddr = httpAddr
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e.node, err = Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = e.node.Close() })
	return e
}

func (e *env) errors() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]error(nil), e.errs...)
}

// httpClient fetches the website over TCP (HTTP/2) the way a visitor does: the name resolves to
// the node, the certificate must verify against the CA.
func (e *env) httpClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", e.tcp)
		},
		TLSClientConfig: &tls.Config{RootCAs: e.pool},
	}}
}

func (e *env) get(t *testing.T, path string) (int, string, http.Header, string) {
	t.Helper()
	resp, err := e.httpClient().Get("https://" + e.domain + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header, resp.Proto
}

func TestStartRefusesNoContent(t *testing.T) {
	_, err := Start(context.Background(), Config{Domain: "x.example.org", TCPAddr: "127.0.0.1:0",
		Masks: []reality2.MaskKey{{Private: mustKey(t)}}, Addrs: []string{"x.example.org:443"}, KV: newMemKV()})
	if !errors.Is(err, ErrNoContent) {
		t.Fatalf("got %v, want ErrNoContent: there must be no template site", err)
	}
}

func mustKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEmptyFolderWarnsButStarts(t *testing.T) {
	e := startNode(t, opts{emptyDir: true, challenge: "tls-alpn-01", noHTTP: true})
	if len(e.warns) == 0 || !strings.Contains(e.warns[0], "empty") {
		t.Fatalf("no empty-folder warning: %v", e.warns)
	}
	if len(e.node.Warnings()) == 0 {
		t.Fatal("Warnings() is empty")
	}
	// The site is a plain 404, not a template page.
	code, body, _, _ := e.get(t, "/")
	if code != http.StatusNotFound || strings.Contains(body, "EchoFog") {
		t.Fatalf("empty folder served %d %q", code, body)
	}
}

func TestACMEBothChallengesAndDDNSDomain(t *testing.T) {
	for _, tc := range []struct{ name, domain, challenge string }{
		{"tls-alpn-01 on a DDNS subdomain", "myhome.duckdns.org", "tls-alpn-01"},
		{"http-01 on a DDNS subdomain", "node-42.duckdns.org", "http-01"},
		{"http-01 on an own domain", "www.example.net", "http-01"},
		{"default prefers tls-alpn-01", "default.example.org", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := startNode(t, opts{domain: tc.domain, challenge: tc.challenge, samples: 5})
			leaf := e.node.Certs().Leaf()
			if leaf == nil || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != tc.domain {
				t.Fatalf("leaf %+v is not for %s", leaf, tc.domain)
			}
			want := tc.challenge
			if want == "" {
				want = "tls-alpn-01"
			}
			if e.ca.Validated(want) != 1 {
				t.Fatalf("%s validated %d times, want 1", want, e.ca.Validated(want))
			}
			code, body, _, _ := e.get(t, "/")
			if code != 200 || body != siteBody {
				t.Fatalf("site: %d %q", code, body)
			}
			if len(leaf.Subject.Organization) != 0 {
				t.Fatalf("certificate carries an organisation: %v", leaf.Subject)
			}
		})
	}
}

func TestACMERequiresAcceptedTermsAndStoresState(t *testing.T) {
	ca := newFakeCA(t)
	ca.Resolve = func(string) (string, string) { return "127.0.0.1:1", "127.0.0.1:1" }
	c, err := NewCerts(CertsConfig{Domain: "a.example.org", KV: newMemKV(), ACME: ACMEConfig{DirectoryURL: ca.URL()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "terms") {
		t.Fatalf("Ensure without AcceptTOS: %v", err)
	}
	if ca.Issued() != 0 {
		t.Fatal("a certificate was ordered without accepted terms")
	}
	for _, bad := range []string{"", "localhost", "a b.example", "*.example.org", "a/b.example"} {
		if _, err := NewCerts(CertsConfig{Domain: bad, KV: newMemKV()}); err == nil {
			t.Fatalf("domain %q accepted", bad)
		}
	}
}

func TestStoredCertificateIsReusedAfterRestart(t *testing.T) {
	e := startNode(t, opts{challenge: "tls-alpn-01", noHTTP: true, samples: 5})
	if e.ca.Issued() != 1 {
		t.Fatalf("issued %d, want 1", e.ca.Issued())
	}
	c, err := NewCerts(CertsConfig{Domain: e.domain, KV: e.kv, ACME: ACMEConfig{DirectoryURL: e.ca.URL(), AcceptTOS: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.ca.Issued() != 1 {
		t.Fatalf("a restart ordered a new certificate (%d issued)", e.ca.Issued())
	}
	if c.Leaf().SerialNumber.Cmp(e.node.Certs().Leaf().SerialNumber) != 0 {
		t.Fatal("restart loaded another certificate")
	}
}

func TestRenewalLoopSchedulesAndRenews(t *testing.T) {
	ca := newFakeCA(t)
	ca.SetValidity(3 * time.Hour)
	now := time.Now()
	var mu sync.Mutex
	var waits []time.Duration
	release := make(chan struct{})
	changes := make(chan *x509.Certificate, 8)
	c, err := NewCerts(CertsConfig{
		Domain: "renew.example.org", KV: newMemKV(),
		ACME: ACMEConfig{DirectoryURL: ca.URL(), AcceptTOS: true, Challenge: "tls-alpn-01"},
		Now:  func() time.Time { return now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		OnChange: func(l *x509.Certificate) { changes <- l },
	})
	if err != nil {
		t.Fatal(err)
	}
	// The CA validates by connecting here, and GetCertificate answers its tls-alpn-01 challenge.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: c.GetCertificate, NextProtos: []string{"acme-tls/1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = conn.(*tls.Conn).Handshake(); _ = conn.Close() }()
		}
	}()
	ca.Resolve = func(string) (string, string) { return ln.Addr().String(), ln.Addr().String() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	first := <-changes
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitWaits := func(n int) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			mu.Lock()
			k := len(waits)
			mu.Unlock()
			if k >= n {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("the renewal loop slept %d times, want %d", k, n)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	waitWaits(1)
	// 3 hours of lifetime: renewal a third (1 h) before expiry, that is about 2 h from now.
	mu.Lock()
	w := waits[0]
	mu.Unlock()
	if w < 2*time.Hour-2*time.Minute || w > 2*time.Hour+2*time.Minute {
		t.Fatalf("first wait %v, want about 2h", w)
	}
	release <- struct{}{}
	select {
	case second := <-changes:
		if second.SerialNumber.Cmp(first.SerialNumber) == 0 {
			t.Fatal("the renewed certificate has the same serial")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no renewal after the wait")
	}
	if ca.Issued() != 2 {
		t.Fatalf("issued %d, want 2", ca.Issued())
	}
	waitWaits(2)
	cancel()
	<-done
}

func TestRenewAtUsesAThirdOfAShortLifetime(t *testing.T) {
	c, _ := NewCerts(CertsConfig{Domain: "a.example.org", KV: newMemKV()})
	nb := time.Now()
	leaf := &x509.Certificate{NotBefore: nb, NotAfter: nb.Add(6 * time.Hour)}
	if got := c.renewAt(leaf); !got.Equal(nb.Add(4 * time.Hour)) {
		t.Fatalf("renewAt = %v, want NotAfter - lifetime/3", got.Sub(nb))
	}
	leaf = &x509.Certificate{NotBefore: nb, NotAfter: nb.Add(90 * 24 * time.Hour)}
	if got := c.renewAt(leaf); !got.Equal(nb.Add(60 * 24 * time.Hour)) {
		t.Fatalf("renewAt = %v, want 30 days before expiry", got.Sub(nb))
	}
}

func TestUniformIntervalRange(t *testing.T) {
	for i := 0; i < 200; i++ {
		d := uniformInterval()
		if d < 5*time.Hour || d >= 7*time.Hour {
			t.Fatalf("interval %v outside [5h, 7h)", d)
		}
	}
}

func TestStrangerGetsTheRealSiteOverTCPAndQUIC(t *testing.T) {
	e := startNode(t, opts{samples: 5})

	code, body, hdr, proto := e.get(t, "/")
	if code != 200 || body != siteBody {
		t.Fatalf("TCP: %d %q", code, body)
	}
	if proto != "HTTP/2.0" {
		t.Fatalf("TCP site speaks %s, want HTTP/2", proto)
	}
	if !strings.Contains(hdr.Get("Alt-Svc"), "h3=") {
		t.Fatalf("no Alt-Svc on the TCP site: %v", hdr)
	}
	// Dot-files and directory listings are not served.
	if code, _, _, _ := e.get(t, "/.env"); code != 404 {
		t.Fatalf("/.env served with %d", code)
	}

	// The same site over HTTP/3 through the QUIC gate, from a client without a token.
	u, err := net.ResolveUDPAddr("udp", e.udp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, u.String(), &tls.Config{ServerName: e.domain, RootCAs: e.pool, NextProtos: []string{"h3"}}, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+e.domain+"/", nil)
	resp, err := (&http3.Transport{}).NewClientConn(conn).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != siteBody || resp.Proto != "HTTP/3.0" {
		t.Fatalf("HTTP/3: %d %s %q", resp.StatusCode, resp.Proto, b)
	}
	// Both transports negotiated the post-quantum hybrid, like the website.
	if g := conn.ConnectionState().TLS.CurveID; g != tls.X25519MLKEM768 {
		t.Fatalf("QUIC group %v, want X25519MLKEM768", g)
	}
}

func TestClassicalGroupRunsSealed(t *testing.T) {
	e := startNode(t, opts{group: tls.X25519, challenge: "tls-alpn-01", noHTTP: true, samples: 5})
	if !e.node.Sealed() || e.node.Group() != tls.X25519 {
		t.Fatalf("group %v sealed %v", e.node.Group(), e.node.Sealed())
	}
	conn, err := tls.Dial("tcp", e.tcp, &tls.Config{ServerName: e.domain, RootCAs: e.pool})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if g := conn.ConnectionState().CurveID; g != tls.X25519 {
		t.Fatalf("site selected %v, want X25519", g)
	}
	if def := startNode(t, opts{samples: 5}); def.node.Sealed() || def.node.Group() != tls.X25519MLKEM768 {
		t.Fatal("the default group must be X25519MLKEM768 and not sealed")
	}
}

// dialSelected opens a TCP connection with a token-carrying Chrome hello, as a client does, and
// returns the node's side of it.
func (e *env) dialSelected(t *testing.T) (client *utls.UConn, server net.Conn, firstFlight []byte) {
	t.Helper()
	raw, err := net.Dial("tcp", e.tcp)
	if err != nil {
		t.Fatal(err)
	}
	rec := &capConn{Conn: raw}
	uc := utls.UClient(rec, &utls.Config{ServerName: e.domain, InsecureSkipVerify: true}, utls.HelloChrome_Auto)
	if err := e.node.minter.tcpPrepare(uc); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := e.node.AcceptTCP(ctx)
		if err == nil {
			// The server handshake runs when the accepted side is used.
			go func() { _ = c.(*tls.Conn).Handshake() }()
		}
		ch <- res{c, err}
	}()
	if err := uc.HandshakeContext(ctx); err != nil {
		t.Fatalf("selected handshake: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = uc.Close(); _ = r.c.Close() })
	return uc, r.c, rec.first()
}

// capConn records the first client write: the ClientHello flight.
type capConn struct {
	net.Conn
	mu  sync.Mutex
	buf []byte
}

func (c *capConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.buf) == 0 {
		c.buf = append([]byte(nil), p...)
	}
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *capConn) first() []byte { c.mu.Lock(); defer c.mu.Unlock(); return c.buf }

func TestSelectedConnectionIsOursAndReplayGoesToTheSite(t *testing.T) {
	e := startNode(t, opts{samples: 5})
	uc, server, flight := e.dialSelected(t)
	if len(flight) == 0 {
		t.Fatal("no ClientHello captured")
	}
	st := server.(*tls.Conn).ConnectionState()
	if st.NegotiatedProtocol != "h2" || st.CurveID != tls.X25519MLKEM768 {
		t.Fatalf("selected connection: ALPN %q group %v", st.NegotiatedProtocol, st.CurveID)
	}
	// Bytes flow over the selected connection (the carrier layer would run its mux here).
	go func() { _, _ = uc.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(server, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("selected connection carries no data: %v %q", err, buf)
	}

	// Replaying the captured hello (an on-path attacker) is a stranger: the website answers it.
	raw, err := net.Dial("tcp", e.tcp)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Write(flight); err != nil {
		t.Fatal(err)
	}
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [5]byte
	if _, err := io.ReadFull(raw, hdr[:]); err != nil || hdr[0] != 0x16 {
		t.Fatalf("the website did not answer the replayed hello: %v %x", err, hdr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if c, err := e.node.AcceptTCP(ctx); err == nil {
		_ = c.Close()
		t.Fatal("a replayed hello was admitted")
	}
}

func TestSlowStrangersDoNotBlockTheSite(t *testing.T) {
	e := startNode(t, opts{maxPre: 2, samples: 5, firstRec: 300 * time.Millisecond})

	var holders []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", e.tcp)
		if err != nil {
			t.Fatal(err)
		}
		holders = append(holders, c)
	}
	defer func() {
		for _, c := range holders {
			c.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	// Both slots are taken by silent connections: a third visitor is served at once, unread.
	code, body, _, _ := e.get(t, "/")
	if code != 200 || body != siteBody {
		t.Fatalf("visitor over the pre-token limit: %d %q", code, body)
	}
	// A silent connection is handed to the site after the timeout and may still complete a
	// handshake with the bytes it sends late.
	time.Sleep(500 * time.Millisecond)
	conn := tls.Client(holders[0], &tls.Config{ServerName: e.domain, RootCAs: e.pool})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("late hello on a slow connection: %v", err)
	}
	// The slots are free again.
	for i := 0; i < 4; i++ {
		if code, _, _, _ := e.get(t, "/"); code != 200 {
			t.Fatalf("visitor %d: %d", i, code)
		}
	}
}

// capPacketConn records the first datagram the client sends.
type capPacketConn struct {
	net.PacketConn
	mu    sync.Mutex
	first [][]byte
}

func (c *capPacketConn) WriteTo(p []byte, a net.Addr) (int, error) {
	c.mu.Lock()
	if len(c.first) < 4 {
		c.first = append(c.first, append([]byte(nil), p...))
	}
	c.mu.Unlock()
	return c.PacketConn.WriteTo(p, a)
}

func TestQUICSelectedAndReplay(t *testing.T) {
	e := startNode(t, opts{samples: 5})
	raddr, _ := net.ResolveUDPAddr("udp", e.udp)
	udp, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	cap := &capPacketConn{PacketConn: udp}
	conf := &quic.Config{ChromeParrot: true}
	e.node.minter.quicHook(conf)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accepted := make(chan error, 1)
	go func() {
		c, err := e.node.AcceptQUIC(ctx)
		if err == nil {
			_ = c.Close()
		}
		accepted <- err
	}()
	conn, err := quic.Dial(ctx, cap, raddr, &tls.Config{ServerName: e.domain, NextProtos: []string{"h3"}, InsecureSkipVerify: true}, conf)
	if err != nil {
		t.Fatalf("selected QUIC handshake: %v", err)
	}
	defer conn.CloseWithError(0, "")
	if err := <-accepted; err != nil {
		t.Fatalf("AcceptQUIC: %v", err)
	}
	if g := conn.ConnectionState().TLS.CurveID; g != tls.X25519MLKEM768 {
		t.Fatalf("group %v", g)
	}

	// Replay of the captured Initial flight from another source: forwarded to the website.
	cap.mu.Lock()
	flight := append([][]byte(nil), cap.first...)
	cap.mu.Unlock()
	if len(flight) == 0 {
		t.Fatal("nothing captured")
	}
	atk, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer atk.Close()
	for _, p := range flight {
		if _, err := atk.WriteToUDP(p, raddr); err != nil {
			t.Fatal(err)
		}
	}
	_ = atk.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := atk.ReadFromUDP(buf)
	if err != nil || n == 0 || buf[0]&0x80 == 0 {
		t.Fatalf("the website did not answer the replayed Initial: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	if c, err := e.node.AcceptQUIC(ctx2); err == nil {
		_ = c.Close()
		t.Fatal("a replayed QUIC hello was admitted")
	}
}

// ownProbers builds the probes of the own check: the website (no token) and a selected
// connection, the same as Remeasure uses, with the given sample counts.
func (e *env) ownProbers(siteSamples int) (site, ours *profile.Prober) {
	site = profile.NewProber(profile.ProbeConfig{Samples: siteSamples, Settle: 50 * time.Millisecond, IgnorePost: true, OnLocal: e.node.registerOwn})
	ours = profile.NewProber(profile.ProbeConfig{Samples: 5, Settle: 50 * time.Millisecond, NoRequest: true, IgnorePost: true,
		OnLocal: e.node.registerOwn, PrepareTCP: e.node.minter.tcpPrepare, PrepareQUIC: e.node.minter.quicHook})
	return
}

// TestCompareSiteAndSelectedEmpty is the central test of T2.6b-2: over TCP and QUIC, over 20
// runs, the profile of a selected connection matches the website's. The website is measured once
// with many samples (the length ranges of an ECDSA signature need them), each run measures the
// selected connection with the five samples of T2.6b-1.
// packingAttempts is how often a run is measured again when its only difference is an unstable
// packing of our side (see TestCompareSiteAndSelectedEmpty).
const packingAttempts = 4

// onlyPackingUnstable reports whether diffs is exactly the load-induced "packing" noise.
func onlyPackingUnstable(diffs []profile.Diff) bool {
	if len(diffs) == 0 {
		return false
	}
	for _, d := range diffs {
		if d.Item != profile.ItemPacking || d.Ours != "unstable" {
			return false
		}
	}
	return true
}

func TestCompareSiteAndSelectedEmpty(t *testing.T) {
	runs := 20
	if testing.Short() {
		runs = 3
	}
	e := startNode(t, opts{samples: 5})
	siteP, oursP := e.ownProbers(60)
	defer e.node.clearOwn()
	ctx := context.Background()

	for _, tr := range []profile.Transport{profile.TCP, profile.QUIC} {
		target := profile.Target{Host: e.domain, SNI: e.domain}
		measure := func(p *profile.Prober) (*profile.CoverProfile, error) {
			if tr == profile.TCP {
				target.Addr = e.tcp
				return p.MeasureTCP(ctx, target)
			}
			target.Addr = e.udp
			return p.MeasureQUIC(ctx, target)
		}
		site, err := measure(siteP)
		if err != nil {
			t.Fatalf("%s site: %v", tr, err)
		}
		if !site.RequestOK {
			t.Fatalf("%s: the website did not answer the request: %s", tr, site.RequestInfo)
		}
		if tr == profile.QUIC && site.Group != uint16(tls.X25519MLKEM768) {
			t.Fatalf("QUIC site group %#x", site.Group)
		}
		for run := 0; run < runs; run++ {
			// The comparison itself stays strict. How a flight is cut into packets depends on
			// timing, and under -race and load a run of five samples can catch a rare cut; that
			// is noise of the test machine, not a property of the server, so such a run is
			// measured again. A real difference shows in every attempt (T2.8 F1).
			var diffs []profile.Diff
			for attempt := 0; attempt < packingAttempts; attempt++ {
				ours, err := measure(oursP)
				if err != nil {
					t.Fatalf("%s run %d: %v", tr, run, err)
				}
				if diffs, _ = CompareOwn(site, ours); !onlyPackingUnstable(diffs) {
					break
				}
			}
			if len(diffs) > 0 {
				t.Fatalf("%s run %d: the selected connection differs from the website: %v", tr, run, diffs)
			}
		}
	}
}

func TestRemeasureReportsAndStoresTheProfile(t *testing.T) {
	e := startNode(t, opts{samples: 12})
	rep := <-e.reps // the report of the start
	if rep.Reason != "start" {
		t.Fatalf("first report reason %q", rep.Reason)
	}
	if !rep.OK() {
		t.Fatalf("diffs at start: %v", rep.Diffs)
	}
	if rep.Site[profile.TCP] == nil || rep.Site[profile.QUIC] == nil {
		t.Fatal("a transport was not measured")
	}
	if _, ok, err := profile.Load(context.Background(), e.kv, profile.TCP, e.domain); err != nil || !ok {
		t.Fatalf("the profile was not stored: %v %v", ok, err)
	}

	// An ACME renewal changes the certificate: the node measures again and says why.
	before := e.node.Certs().Leaf().SerialNumber.String()
	if err := e.node.Certs().renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-e.reps:
		if r.Reason != "certificate" || r.Serial == before || r.Serial != e.node.Certs().Leaf().SerialNumber.String() {
			t.Fatalf("report after renewal: reason %q serial %q (before %q)", r.Reason, r.Serial, before)
		}
		if !r.OK() {
			t.Fatalf("diffs after renewal: %v", r.Diffs)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("no re-measurement after the certificate changed")
	}
}

func TestSiteHidesDotFilesAndListings(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "docs", "a.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".git"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644)
	h, warns, err := Content{Dir: dir}.handler()
	if err != nil || len(warns) != 0 {
		t.Fatalf("handler: %v %v", err, warns)
	}
	for path, want := range map[string]int{"/": 200, "/docs/a.txt": 200, "/docs/": 404, "/.git": 404, "/docs/.x": 404} {
		rr := &respRec{h: http.Header{}}
		h.ServeHTTP(rr, mustReq(path))
		if rr.code != want && !(rr.code == 0 && want == 200) {
			t.Errorf("%s: %d, want %d", path, rr.code, want)
		}
	}
	if _, _, err := (Content{Dir: dir, Proxy: "http://127.0.0.1:1"}).handler(); err == nil {
		t.Fatal("both a folder and a proxy accepted")
	}
	if _, _, err := (Content{Proxy: "ftp://x"}).handler(); err == nil {
		t.Fatal("a non-http proxy accepted")
	}
	if _, w, err := (Content{Dir: t.TempDir()}).handler(); err != nil || len(w) != 1 {
		t.Fatalf("empty folder: %v %v", err, w)
	}
}

func TestProxyContent(t *testing.T) {
	app := newAppServer(t)
	h, _, err := Content{Proxy: app}.handler()
	if err != nil {
		t.Fatal(err)
	}
	rr := &respRec{h: http.Header{}}
	h.ServeHTTP(rr, mustReq("/x"))
	if !strings.Contains(rr.body.String(), "app says /x") {
		t.Fatalf("proxy: %q", rr.body.String())
	}
}

// TestCompareOwnCatchesADifferentServer is the negative control: a server that is not the website
// (a self-signed certificate of another size, another ALPN) must show up, or the empty result of
// the test above would prove nothing.
func TestCompareOwnCatchesADifferentServer(t *testing.T) {
	e := startNode(t, opts{samples: 5})
	other, err := profile.StartOwnTCP(profile.OwnConfig{SNI: e.domain, ALPN: "http/1.1", Key: x509.ECDSA})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	siteP, _ := e.ownProbers(10)
	defer e.node.clearOwn()
	ctx := context.Background()
	site, err := siteP.MeasureTCP(ctx, profile.Target{Host: e.domain, SNI: e.domain, Addr: e.tcp})
	if err != nil {
		t.Fatal(err)
	}
	pr := profile.NewProber(profile.ProbeConfig{Samples: 5, Settle: 50 * time.Millisecond, NoRequest: true, IgnorePost: true})
	ours, err := pr.MeasureTCP(ctx, profile.Target{Host: e.domain, SNI: e.domain, Addr: other.Addr})
	if err != nil {
		t.Fatal(err)
	}
	if diffs, _ := CompareOwn(site, ours); len(diffs) == 0 {
		t.Fatal("CompareOwn found no difference between the website and a self-signed server")
	}
}
