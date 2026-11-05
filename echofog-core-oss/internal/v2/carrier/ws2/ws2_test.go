package ws2

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/carriertest"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

const (
	secretPath = "/s/4f9c1e7a2b6d8035"
	siteBanner = "plain site"
)

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// rig is a node behind a "CDN": an httptest TLS server whose handler is a ReverseProxy to the
// node's plain HTTP listener, the way a CDN forwards a WebSocket.
type rig struct {
	node    *Listener
	cdn     *httptest.Server
	cdnAddr string // host:port of the CDN, the token's address
	card    *objects.Card
	addr    *pb.Addr
	roots   *x509.CertPool
	hellos  chan *tls.ClientHelloInfo
}

type rigOpt func(*Config)

func newRig(t *testing.T, opts ...rigOpt) *rig {
	t.Helper()
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("http://" + backend.Addr().String())
	r := &rig{hellos: make(chan *tls.ClientHelloInfo, 64)}
	r.cdn = httptest.NewUnstartedServer(httputil.NewSingleHostReverseProxy(target))
	r.cdn.TLS = &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case r.hellos <- h:
			default:
			}
			return nil, nil
		},
	}
	r.cdn.StartTLS()
	t.Cleanup(r.cdn.Close)
	r.cdnAddr = r.cdn.Listener.Addr().String()
	r.roots = x509.NewCertPool()
	r.roots.AddCert(r.cdn.Certificate())

	own := testCertificate(t)
	cfg := Config{
		Path:  secretPath,
		Cover: reality2.TCPCover{Group: tls.X25519MLKEM768, ALPN: "h2", Cert: &own},
		Masks: []reality2.MaskKey{{Private: sk}},
		Addrs: []string{r.cdnAddr},
		Site:  http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, siteBanner) }),
	}
	for _, o := range opts {
		o(&cfg)
	}
	r.node, err = Listen(backend, cfg)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = r.node.Close() })

	params, _ := objects.Marshal(&pb.SuiteParams{Sni: "cover.example"})
	r.addr = &pb.Addr{Suite: objects.SuiteWS2, Url: "wss://" + r.cdnAddr + secretPath, Mask: 1, Params: params}
	r.card = &objects.Card{Body: &pb.NodeCard{MaskKeys: []*pb.MaskKey{{Key: keys.New(keys.X25519, sk.PublicKey().Bytes())}}}}
	return r
}

func (r *rig) dialer() *Dialer { return NewDialer(DialerConfig{RootCAs: r.roots}) }

func (r *rig) pair(t *testing.T) (client, server mux.Carrier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type res struct {
		c   mux.Carrier
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := r.node.Accept(ctx); ch <- res{c, err} }()
	c, err := r.dialer().Dial(ctx, carrier.DialRequest{Card: r.card, Addr: r.addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	s := <-ch
	if s.err != nil {
		t.Fatalf("accept: %v", s.err)
	}
	t.Cleanup(func() { c.Close(); s.c.Close() })
	return c, s.c
}

// rawWS opens a WebSocket to the CDN path without the ws2 dialer: a stranger's client.
func (r *rig) rawWS(path string) (*websocket.Conn, *http.Response, error) {
	d := websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: r.roots}, HandshakeTimeout: 5 * time.Second}
	return d.Dial("wss://"+r.cdnAddr+path, nil)
}

func TestCarrierConformance(t *testing.T) {
	carriertest.Run(t, func(t *testing.T) (mux.Carrier, mux.Carrier) { return newRig(t).pair(t) })
}

func TestParseURL(t *testing.T) {
	for _, tc := range []struct {
		in, hp, path string
		bad          bool
	}{
		{in: "wss://cdn.example.org/s/abc", hp: "cdn.example.org:443", path: "/s/abc"},
		{in: "wss://cdn.example.org:8443/s/abc?x=1", hp: "cdn.example.org:8443", path: "/s/abc"},
		{in: "wss://[2001:db8::1]/p", hp: "[2001:db8::1]:443", path: "/p"},
		{in: "wss://cdn.example.org", hp: "cdn.example.org:443", path: "/"},
		{in: "ws://cdn.example.org/p", bad: true},
		{in: "https://cdn.example.org/p", bad: true},
		{in: "wss:///p", bad: true},
		{in: "wss://u:p@cdn.example.org/p", bad: true},
		{in: "wss://cdn.example.org:0/p", bad: true},
		{in: "", bad: true},
	} {
		hp, path, err := ParseURL(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: accepted", tc.in)
			}
			continue
		}
		if err != nil || hp != tc.hp || path != tc.path {
			t.Errorf("%q: got %q %q %v, want %q %q", tc.in, hp, path, err, tc.hp, tc.path)
		}
	}
}

func FuzzParseURL(f *testing.F) {
	for _, s := range []string{"wss://a.b/c", "wss://[::1]:1/", "ws://x", "wss://", "wss://a:99999/%zz"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		hp, path, err := ParseURL(s)
		if err != nil {
			return
		}
		if !strings.HasPrefix(path, "/") {
			t.Fatalf("path %q does not start with /", path)
		}
		if _, _, err := net.SplitHostPort(hp); err != nil {
			t.Fatalf("host:port %q: %v", hp, err)
		}
	})
}

// TestOuterTLSIsABrowserHello: the ClientHello the CDN sees is Chrome's (GREASE, http/1.1 only for
// a WebSocket), which the Go standard stack never sends (wire.md §4.4).
func TestOuterTLSIsABrowserHello(t *testing.T) {
	r := newRig(t)
	r.pair(t)
	h := <-r.hellos
	if len(h.CipherSuites) == 0 || h.CipherSuites[0]&0x0f0f != 0x0a0a {
		t.Errorf("cipher suites %#x: the first is not a GREASE value, so not a browser hello", h.CipherSuites)
	}
	if len(h.SupportedProtos) != 1 || h.SupportedProtos[0] != "http/1.1" {
		t.Errorf("ALPN %v, want [http/1.1]", h.SupportedProtos)
	}
}

// TestOtherPathsServeTheSite: only the exact secret path upgrades; every other path, and the
// secret path without an upgrade, is an ordinary site.
func TestOtherPathsServeTheSite(t *testing.T) {
	r := newRig(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: r.roots}}}
	for _, p := range []string{"/", "/s", secretPath + "x", secretPath + "/", "/index.html", secretPath} {
		resp, err := client.Get("https://" + r.cdnAddr + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != siteBanner {
			t.Errorf("GET %s: %d %q, want the site", p, resp.StatusCode, b)
		}
	}
	for _, p := range []string{"/", secretPath + "x"} {
		ws, resp, err := r.rawWS(p)
		if err == nil {
			ws.Close()
			t.Errorf("WebSocket upgrade succeeded on %s", p)
			continue
		}
		if resp == nil || resp.StatusCode == http.StatusSwitchingProtocols {
			t.Errorf("upgrade on %s: %v %v", p, resp, err)
		}
	}
}

// strangerOutcome connects a stranger, sends first, and reports how the server ended the WebSocket.
func strangerOutcome(t *testing.T, r *rig, first func(*websocket.Conn)) (code int, after time.Duration) {
	t.Helper()
	ws, _, err := r.rawWS(secretPath)
	if err != nil {
		t.Errorf("upgrade: %v", err)
		return 0, 0
	}
	defer ws.Close()
	start := time.Now()
	first(ws)
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, _, err = ws.ReadMessage()
	ce, ok := err.(*websocket.CloseError)
	if !ok {
		t.Errorf("not a close frame: %v", err)
		return 0, 0
	}
	return ce.Code, time.Since(start)
}

// TestStrangersAreClosedWith1008: no token, whatever the reason, ends in close code 1008 after
// uniform(0.05, 0.5) s, the same for every reason (AD20): garbage, a record that is not a hello,
// a hello of a standard TLS client (no token).
func TestStrangersAreClosedWith1008(t *testing.T) {
	r := newRig(t)
	cases := map[string]func(*websocket.Conn){
		"garbage": func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte("GET / HTTP/1.1\r\n\r\n"))
		},
		"not a handshake record": func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte{0x17, 3, 3, 0, 5, 1, 2, 3, 4, 5})
		},
		"huge record": func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte{0x16, 3, 1, 0xff, 0xff, 1})
		},
		"stdlib hello": func(ws *websocket.Conn) {
			c := tls.Client(&wsPipe{ws: ws}, &tls.Config{InsecureSkipVerify: true, ServerName: "cover.example"})
			_ = c.Handshake() // sends the hello, then fails reading: the test reads the answer itself
		},
	}
	var wg sync.WaitGroup
	for name, f := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, d := strangerOutcome(t, r, f)
			if code != websocket.ClosePolicyViolation {
				t.Errorf("%s: close code %d, want 1008", name, code)
			}
			if d < 50*time.Millisecond || d > 3*time.Second {
				t.Errorf("%s: closed after %v, want within uniform(0.05, 0.5) s", name, d)
			}
		}()
	}
	wg.Wait()
}

// TestStrangerOverPreTokenLimitGetsTheSameRefusal: a connection over the pre-token limit is refused
// with the same code (AD20), not reset and not proxied.
func TestStrangerOverPreTokenLimitGetsTheSameRefusal(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxPreToken = 1 })
	hold, _, err := r.rawWS(secretPath) // takes the only pre-token slot and says nothing
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	time.Sleep(300 * time.Millisecond)
	code, d := strangerOutcome(t, r, func(ws *websocket.Conn) { _ = ws.WriteMessage(websocket.BinaryMessage, []byte("x")) })
	if code != websocket.ClosePolicyViolation || d < 50*time.Millisecond || d > 3*time.Second {
		t.Fatalf("over the limit: code %d after %v", code, d)
	}
}

// TestOversizedMessageIsRefused: a message above 16 KiB ends the connection.
func TestOversizedMessageIsRefused(t *testing.T) {
	r := newRig(t)
	ws, _, err := r.rawWS(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.WriteMessage(websocket.BinaryMessage, make([]byte, MaxMessage+1))
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("the server answered an oversized message with data")
	}
}

// TestWritesAreBinaryMessagesUpTo16KiB: the connection adapter sends binary messages of at most
// 16 KiB, however much is written at once.
func TestWritesAreBinaryMessagesUpTo16KiB(t *testing.T) {
	srvWS := make(chan *websocket.Conn, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, req, nil)
		if err == nil {
			srvWS <- c
		}
	}))
	defer hs.Close()
	cws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cws.Close()
	sws := <-srvWS
	defer sws.Close()
	conn := newConn(cws, nil, nil)
	const total = 3*MaxMessage + 123
	go func() { _, _ = conn.Write(make([]byte, total)) }()
	got := 0
	for got < total {
		mt, b, err := sws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if mt != websocket.BinaryMessage || len(b) > MaxMessage || len(b) == 0 {
			t.Fatalf("message type %d length %d", mt, len(b))
		}
		got += len(b)
	}
}

// TestDialerUsesTheProxyHook: the socket to the CDN is opened by DialRequest.DialContext (T2.1).
func TestDialerUsesTheProxyHook(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	var seen []string
	hook := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		seen = append(seen, network+" "+address)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		if c, err := r.node.Accept(ctx); err == nil {
			defer c.Close()
		}
	}()
	c, err := r.dialer().Dial(ctx, carrier.DialRequest{Card: r.card, Addr: r.addr, DialContext: hook})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "tcp "+r.cdnAddr {
		t.Fatalf("proxy hook saw %v, want [tcp %s]", seen, r.cdnAddr)
	}
}

func TestDialerRejectsBadAddresses(t *testing.T) {
	r := newRig(t)
	d := r.dialer()
	ctx := context.Background()
	for name, mut := range map[string]func(a *pb.Addr){
		"wrong suite": func(a *pb.Addr) { a.Suite = objects.SuiteReality2TCP },
		"plain ws":    func(a *pb.Addr) { a.Url = "ws://" + r.cdnAddr + secretPath },
		"no sni":      func(a *pb.Addr) { a.Params = nil },
		"no mask":     func(a *pb.Addr) { a.Mask = 0 },
	} {
		a := &pb.Addr{Suite: r.addr.Suite, Url: r.addr.Url, Mask: r.addr.Mask, Params: r.addr.Params}
		mut(a)
		if _, err := d.Dial(ctx, carrier.DialRequest{Card: r.card, Addr: a}); err == nil {
			t.Errorf("%s: dial succeeded", name)
		}
	}
}

// wsPipe lets a standard TLS client run over a raw WebSocket in tests.
type wsPipe struct {
	ws *websocket.Conn
}

// Read never reads the WebSocket: only the test's own goroutine does.
func (p *wsPipe) Read([]byte) (int, error) { return 0, io.EOF }
func (p *wsPipe) Write(b []byte) (int, error) {
	return len(b), p.ws.WriteMessage(websocket.BinaryMessage, b)
}
func (p *wsPipe) Close() error                     { return p.ws.Close() }
func (p *wsPipe) LocalAddr() net.Addr              { return p.ws.LocalAddr() }
func (p *wsPipe) RemoteAddr() net.Addr             { return p.ws.RemoteAddr() }
func (p *wsPipe) SetDeadline(time.Time) error      { return nil }
func (p *wsPipe) SetReadDeadline(time.Time) error  { return nil }
func (p *wsPipe) SetWriteDeadline(time.Time) error { return nil }
