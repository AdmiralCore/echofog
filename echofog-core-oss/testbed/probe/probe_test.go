package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------- Reduce / Equivalent

func TestReduceMakesStableAnswers(t *testing.T) {
	alert := []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x46}
	cases := []struct {
		name string
		net  string
		got  []byte
		end  error
		want Answer
	}{
		{"silence", "tcp", nil, errors.New("i/o timeout (deadline exceeded)"), Answer{Class: "silence"}},
		{"closed", "tcp", nil, io.EOF, Answer{Class: "closed"}},
		{"reset", "tcp", nil, syscall.ECONNRESET, Answer{Class: "reset"}},
		{"alert", "tcp", alert, io.EOF, Answer{Class: "alert", Detail: "70"}},
		{"handshake record", "tcp", []byte{0x16, 0x03, 0x03, 0x00, 0x7a, 0x02}, nil, Answer{Class: "tls", Detail: "22/0303/122"}},
		{"http", "tcp", []byte("HTTP/1.1 400 Bad Request\r\n\r\n"), io.EOF, Answer{Class: "http", Detail: "400"}},
		{"other bytes", "tcp", []byte{1, 2, 3}, nil, Answer{Class: "bytes"}},
		{"udp silence", "udp", nil, errors.New("i/o timeout (deadline exceeded)"), Answer{Class: "silence"}},
		{"udp long header", "udp", []byte{0xc3, 0, 0, 0, 1, 8}, nil, Answer{Class: "datagram", Detail: "long/00000001"}},
		{"udp version negotiation", "udp", []byte{0x80, 0, 0, 0, 0, 8}, nil, Answer{Class: "datagram", Detail: "long/00000000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Reduce(tc.net, tc.got, tc.end))
		})
	}
}

func TestEquivalent(t *testing.T) {
	assert.True(t, Equivalent(Answer{Class: "silence"}, Answer{Class: "silence"}))
	assert.True(t, Equivalent(Answer{Class: "tls", Detail: "22/0303/122"}, Answer{Class: "tls", Detail: "22/0303/122"}))
	assert.False(t, Equivalent(Answer{Class: "tls", Detail: "22/0303/122"}, Answer{Class: "tls", Detail: "22/0303/123"}), "the length of the first record is part of the answer")
	assert.False(t, Equivalent(Answer{Class: "tls", Detail: "22/0303/122"}, Answer{Class: "alert", Detail: "70"}))
	assert.False(t, Equivalent(Answer{Class: "http", Detail: "400"}, Answer{Class: "http", Detail: "404"}))
	assert.False(t, Equivalent(Answer{Class: "closed"}, Answer{Class: "reset"}))
}

func TestOwnAnswersCountsOnlyTheUnexpected(t *testing.T) {
	r := Report{Outcomes: []Outcome{
		{Own: false}, {Own: true}, {Own: true, Expected: true}, {Own: true, Skipped: "no second address"},
	}}
	assert.Equal(t, 1, r.OwnAnswers())
}

// ---------------------------------------------------------------------------- a cover and nodes

type lab struct {
	coverAddr string
	coverSrv  *httptest.Server
	conns     atomic.Int64
}

func newCover(t *testing.T) *lab {
	t.Helper()
	l := &lab{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "covered/1.0")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>the cover page</html>")
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{NextProtos: []string{"http/1.1"}, Certificates: []tls.Certificate{selfSigned(t)}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	l.coverSrv = srv
	l.coverAddr = srv.Listener.Addr().String()
	return l
}

// selfSigned makes a certificate of its own: httptest servers all share one, which would make two
// different covers look like the same site.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "cover.lab"}, DNSNames: []string{"cover.lab"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type nodeBehaviour struct {
	// own, when set, answers the first bytes it likes with a reply of its own instead of the cover.
	own func(first []byte) []byte
	// token, when set, is the one first flight the node admits (once) and answers on its own.
	token []byte
	// now is the node's clock; a token older than window is a stranger.
	now    func() time.Time
	window time.Duration
}

type node struct {
	addr string
	mu   sync.Mutex
	used map[string]time.Time
	b    nodeBehaviour
}

// newNode starts a TCP node that proxies every stranger to the cover.
func newNode(t *testing.T, cover string, b nodeBehaviour) *node {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	n := &node{addr: ln.Addr().String(), used: map[string]time.Time{}, b: b}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go n.serve(c, cover)
		}
	}()
	return n
}

func (n *node) serve(c net.Conn, cover string) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	first := make([]byte, 4096)
	k, err := c.Read(first)
	if err != nil || k == 0 {
		return
	}
	first = first[:k]
	if n.b.token != nil && string(first) == string(n.b.token) {
		n.mu.Lock()
		at, replayed := n.used[string(first)]
		now := time.Now()
		if n.b.now != nil {
			now = n.b.now()
		}
		fresh := !replayed || now.Sub(at) < 0
		if replayed && (n.b.window == 0 || now.Sub(at) < n.b.window) {
			fresh = false
		}
		if !replayed {
			n.used[string(first)] = now
		}
		n.mu.Unlock()
		if !replayed {
			_, _ = c.Write([]byte("\x16\x03\x03\x00\x05OWNED")) // the node's own answer to a valid token
			return
		}
		_ = fresh // a replay, in the window or late: the cover below
	}
	if n.b.own != nil {
		if r := n.b.own(first); r != nil {
			_, _ = c.Write(r)
			return
		}
	}
	up, err := net.DialTimeout("tcp", cover, 2*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	_, _ = up.Write(first)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

// recordClientHello runs a crypto/tls handshake to addr and returns the first client record.
func recordClientHello(t *testing.T, addr, sni string) []byte {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	rec := &capture{Conn: raw}
	_ = rec.SetDeadline(time.Now().Add(3 * time.Second))
	_ = tls.Client(rec, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake()
	_ = raw.Close()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.GreaterOrEqual(t, len(rec.out), 5)
	n := 5 + int(rec.out[3])<<8 + int(rec.out[4])
	return append([]byte(nil), rec.out[:n]...)
}

type capture struct {
	net.Conn
	mu  sync.Mutex
	out []byte
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.out = append(c.out, p...)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func target(n *node, cv *lab) Target {
	return Target{Name: "ordinary", Network: "tcp", Addr: n.addr, Cover: cv.coverAddr, SNI: "cover.lab"}
}

func byDetector(r Report, id string) []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Detector == id {
			out = append(out, o)
		}
	}
	return out
}

// ---------------------------------------------------------------------------- the runs

func TestAHonestNodeAnswersLikeItsCoverEverywhere(t *testing.T) {
	cv := newCover(t)
	first := recordClientHello(t, cv.coverAddr, "cover.lab")
	n := newNode(t, cv.coverAddr, nodeBehaviour{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rep := Run(ctx, target(n, cv), Inputs{FirstFlight: first, Window: time.Hour, Wait: func(time.Duration) {}, Listen: 600 * time.Millisecond})

	assert.Equal(t, 0, rep.OwnAnswers(), "outcomes: %+v", rep.Outcomes)
	assert.Len(t, byDetector(rep, "D11"), 3, "at once, after the window, from another address")
	assert.GreaterOrEqual(t, len(byDetector(rep, "D12")), 5)
	assert.GreaterOrEqual(t, len(byDetector(rep, "D13")), 3)
	assert.Len(t, byDetector(rep, "D14"), 3, "16, 64 and 256 bytes")
	for _, o := range rep.Outcomes {
		if o.Skipped != "" {
			continue
		}
		assert.NotEqual(t, Answer{}, o.Cover, "%s/%s has an answer to compare against", o.Detector, o.Case)
	}
}

func TestANodeThatAnswersGarbageOnItsOwnIsCaught(t *testing.T) {
	cv := newCover(t)
	first := recordClientHello(t, cv.coverAddr, "cover.lab")
	leaky := newNode(t, cv.coverAddr, nodeBehaviour{own: func(first []byte) []byte {
		if len(first) < 5 || first[0] != 0x16 { // anything that is not a TLS record: its own alert
			return []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x46}
		}
		return nil
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep := Run(ctx, target(leaky, cv), Inputs{FirstFlight: first, Window: time.Hour, Wait: func(time.Duration) {}, Listen: 600 * time.Millisecond})

	assert.Greater(t, rep.OwnAnswers(), 0)
	own := 0
	for _, o := range byDetector(rep, "D12") {
		if o.Own {
			own++
			assert.Equal(t, "alert", o.Node.Class, o.Case)
		}
	}
	assert.Greater(t, own, 0, "the malformed probe is what finds it")
}

func TestReplayReachesTheCoverBeforeAndAfterTheWindow(t *testing.T) {
	cv := newCover(t)
	legit := recordClientHello(t, cv.coverAddr, "cover.lab")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	n := newNode(t, cv.coverAddr, nodeBehaviour{token: legit, now: now, window: time.Minute})

	// the legitimate client arrives first and is answered on its own
	c, err := net.Dial("tcp", n.addr)
	require.NoError(t, err)
	_, _ = c.Write(legit)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 16)
	k, _ := c.Read(got)
	_ = c.Close()
	require.Contains(t, string(got[:k]), "OWNED")

	waited := time.Duration(0)
	in := Inputs{FirstFlight: legit, Window: 10 * time.Minute, Listen: 600 * time.Millisecond, Wait: func(d time.Duration) {
		waited += d
		clock.Add(int64(d))
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outs := Replay(ctx, target(n, cv), in)

	require.Len(t, outs, 3)
	assert.GreaterOrEqual(t, waited, 10*time.Minute, "the late replay waited for the window")
	for _, o := range outs {
		assert.False(t, o.Own, "%s: node %+v, cover %+v", o.Case, o.Node, o.Cover)
	}
	assert.Equal(t, "other address", outs[2].Case)
	assert.NotEmpty(t, outs[2].Skipped, "no second source address is configured, and the report says so")
}

func TestReplayFromAnotherAddressUsesTheAltDialer(t *testing.T) {
	cv := newCover(t)
	legit := recordClientHello(t, cv.coverAddr, "cover.lab")
	n := newNode(t, cv.coverAddr, nodeBehaviour{})
	var used atomic.Int64
	tg := target(n, cv)
	tg.AltDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		used.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	outs := Replay(context.Background(), tg, Inputs{FirstFlight: legit, Window: time.Second, Wait: func(time.Duration) {}, Listen: 500 * time.Millisecond})
	require.Len(t, outs, 3)
	assert.Empty(t, outs[2].Skipped)
	assert.Greater(t, used.Load(), int64(0))
}

func TestCoverCheckComparesCertificateAlpnAndPage(t *testing.T) {
	cv := newCover(t)
	good := newNode(t, cv.coverAddr, nodeBehaviour{})
	outs := CoverCheck(context.Background(), target(good, cv), Inputs{Listen: time.Second})
	require.GreaterOrEqual(t, len(outs), 3)
	cases := map[string]bool{}
	for _, o := range outs {
		cases[o.Case] = true
		assert.False(t, o.Own, "%s: %+v vs %+v", o.Case, o.Node, o.Cover)
	}
	for _, c := range []string{"certificate", "alpn", "page"} {
		assert.True(t, cases[c], c)
	}

	// a node with a certificate of its own, in front of the same page
	other := newCover(t)
	imposter := newNode(t, other.coverAddr, nodeBehaviour{})
	tg := target(imposter, cv)
	outs = CoverCheck(context.Background(), tg, Inputs{Listen: time.Second})
	diff := false
	for _, o := range outs {
		if o.Case == "certificate" && o.Own {
			diff = true
		}
	}
	assert.True(t, diff, "a different certificate is a different site")
}

func TestPostSuspicionBehavesLikeTheCoverAfterAFirstBadFlight(t *testing.T) {
	cv := newCover(t)
	good := newNode(t, cv.coverAddr, nodeBehaviour{})
	outs := PostSuspicion(context.Background(), target(good, cv), Inputs{Listen: 500 * time.Millisecond})
	require.Len(t, outs, 3)
	for _, o := range outs {
		assert.False(t, o.Own, "%s: %+v vs %+v", o.Case, o.Node, o.Cover)
	}

	// a node that, once it has been poked, clamps down (closes instead of passing on)
	var pokes atomic.Int64
	hardening := newNode(t, cv.coverAddr, nodeBehaviour{own: func(first []byte) []byte {
		if pokes.Add(1) > 2 {
			return []byte("go away")
		}
		return nil
	}})
	outs = PostSuspicion(context.Background(), target(hardening, cv), Inputs{Listen: 500 * time.Millisecond})
	own := 0
	for _, o := range outs {
		if o.Own {
			own++
		}
	}
	assert.Greater(t, own, 0)
}

func TestTokenProbeExpectsAnAnswerForAValidTokenAndTheCoverForAForeignOne(t *testing.T) {
	cv := newCover(t)
	valid := recordClientHello(t, cv.coverAddr, "cover.lab")
	foreign := recordClientHello(t, cv.coverAddr, "cover.lab")
	n := newNode(t, cv.coverAddr, nodeBehaviour{token: valid})
	outs := TokenProbe(context.Background(), target(n, cv), Inputs{Valid: valid, Foreign: foreign, Listen: 600 * time.Millisecond})
	require.Len(t, outs, 2)
	assert.Equal(t, "valid token", outs[0].Case)
	assert.True(t, outs[0].Own)
	assert.True(t, outs[0].Expected, "a valid token is meant to be answered")
	assert.Equal(t, "foreign token", outs[1].Case)
	assert.False(t, outs[1].Own)

	rep := Report{Outcomes: outs}
	assert.Equal(t, 0, rep.OwnAnswers())

	// a node that answers a foreign token on its own breaks the rule
	talker := newNode(t, cv.coverAddr, nodeBehaviour{token: valid, own: func(first []byte) []byte { return []byte("\x16\x03\x03\x00\x05OWNED") }})
	outs = TokenProbe(context.Background(), target(talker, cv), Inputs{Valid: valid, Foreign: foreign, Listen: 600 * time.Millisecond})
	assert.True(t, outs[1].Own)
	assert.Equal(t, 1, Report{Outcomes: outs}.OwnAnswers())
}

func TestProbesWorkOverUDP(t *testing.T) {
	// a node on UDP that stays silent for everything unauthenticated, like noise3
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	tg := Target{Name: "hidden", Network: "udp", Addr: pc.LocalAddr().String(), Cover: pc.LocalAddr().String()}
	outs := Malformed(context.Background(), tg, Inputs{Listen: 300 * time.Millisecond})
	require.NotEmpty(t, outs)
	for _, o := range outs {
		assert.Equal(t, "silence", o.Node.Class, o.Case)
		assert.False(t, o.Own)
	}

	// and one that answers a long-header probe with a Version Negotiation of its own
	vn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = vn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			_, from, err := vn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = vn.WriteTo([]byte{0x80, 0, 0, 0, 0, 8, 1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 1}, from)
		}
	}()
	tg = Target{Name: "hidden", Network: "udp", Addr: vn.LocalAddr().String(), Cover: pc.LocalAddr().String()}
	outs = Malformed(context.Background(), tg, Inputs{Listen: 300 * time.Millisecond})
	own := 0
	for _, o := range outs {
		if o.Own {
			own++
			assert.Equal(t, "datagram", o.Node.Class)
		}
	}
	assert.Greater(t, own, 0, "an answer to a malformed Initial is a fingerprint")
}

func TestRunnerContinuesWhenTheNodeIsUnreachable(t *testing.T) {
	cv := newCover(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := ln.Addr().String()
	_ = ln.Close()
	rep := Run(context.Background(), Target{Name: "dead", Network: "tcp", Addr: dead, Cover: cv.coverAddr, SNI: "cover.lab"}, Inputs{Listen: 200 * time.Millisecond})
	require.NotEmpty(t, rep.Outcomes)
	for _, o := range rep.Outcomes {
		assert.False(t, strings.Contains(o.Node.Detail, "panic"))
	}
}
