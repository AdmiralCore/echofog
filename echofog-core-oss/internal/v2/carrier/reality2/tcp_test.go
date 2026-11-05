package reality2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/carriertest"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

const coverBanner = "the real cover answers"

// tcpCover is a TLS server standing for the cover site: it presents its own certificate, says one
// line and counts the connections it saw.
type tcpCover struct {
	addr  string
	cert  tls.Certificate
	conns atomic.Int64
}

func newTCPCover(t *testing.T) *tcpCover {
	t.Helper()
	cv := &tcpCover{cert: testCertificate(t)}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cv.cert}, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	cv.addr = ln.Addr().String()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			cv.conns.Add(1)
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Write([]byte(coverBanner))
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return cv
}

type tcpRig struct {
	cover *tcpCover
	ln    *TCPListener
	card  *objects.Card
	addr  *pb.Addr
	mask  *ecdh.PrivateKey
}

type rigOpt func(*TCPListenerConfig)

func newTCPRig(t *testing.T, opts ...rigOpt) *tcpRig {
	t.Helper()
	cv := newTCPCover(t)
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(raw.Addr().(*net.TCPAddr).Port)
	own := testCertificate(t)
	cfg := TCPListenerConfig{
		Cover:     TCPCover{Group: tls.X25519MLKEM768, ALPN: "h2", Cert: &own},
		Masks:     []MaskKey{{Private: sk}},
		Addrs:     []string{AddressString("127.0.0.1", port)},
		CoverAddr: cv.addr,
	}
	for _, o := range opts {
		o(&cfg)
	}
	ln, err := ListenTCP(raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	params, err := objects.Marshal(&pb.SuiteParams{Sni: "cover.example"})
	if err != nil {
		t.Fatal(err)
	}
	return &tcpRig{
		cover: cv, ln: ln, mask: sk, card: testCard(t, sk),
		addr: &pb.Addr{Suite: objects.SuiteReality2TCP, Host: "127.0.0.1", Port: port, Mask: 1, Params: params},
	}
}

func (r *tcpRig) dial(ctx context.Context) (mux.Carrier, error) {
	return NewTCPDialer(TCPDialerConfig{}).Dial(ctx, carrier.DialRequest{Card: r.card, Addr: r.addr})
}

func (r *tcpRig) pair(t *testing.T) (client, server mux.Carrier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	type res struct {
		c   mux.Carrier
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := r.ln.Accept(ctx)
		ch <- res{c, err}
	}()
	c, err := r.dial(ctx)
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

// TestTCPCarrierConformance runs the shared carrier suite over a real TCP and TLS pair.
func TestTCPCarrierConformance(t *testing.T) {
	carriertest.Run(t, func(t *testing.T) (mux.Carrier, mux.Carrier) {
		return newTCPRig(t).pair(t)
	})
}

// TestTCPCarrierSealedByGroup: the negotiated group decides sealed mode, on both sides
// (wire.md §8.5). The cover's classical group is mirrored, so the path is sealed.
func TestTCPCarrierSealedByGroup(t *testing.T) {
	for _, tc := range []struct {
		group  tls.CurveID
		sealed bool
	}{{tls.X25519MLKEM768, false}, {tls.X25519, true}} {
		own := testCertificate(t)
		r := newTCPRig(t, func(c *TCPListenerConfig) { c.Cover = TCPCover{Group: tc.group, ALPN: "h2", Cert: &own} })
		c, s := r.pair(t)
		if c.Sealed() != tc.sealed || s.Sealed() != tc.sealed {
			t.Errorf("group %v: sealed client %v server %v, want %v", tc.group, c.Sealed(), s.Sealed(), tc.sealed)
		}
		if c.ChannelBinding() != s.ChannelBinding() || c.ChannelBinding() == ([32]byte{}) {
			t.Errorf("group %v: channel bindings differ or are empty", tc.group)
		}
	}
}

// TestTCPStrangerGetsTheCover: a client without a token completes a TLS handshake with the cover's
// own certificate, through the proxy, which proves the bytes already read were forwarded.
func TestTCPStrangerGetsTheCover(t *testing.T) {
	r := newTCPRig(t)
	conn, err := tls.Dial("tcp", r.ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, ServerName: "cover.example", NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatalf("stranger handshake: %v", err)
	}
	defer conn.Close()
	st := conn.ConnectionState()
	if !bytes.Equal(st.PeerCertificates[0].Raw, r.cover.cert.Certificate[0]) {
		t.Fatal("a stranger saw a certificate that is not the cover's")
	}
	buf := make([]byte, len(coverBanner))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != coverBanner {
		t.Fatalf("cover banner %q, %v", buf, err)
	}
}

// TestTCPGarbageAndShortFirstRecordGoToTheCover: whatever is not a token hello is proxied with
// every byte it sent, including a truncated record that the node cannot parse.
func TestTCPGarbageAndShortFirstRecordGoToTheCover(t *testing.T) {
	for name, payload := range map[string][]byte{
		"not tls":      []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		"short record": {0x16, 0x03, 0x01, 0x00, 0x10, 0x01, 0x02},
		"huge record":  {0x16, 0x03, 0x01, 0xff, 0xff, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			got := make(chan []byte, 1)
			cover, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer cover.Close()
			go func() {
				c, err := cover.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				b := make([]byte, len(payload))
				_, _ = io.ReadFull(c, b)
				got <- b
			}()
			r := newTCPRig(t, func(c *TCPListenerConfig) {
				c.CoverAddr = cover.Addr().String()
				c.FirstRecordTimeout = 400 * time.Millisecond
			})
			c, err := net.Dial("tcp", r.ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, _ = c.Write(payload)
			select {
			case b := <-got:
				if !bytes.Equal(b, payload) {
					t.Fatalf("cover got %q, want %q", b, payload)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the cover received nothing")
			}
		})
	}
}

// recordingDial wraps the client's socket and keeps the bytes it sent.
type recordingConn struct {
	net.Conn
	mu  sync.Mutex
	out []byte
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.out = append(c.out, p...)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

// TestTCPReplayedHelloGoesToTheCover: the first record of an admitted client, sent again from
// another socket, is a replay: it is not admitted and the sender gets the cover (wire.md §4.2).
func TestTCPReplayedHelloGoesToTheCover(t *testing.T) {
	r := newTCPRig(t)
	rec := &recordingConn{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accepted := make(chan mux.Carrier, 2)
	go func() {
		for {
			c, err := r.ln.Accept(ctx)
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	c, err := NewTCPDialer(TCPDialerConfig{}).Dial(ctx, carrier.DialRequest{
		Card: r.card, Addr: r.addr,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			rec.Conn = raw
			return rec, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := <-accepted
	defer s.Close()

	rec.mu.Lock()
	first := append([]byte(nil), rec.out...)
	rec.mu.Unlock()
	if len(first) < 5 || first[0] != 0x16 {
		t.Fatalf("recorded %d bytes, not a TLS record", len(first))
	}
	rl := 5 + int(first[3])<<8 + int(first[4])
	conn, err := net.Dial("tcp", r.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write(first[:rl])
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	// The cover answers a ClientHello with its own ServerHello: a TLS handshake record (0x16).
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil || b[0] != 0x16 {
		t.Fatalf("replay got %x, %v; want the cover's handshake record", b, err)
	}
	select {
	case extra := <-accepted:
		extra.Close()
		t.Fatal("a replayed hello was admitted")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestTCPSlowStrangerDoesNotHoldASlot: connections that never send a record are decided after
// the first-record timeout, so the pre-token slots come back and a client with a token is served.
func TestTCPSlowStrangerDoesNotHoldASlot(t *testing.T) {
	r := newTCPRig(t, func(c *TCPListenerConfig) {
		c.MaxPreToken = 2
		c.FirstRecordTimeout = 400 * time.Millisecond
	})
	var silent []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", r.ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		silent = append(silent, c)
	}
	defer func() {
		for _, c := range silent {
			c.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // both silent connections hold a slot now
	// An excess connection is proxied to the cover without being read: it gets the cover's
	// handshake bytes only after it speaks, but the cover has seen the TCP connection.
	before := r.cover.conns.Load()
	extra, err := net.Dial("tcp", r.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	deadline := time.Now().Add(2 * time.Second)
	for r.cover.conns.Load() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.cover.conns.Load() == before {
		t.Fatal("the excess connection did not reach the cover without being read")
	}
	time.Sleep(600 * time.Millisecond) // the first-record timeout has passed: the slots are free
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		s, err := r.ln.Accept(ctx)
		if err == nil {
			s.Close()
		}
		done <- err
	}()
	c, err := r.dial(ctx)
	if err != nil {
		t.Fatalf("a client with a token after the timeout: %v", err)
	}
	c.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestTCPPerAddressLimitOnlyAfterToken: the limit holds admitted connections of one address, and
// strangers are never counted against it.
func TestTCPPerAddressLimitOnlyAfterToken(t *testing.T) {
	r := newTCPRig(t, func(c *TCPListenerConfig) { c.MaxPerAddr = 1 })
	c1, s1 := r.pair(t)
	// A second authenticated connection from the same address is refused ...
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c2, err := r.dial(ctx); err == nil {
		c2.Close()
		t.Fatal("a second admitted connection from one address was served")
	}
	// ... but a stranger from the same address still reaches the cover.
	conn, err := tls.Dial("tcp", r.ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, ServerName: "cover.example"})
	if err != nil {
		t.Fatalf("stranger refused by a per-address limit: %v", err)
	}
	conn.Close()
	// Closing the first carrier gives the slot back.
	c1.Close()
	s1.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
		go func() {
			if s, err := r.ln.Accept(ctx2); err == nil {
				s.Close()
			}
		}()
		c3, err := r.dial(ctx2)
		cancel2()
		if err == nil {
			c3.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot did not come back: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTCPDialUsesTheProxyHook: the request's DialContext opens the socket (T2.1 hook).
func TestTCPDialUsesTheProxyHook(t *testing.T) {
	r := newTCPRig(t)
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		if s, err := r.ln.Accept(ctx); err == nil {
			s.Close()
		}
	}()
	c, err := NewTCPDialer(TCPDialerConfig{}).Dial(ctx, carrier.DialRequest{
		Card: r.card, Addr: r.addr,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			calls.Add(1)
			if network != "tcp" || addr != AddressString("127.0.0.1", r.addr.Port) {
				t.Errorf("hook got %s %s", network, addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if calls.Load() != 1 {
		t.Fatalf("proxy hook called %d times", calls.Load())
	}
}

func TestTCPDialerRejectsUnusableAddress(t *testing.T) {
	d := NewTCPDialer(TCPDialerConfig{})
	card := testCard(t, mustKey(t))
	for name, a := range map[string]*pb.Addr{
		"quic suite": {Suite: objects.SuiteReality2QUIC, Host: "192.0.2.1", Port: 443, Mask: 1},
		"no mask":    {Suite: objects.SuiteReality2TCP, Host: "192.0.2.1", Port: 443},
		"no sni":     {Suite: objects.SuiteReality2TCP, Host: "192.0.2.1", Port: 443, Mask: 1},
	} {
		if _, err := d.Dial(context.Background(), carrier.DialRequest{Card: card, Addr: a}); !errors.Is(err, ErrCard) {
			t.Errorf("%s: %v, want ErrCard", name, err)
		}
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

func TestTCPListenerRequiresACover(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := ListenTCP(ln, TCPListenerConfig{CoverAddr: "127.0.0.1:443"}); !errors.Is(err, ErrTCPCover) {
		t.Fatalf("no profile: %v", err)
	}
	if _, err := ListenTCP(ln, TCPListenerConfig{Cover: TCPCover{Group: tls.X25519}}); !errors.Is(err, ErrTCPCover) {
		t.Fatalf("no address: %v", err)
	}
}

func TestTCPListenerCloseIsIdempotentAndStopsAccept(t *testing.T) {
	r := newTCPRig(t)
	done := make(chan error, 1)
	go func() { _, err := r.ln.Accept(context.Background()); done <- err }()
	time.Sleep(50 * time.Millisecond)
	if err := r.ln.Close(); err != nil {
		t.Fatal(err)
	}
	_ = r.ln.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept returned a carrier from a closed listener")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
}

func TestRegisterTCP(t *testing.T) {
	reg := carrier.NewRegistry()
	if err := RegisterTCP(reg, NewTCPDialer(TCPDialerConfig{}), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.DialerFor(objects.SuiteReality2TCP); !ok {
		t.Fatal("dialer not registered")
	}
	if err := RegisterTCP(nil, nil, nil); err == nil {
		t.Fatal("nil registry accepted")
	}
}

// ---- padding

type sinkConn struct {
	net.Conn
	writes [][]byte
}

func (s *sinkConn) Write(p []byte) (int, error) {
	s.writes = append(s.writes, append([]byte(nil), p...))
	return len(p), nil
}

func TestPaddingFrameExactLengths(t *testing.T) {
	for n := 3; n <= 600; n++ {
		b, ok := paddingFrame(n)
		if !ok {
			if n != 67 {
				t.Errorf("no padding frame of %d bytes", n)
			}
			continue
		}
		if len(b) != n {
			t.Fatalf("frame of %d bytes asked, got %d", n, len(b))
		}
		f, err := frame.NewReader(bytes.NewReader(b), nil).ReadFrame()
		if err != nil || f.Type != frame.TypePadding {
			t.Fatalf("n=%d: %+v %v", n, f, err)
		}
	}
	if _, ok := paddingFrame(2); ok {
		t.Fatal("a padding frame of 2 bytes cannot exist")
	}
}

// TestPadConnFollowsTheProfile: within the budget each write is rounded up to a bucket and still
// parses as frames; after the budget it passes through unchanged (wire.md §4.3, §9).
func TestPadConnFollowsTheProfile(t *testing.T) {
	prof, err := shape.Builtin(shape.IDDefaultTLS)
	if err != nil {
		t.Fatal(err)
	}
	sink := &sinkConn{}
	pc := &padConn{Conn: sink, sender: shape.NewSender(prof, nil, false)}
	msg := frame.AppendFrame(nil, frame.Frame{Type: frame.TypePing, Payload: make([]byte, 8)})
	padded, plain := 0, 0
	for i := 0; i < 400; i++ {
		if n, err := pc.Write(msg); err != nil || n != len(msg) {
			t.Fatalf("write: %d %v", n, err)
		}
		w := sink.writes[len(sink.writes)-1]
		if len(w) == len(msg) {
			plain++
			continue
		}
		padded++
		if len(w) > maxRecordPlaintext {
			t.Fatalf("padded write of %d bytes does not fit one record", len(w))
		}
		rd := frame.NewReader(bytes.NewReader(w), nil)
		f, err := rd.ReadFrame()
		if err != nil || f.Type != frame.TypePing {
			t.Fatalf("first frame of a padded write: %+v %v", f, err)
		}
		if f, err = rd.ReadFrame(); err != nil || f.Type != frame.TypePadding {
			t.Fatalf("padding frame: %+v %v", f, err)
		}
	}
	if padded == 0 {
		t.Fatal("nothing was padded inside the budget")
	}
	if plain == 0 {
		t.Fatal("padding went on after the budget")
	}
	// The budget is at most 16384 bytes of traffic: 400 writes of 11 bytes pad only at the start.
	last := sink.writes[len(sink.writes)-1]
	if len(last) != len(msg) {
		t.Fatalf("a write after the budget is %d bytes, want %d", len(last), len(msg))
	}
}

// TestSealedPathIsNotPadded: on a sealed path a plaintext PADDING frame after the sealing switch
// would end the path, so the byte layer adds none.
func TestSealedPathIsNotPadded(t *testing.T) {
	prof, _ := shape.Builtin(shape.IDDefaultTLS)
	sink := &sinkConn{}
	if _, ok := any(wrapPad(sink, true, prof)).(*padConn); ok {
		t.Fatal("a sealed path got a padding writer")
	}
	if _, ok := any(wrapPad(sink, false, prof)).(*padConn); !ok {
		t.Fatal("an unsealed path with a profile got no padding writer")
	}
}

// ---- parser robustness

func FuzzReadFirstRecord(f *testing.F) {
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 1, 0, 0, 1, 0})
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	f.Add([]byte{0x16, 0x03, 0x01, 0xff, 0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		a, b := net.Pipe()
		go func() { _, _ = b.Write(data); _ = b.Close() }()
		buf, h := readFirstRecord(a, 200*time.Millisecond)
		_ = a.Close()
		if len(buf) > len(data) {
			t.Fatalf("read %d bytes from %d", len(buf), len(data))
		}
		if !bytes.HasPrefix(data, buf) {
			t.Fatal("the bytes read are not a prefix of the input: the proxy would corrupt the stream")
		}
		if h != nil && (len(h.Token) != TokenLen || len(h.C) != 32) {
			t.Fatalf("hello with token %d bytes, C %d bytes", len(h.Token), len(h.C))
		}
	})
}

// TestTCPSlotHeldUntilTheTokenIsChecked: wire.md §4.2 releases the pre-token slot as soon as the
// token is checked, not before (T2.8 F2).
func TestTCPSlotHeldUntilTheTokenIsChecked(t *testing.T) {
	r := newTCPRig(t, func(c *TCPListenerConfig) { c.MaxPreToken = 1 })
	held := make(chan int, 1)
	r.ln.verifyHook = func() { held <- len(r.ln.slots) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		if s, err := r.ln.Accept(ctx); err == nil {
			s.Close()
		}
	}()
	c, err := r.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case n := <-held:
		if n != 1 {
			t.Fatalf("slots held during the token check: %d, want 1", n)
		}
	case <-time.After(time.Second):
		t.Fatal("the token check did not run the hook")
	}
	// and the slot is free afterwards
	deadline := time.Now().Add(2 * time.Second)
	for len(r.ln.slots) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(r.ln.slots) != 0 {
		t.Fatal("the slot was not released after the check")
	}
}

// TestTCPProxyHasALifetime: a stranger that trickles a byte now and then, or never reads what the
// cover sends, cannot hold a cover connection for ever (T2.8 F3).
func TestTCPProxyHasALifetime(t *testing.T) {
	old := coverMaxLife
	coverMaxLife = 600 * time.Millisecond
	defer func() { coverMaxLife = old }()
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	go func() { // a cover that keeps every connection open and says nothing
		for {
			c, err := hold.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	r := newTCPRig(t, func(c *TCPListenerConfig) { c.CoverAddr = hold.Addr().String() })
	c, err := net.Dial("tcp", r.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	stop := time.After(4 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if _, err := c.Write([]byte{0}); err != nil {
				return // closed by the node
			}
			_ = c.SetReadDeadline(time.Now().Add(time.Millisecond))
			var b [64]byte
			if _, err := c.Read(b[:]); err != nil {
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					return // EOF or reset: the node ended it
				}
			}
		case <-stop:
			t.Fatal("a trickling stranger held the cover connection past its lifetime")
		}
	}
}

// TestTCPStrangerHookReplacesTheCover: with a Stranger hook a listener needs no cover; every
// connection that is not admitted goes to the hook with the bytes it sent.
func TestTCPStrangerHookReplacesTheCover(t *testing.T) {
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	own := testCertificate(t)
	got := make(chan []byte, 1)
	ln, err := ListenTCP(raw, TCPListenerConfig{
		Cover: TCPCover{Group: tls.X25519MLKEM768, ALPN: "h2", Cert: &own},
		Masks: []MaskKey{{Private: sk}},
		Addrs: []string{AddressString("127.0.0.1", 1)},
		Stranger: func(c net.Conn, pre []byte) {
			defer c.Close()
			got <- append([]byte(nil), pre...)
		},
	})
	if err != nil {
		t.Fatalf("ListenTCP without a cover but with a Stranger hook: %v", err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	select {
	case pre := <-got:
		if !bytes.HasPrefix([]byte("GET / HTTP/1.1\r\n\r\n"), pre) || len(pre) == 0 {
			t.Fatalf("hook got %q", pre)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook was not called")
	}
}
