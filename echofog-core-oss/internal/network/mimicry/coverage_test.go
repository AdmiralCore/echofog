package mimicry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// feedConn returns a net.Conn that yields b then EOF, for feeding a crafted record to a reader.
func feedConn(t *testing.T, b []byte) net.Conn {
	t.Helper()
	w, r := net.Pipe()
	go func() {
		_, _ = w.Write(b)
		_ = w.Close()
	}()
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestGenerateCamouflageCert(t *testing.T) {
	for _, name := range []string{"", "example.test"} {
		cert, err := GenerateCamouflageCert(name)
		if err != nil {
			t.Fatalf("name %q: %v", name, err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		want := name
		if want == "" {
			want = "www.microsoft.com"
		}
		if leaf.Subject.CommonName != want {
			t.Fatalf("CN = %q, want %q", leaf.Subject.CommonName, want)
		}
		if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != want {
			t.Fatalf("DNSNames = %v, want [%q]", leaf.DNSNames, want)
		}
	}
}

func TestReadClientHelloRecordRejectsNonHello(t *testing.T) {
	cases := []struct {
		name string
		rec  []byte
		want bool
	}{
		{"short header", []byte{22, 3, 1}, false},
		{"not handshake type", []byte{23, 3, 3, 0, 5, 1, 2, 3, 4, 5}, false},
		{"not TLS major", []byte{22, 2, 3, 0, 5, 1, 2, 3, 4, 5}, false},
		{"zero length", []byte{22, 3, 1, 0, 0}, false},
		{"truncated body", []byte{22, 3, 1, 0, 10, 1, 2, 3}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := readClientHelloRecord(feedConn(t, c.rec))
			if ok != c.want {
				t.Fatalf("ok = %v, want %v", ok, c.want)
			}
		})
	}
}

func TestReadClientHelloRecordOversizedLength(t *testing.T) {
	// length field claims more than the cap; rejected before the body is read
	rec := []byte{22, 3, 1, 0xff, 0xff}
	_, ok := readClientHelloRecord(feedConn(t, rec))
	if ok {
		t.Fatal("an oversized record length was accepted")
	}
}

func TestSessionIDFromClientHelloEdgeCases(t *testing.T) {
	const off = 5 + 4 + 2 + 32
	// too short to hold the session id length byte
	if _, ok := sessionIDFromClientHello(make([]byte, off)); ok {
		t.Fatal("a record too short returned a session id")
	}
	// wrong handshake type byte
	bad := make([]byte, off+1+4)
	bad[5] = 2
	bad[off] = 4
	if _, ok := sessionIDFromClientHello(bad); ok {
		t.Fatal("a non-ClientHello returned a session id")
	}
	// zero-length session id
	zero := make([]byte, off+1)
	zero[5] = 1
	zero[off] = 0
	if _, ok := sessionIDFromClientHello(zero); ok {
		t.Fatal("a zero-length session id was returned")
	}
	// session id length overruns the record
	over := make([]byte, off+1+2)
	over[5] = 1
	over[off] = 32
	if _, ok := sessionIDFromClientHello(over); ok {
		t.Fatal("a session id length past the record end was accepted")
	}
	// a well-formed 4-byte session id is returned exactly
	good := make([]byte, off+1+4)
	good[5] = 1
	good[off] = 4
	copy(good[off+1:], []byte{9, 8, 7, 6})
	sid, ok := sessionIDFromClientHello(good)
	if !ok || len(sid) != 4 || sid[0] != 9 || sid[3] != 6 {
		t.Fatalf("sid = %v ok = %v", sid, ok)
	}
}

func TestReplayCacheEvictionAndExpiry(t *testing.T) {
	r := &replayCache{seen: make(map[string]int64), max: 2}
	now := int64(1000)
	if r.seenBefore("a", now) {
		t.Fatal("first sight reported as replay")
	}
	if !r.seenBefore("a", now) {
		t.Fatal("second sight not reported as replay")
	}
	// an entry past its expiry is no longer a replay
	if r.seenBefore("a", now+2*ReplayWindowSeconds+1) {
		t.Fatal("an expired entry still counted as replay")
	}
	// filling to max with expired entries prunes them to make room
	r = &replayCache{seen: make(map[string]int64), max: 2}
	r.seenBefore("x", now)
	r.seenBefore("y", now)
	// both x and y expired by this time; adding z prunes them
	if r.seenBefore("z", now+2*ReplayWindowSeconds+1) {
		t.Fatal("z should be new")
	}
	if len(r.seen) != 1 {
		t.Fatalf("expired entries were not pruned: %d left", len(r.seen))
	}
	// when nothing is expired and the map is full, it is reset rather than growing
	r = &replayCache{seen: make(map[string]int64), max: 2}
	r.seenBefore("p", now)
	r.seenBefore("q", now)
	r.seenBefore("s", now) // not expired, map full -> reset then insert
	if len(r.seen) != 1 {
		t.Fatalf("full live map was not reset: %d entries", len(r.seen))
	}
}

func TestDeriveAEADEmptyKey(t *testing.T) {
	if _, err := DeriveAEAD(nil); err == nil {
		t.Fatal("empty key accepted")
	}
}

func TestVersionName(t *testing.T) {
	if tlsVersionName(tls.VersionTLS12) != "1.2" {
		t.Fatal("1.2 name")
	}
	if got := tlsVersionName(0x0301); got != "0x0301" {
		t.Fatalf("unknown version name = %q", got)
	}
}

func TestStealthKeyFromPassword(t *testing.T) {
	empty := StealthKeyFromPassword("")
	def := DefaultStealthKey()
	if string(empty) != string(def) {
		t.Fatal("empty password should give the default key")
	}
	if string(StealthKeyFromPassword("secret")) == string(def) {
		t.Fatal("a password should change the key")
	}
}

func TestBuildServerTLSConfigAcceptsAnyPeerCert(t *testing.T) {
	cfg := BuildServerTLSConfig(generateTestCertificate(t))
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("no VerifyPeerCertificate set")
	}
	if err := cfg.VerifyPeerCertificate([][]byte{{1, 2, 3}}, nil); err != nil {
		t.Fatalf("peer cert check should accept anything, got %v", err)
	}
}

func TestServerHandshakePassesNonTLSToTheProbe(t *testing.T) {
	cfg := DefaultMimicryConfig()
	cfg.AuthKey = NodeStealthKey("", [32]byte{1})
	conn, isPeer, _, err := ServerHandshake(feedConn(t, []byte("GET / HTTP/1.1\r\n")), BuildServerTLSConfig(generateTestCertificate(t)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if isPeer {
		t.Fatal("plain HTTP was taken for a peer")
	}
	if conn == nil {
		t.Fatal("no pass-on connection returned")
	}
}

func TestHandleActiveProbeWithoutDestCloses(t *testing.T) {
	cfg := &MimicryConfig{} // no FallbackAddr, no ServerName -> nothing to relay to
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan struct{})
	go func() { HandleActiveProbe(a, cfg); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleActiveProbe did not return when there was nowhere to relay")
	}
	// the caller side is closed
	_ = a.SetReadDeadline(time.Now().Add(time.Second))
}

func TestHandleActiveProbeUnreachableDestCloses(t *testing.T) {
	cfg := &MimicryConfig{FallbackAddr: "127.0.0.1:1"} // refused fast
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan struct{})
	go func() { HandleActiveProbe(a, cfg); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("HandleActiveProbe did not return for an unreachable dest")
	}
}

func TestCheckConformanceUnreachableDest(t *testing.T) {
	cfg := BuildServerTLSConfig(generateTestCertificate(t))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	verdict, ok := CheckServerHelloConformance(ctx, cfg, "127.0.0.1:1", "www.microsoft.com")
	if ok {
		t.Fatal("conformance reported ok for an unreachable site")
	}
	if verdict == "" {
		t.Fatal("no verdict text")
	}
}

func TestProbeSiteErrorOnRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := ProbeSite(ctx, "127.0.0.1:1", "x"); err == nil {
		t.Fatal("ProbeSite should fail on a refused address")
	}
}

func TestClientHandshakeEmptyKeyErrors(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	cfg := DefaultMimicryConfig()
	cfg.AuthKey = nil // DeriveAEAD fails before any I/O
	if _, _, err := ClientHandshake(a, cfg); err == nil {
		t.Fatal("ClientHandshake accepted an empty auth key")
	}
}

func TestServerHandshakeBadTokenRelayed(t *testing.T) {
	// a real TLS 1.3 ClientHello whose session id is not a valid token: treated as a stranger
	cfg := DefaultMimicryConfig()
	cfg.AuthKey = NodeStealthKey("", [32]byte{2})
	srvTLS := BuildServerTLSConfig(generateTestCertificate(t))

	cliRaw, srvRaw := net.Pipe()
	defer cliRaw.Close()
	go func() {
		c := tls.Client(cliRaw, &tls.Config{InsecureSkipVerify: true, ServerName: "www.microsoft.com"})
		_ = c.Handshake() // no token sent in the session id
	}()
	conn, isPeer, _, err := ServerHandshake(srvRaw, srvTLS, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if isPeer {
		t.Fatal("a ClientHello without a valid token was taken for a peer")
	}
	if conn == nil {
		t.Fatal("no pass-on connection for the stranger")
	}
	_ = srvRaw.Close()
}
