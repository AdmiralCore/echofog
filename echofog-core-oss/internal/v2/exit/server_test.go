package exit

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/session"
)

var exitServices = []handshake.ServiceOffer{{ID: 6, Versions: []uint32{2}}}
var exitSuites = []uint16{handshake.SuiteXK, handshake.SuiteXX}

// testClock is the injected clock (AD13).
type testClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    uint64
	timers map[uint64]*testTimer
}

type testTimer struct {
	c    *testClock
	id   uint64
	at   time.Time
	f    func()
	dead bool
}

func newTestClock() *testClock {
	return &testClock{now: time.Unix(1_800_000_000, 0), timers: map[uint64]*testTimer{}}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &testTimer{c: c, id: c.seq, at: c.now.Add(d), f: f}
	c.timers[t.id] = t
	return t
}

func (t *testTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.dead {
		return false
	}
	t.dead = true
	delete(t.c.timers, t.id)
	return true
}

// Advance moves the clock and fires the timers that became due.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*testTimer
	for _, t := range c.timers {
		if !t.dead && !t.at.After(c.now) {
			due = append(due, t)
		}
	}
	for _, t := range due {
		t.dead = true
		delete(c.timers, t.id)
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

type tnode struct {
	t       *testing.T
	account *identity.Account
	device  *identity.Device
	devices []byte
	now     time.Time
}

func newTNode(t *testing.T) *tnode {
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatalf("identity.NewThrowaway: %v", err)
	}
	t.Cleanup(tw.Destroy)
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		t.Fatalf("DeviceList: %v", err)
	}
	return &tnode{t: t, account: tw.Account, device: tw.Device, devices: devices, now: time.Now()}
}

func (n *tnode) card(scope objects.Scope, seq uint64) []byte {
	n.t.Helper()
	body := &pb.NodeCard{ProtoVer: handshake.ProtoVer, HsKey: n.device.Handshake().Public(),
		L1Suites: []uint32{uint32(handshake.SuiteXK), uint32(handshake.SuiteXX)},
		Account:  n.account.Public(), Devices: n.devices, Scope: uint32(scope)}
	var env []byte
	err := n.device.WithSigner(func(s *keys.Signer) error {
		var err error
		env, err = objects.SealCard(s, seq, uint64(n.now.Add(24*time.Hour).Unix()), body)
		return err
	})
	if err != nil {
		n.t.Fatalf("SealCard: %v", err)
	}
	return env
}

func (n *tnode) own() (handshake.OwnCards, []byte) {
	peer := n.card(objects.ScopePeer, 10)
	return handshake.OwnCards{
		Current: map[uint32]handshake.OwnCard{uint32(objects.ScopePeer): {Seq: 10, Envelope: peer}},
		Issued:  map[uint64]handshake.IssuedCard{10: {Scope: uint32(objects.ScopePeer), At: n.now}},
	}, peer
}

func (n *tnode) local(withOwn bool) session.LocalIdentity {
	own, _ := n.own()
	l := session.LocalIdentity{Device: n.device, Card: n.card(objects.ScopePeer, 7), Suites: exitSuites,
		Features: []uint32{uint32(frame.FeaturePathMigration)}, Services: exitServices}
	if withOwn {
		l.OwnCards = own
	}
	return l
}

func (n *tnode) verified() *objects.Card {
	_, peer := n.own()
	c, err := objects.VerifyCard(peer, n.now)
	if err != nil {
		n.t.Fatalf("VerifyCard: %v", err)
	}
	return c
}

type oneListener struct {
	c    mux.Carrier
	mu   sync.Mutex
	used bool
}

func (l *oneListener) Accept(ctx context.Context) (mux.Carrier, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	l.used = true
	return l.c, nil
}
func (l *oneListener) Addr() net.Addr { return nil }
func (l *oneListener) Close() error   { return nil }

var _ carrier.Listener = (*oneListener)(nil)

type dgram struct {
	Type    uint64
	Payload []byte
}

// rig is one exit server with one authenticated client.
type rig struct {
	t      *testing.T
	ctx    context.Context
	srv    *Server
	client *mux.Session
	node   [32]byte
	dgrams chan dgram
	clk    *testClock
}

type rigConfig struct {
	ServerConfig
	NoDatagrams bool
}

func newRig(t *testing.T, rc rigConfig) *rig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	clk := newTestClock()
	if rc.ServerConfig.Clock == nil {
		rc.ServerConfig.Clock = clk
	}
	srv, err := NewServer(rc.ServerConfig)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	server := newTNode(t)
	client := newTNode(t)
	pair := memcarrier.NewBytes()

	_, serverCardEnv := server.own()
	serverCard, err := objects.VerifyCard(serverCardEnv, server.now)
	if err != nil {
		t.Fatal(err)
	}
	dialed := make(chan *session.Established, 1)
	go func() {
		est, err := session.Dial(ctx, serverCard, carrier.Attempt{Kind: carrier.AttemptCarrier},
			client.local(false), session.WithCarrier(pair.Client))
		if err != nil {
			t.Errorf("dial: %v", err)
		}
		dialed <- est
	}()
	serverEst, err := session.Accept(ctx, &oneListener{c: pair.Server},
		session.AcceptConfig{Local: server.local(true)})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	clientEst := <-dialed
	if clientEst == nil {
		t.Fatal("client session not established")
	}
	t.Cleanup(func() { _ = clientEst.Session.Close(); _ = serverEst.Session.Close() })

	peer := Peer{Node: [32]byte(serverEst.Peer.NodeID), Device: [32]byte(serverEst.Peer.DeviceID),
		SID: serverEst.SID, Remote: serverEst.Remote}
	if !rc.NoDatagrams {
		peer.SendDatagram = serverEst.Session.SendDatagram
	}

	r := &rig{t: t, ctx: ctx, srv: srv, client: clientEst.Session, node: peer.Node,
		dgrams: make(chan dgram, 64), clk: clk}

	go func() {
		for {
			st, err := serverEst.Session.AcceptStream(ctx)
			if err != nil {
				return
			}
			if st.Open().Service != 6 {
				st.Reset(frame.CodeProtocolViolation)
				continue
			}
			go srv.Handle(ctx, peer, st)
		}
	}()
	go func() {
		for {
			d, err := serverEst.Session.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if d.Type == frame.DgramExitUDP {
				srv.HandleDatagram(peer, d.Payload)
			}
		}
	}()
	go func() {
		for {
			d, err := clientEst.Session.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			select {
			case r.dgrams <- dgram{Type: d.Type, Payload: append([]byte(nil), d.Payload...)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return r
}

func (r *rig) open(t *testing.T) *mux.Stream {
	t.Helper()
	st, err := r.client.OpenStream(r.ctx, 6, 2)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	return st
}

type frameResult struct {
	f   frame.Frame
	err error
}

func (r *rig) read(t *testing.T, st *mux.Stream) frame.Frame {
	t.Helper()
	ch := make(chan frameResult, 1)
	go func() {
		f, err := st.ReadFrame()
		ch <- frameResult{f, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("ReadFrame: %v", res.err)
		}
		return res.f
	case <-time.After(3 * time.Second):
		t.Fatal("ReadFrame timed out")
	}
	return frame.Frame{}
}

func (r *rig) connect(t *testing.T, host string, port uint32) (*mux.Stream, Result) {
	t.Helper()
	st := r.open(t)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteFrame(TypeTCPConnect, 0, TCPConnect{Host: host, Port: port}.Append(nil)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	f := r.read(t, st)
	if f.Type != TypeResult {
		t.Fatalf("frame type %#x, want RESULT", f.Type)
	}
	res, err := ParseResult(f.Payload)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	return st, res
}

func TestSSRFRefused(t *testing.T) {
	localV6 := localV6Sibling(t)
	rig := newRig(t, rigConfig{ServerConfig: ServerConfig{
		Resolver: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		},
		SelfIPs: func() []net.IP { return []net.IP{net.ParseIP("8.8.8.8")} },
	}})

	cases := []struct {
		name string
		host string
		port uint32
		want uint32
	}{
		{"loopback", "127.0.0.1", 80, ResultDenied},
		{"private", "10.0.0.1", 80, ResultDenied},
		{"link-local", "169.254.1.1", 80, ResultDenied},
		{"dns rebinding to loopback", "rebind.test", 80, ResultDenied},
		{"own public IP", "8.8.8.8", 80, ResultDenied},
		{"smtp port", "1.1.1.1", 25, ResultDenied},
		{"ipv6 loopback", "::1", 80, ResultDenied},
	}
	if localV6 != "" {
		cases = append(cases, struct {
			name string
			host string
			port uint32
			want uint32
		}{"ipv6 on-link /64", localV6, 80, ResultDenied})
	} else {
		t.Log("no local IPv6 /64 found; the on-link case is covered by the link-local case")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, res := rig.connect(t, tc.host, tc.port)
			if res.Code != tc.want {
				t.Fatalf("%s:%d = %d, want %d", tc.host, tc.port, res.Code, tc.want)
			}
		})
	}
}

// localV6Sibling returns "[addr]:port" for an address inside a local /64 but not the interface's
// own address. It proves the exit never dials the operator's on-link neighbours, including the
// global address of a home IPv6 LAN (core's localNets rule).
func localV6Sibling(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var fallback string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		if ip.To4() != nil || ip.To16() == nil {
			continue
		}
		ones, bits := ipnet.Mask.Size()
		if bits != 128 || ones != 64 {
			continue
		}
		b := append(net.IP(nil), ipnet.IP.Mask(ipnet.Mask)...)
		for i := 8; i < 16; i++ {
			b[i] = 0xaa
		}
		if b.Equal(ip) {
			b[15] ^= 0x01
		}
		host := net.JoinHostPort(b.String(), "80")
		if !ip.IsLinkLocalUnicast() && !ip.IsPrivate() {
			return host
		}
		if fallback == "" {
			fallback = host
		}
	}
	return fallback
}

func TestTCPRelayFastOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		_, _ = c.Write(append([]byte("echo:"), buf[:n]...))
	}()

	rig := newRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})
	st := rig.open(t)
	defer st.Close()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := mustAtoi(t, portStr)
	if err := st.WriteFrame(TypeTCPConnect, 0, TCPConnect{Host: host, Port: uint32(port)}.Append(nil)); err != nil {
		t.Fatal(err)
	}
	// Fast open: DATA follows TCP_CONNECT before RESULT.
	if err := st.WriteFrame(TypeData, 0, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	f := rig.read(t, st)
	if f.Type != TypeResult {
		t.Fatalf("type %#x, want RESULT", f.Type)
	}
	res, _ := ParseResult(f.Payload)
	if res.Code != ResultOK {
		t.Fatalf("RESULT = %d, want ok", res.Code)
	}
	f = rig.read(t, st)
	if f.Type != TypeData || string(f.Payload) != "echo:hi" {
		t.Fatalf("reply = %#x %q", f.Type, f.Payload)
	}
}

func TestTCPUnreachable(t *testing.T) {
	// A port that is closed: refused and timed out alike are unreachable (D19).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	host, portStr, _ := net.SplitHostPort(addr)

	rig := newRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})
	_, res := rig.connect(t, host, uint32(mustAtoi(t, portStr)))
	if res.Code != ResultUnreachable {
		t.Fatalf("closed port = %d, want unreachable (2)", res.Code)
	}
}

func TestTCPRateLimit(t *testing.T) {
	rig := newRig(t, rigConfig{ServerConfig: ServerConfig{Limits: Limits{TCPPerSecond: 2}}})
	for i := 0; i < 2; i++ {
		_, res := rig.connect(t, "127.0.0.1", 80)
		if res.Code != ResultDenied {
			t.Fatalf("connection %d = %d, want denied", i+1, res.Code)
		}
	}
	_, res := rig.connect(t, "127.0.0.1", 80)
	if res.Code != ResultRateLimited {
		t.Fatalf("third connection = %d, want rate-limited (7)", res.Code)
	}
	// Another identity is not affected by this one's counter (unit-covered above).
	rig.clk.Advance(time.Second)
	_, res = rig.connect(t, "127.0.0.1", 80)
	if res.Code != ResultDenied {
		t.Fatalf("after refill = %d, want denied", res.Code)
	}
}

func TestAccountGet(t *testing.T) {
	rig := newRig(t, rigConfig{})
	st := rig.open(t)
	defer st.Close()
	if err := st.WriteFrame(TypeAccountGet, 0, nil); err != nil {
		t.Fatal(err)
	}
	f := rig.read(t, st)
	if f.Type != TypeAccount {
		t.Fatalf("type %#x, want ACCOUNT", f.Type)
	}
	a, err := ParseAccount(f.Payload)
	if err != nil {
		t.Fatalf("ParseAccount: %v", err)
	}
	if a.Price != 0 {
		t.Fatalf("price = %d, want 0 for a free exit", a.Price)
	}
}

func TestUDPRelayDatagrams(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	host, portStr, _ := net.SplitHostPort(pc.LocalAddr().String())
	target := net.JoinHostPort(host, portStr)
	_ = target

	rig := newRig(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}})
	st := rig.open(t)
	defer st.Close()
	if err := st.WriteFrame(TypeUDPOpen, 0, UDPOpen{Req: 1}.Append(nil)); err != nil {
		t.Fatal(err)
	}
	f := rig.read(t, st)
	if f.Type != TypeUDPOK {
		t.Fatalf("type %#x, want UDP_OK", f.Type)
	}
	ok, err := ParseUDPOK(f.Payload)
	if err != nil {
		t.Fatalf("ParseUDPOK: %v", err)
	}
	if ok.Code != ResultOK || ok.Session == 0 {
		t.Fatalf("UDP_OK = %+v, want ok with a datagram session", ok)
	}

	// Fragments of the reply may arrive out of order and in any count from 1 to 8.
	payload := bytes.Repeat([]byte("udp!"), 800) // 3200 bytes
	frags := splitForTest(ok.Session, 1, net.JoinHostPort(host, portStr), payload)
	for i := len(frags) - 1; i >= 0; i-- {
		if err := rig.client.SendDatagram(frame.DgramExitUDP, frags[i].Append(nil)); err != nil {
			t.Fatal(err)
		}
	}

	got := make(map[uint8][]byte)
	count := uint8(0)
	var addr string
	deadline := time.After(5 * time.Second)
	for count == 0 || len(got) < int(count) {
		select {
		case d := <-rig.dgrams:
			if d.Type != frame.DgramExitUDP {
				continue
			}
			fr, err := ParseFragment(d.Payload)
			if err != nil {
				t.Fatalf("ParseFragment: %v", err)
			}
			if fr.Session != uint64(ok.Session) || fr.Packet != 1 {
				t.Fatalf("fragment session %d packet %d", fr.Session, fr.Packet)
			}
			if count == 0 {
				count = fr.Count
			} else if fr.Count != count {
				t.Fatalf("fragment count changed from %d to %d", count, fr.Count)
			}
			got[fr.Index] = fr.Data
			if fr.Index == 0 {
				addr = fr.Addr
			}
		case <-deadline:
			t.Fatalf("got %d/%d fragments", len(got), count)
		}
	}
	if addr != net.JoinHostPort(host, portStr) {
		// The exit labels replies with the address the client used.
		t.Fatalf("addr = %q", addr)
	}
	reassembled := make([]byte, 0, len(payload))
	for i := uint8(0); i < count; i++ {
		part, ok := got[i]
		if !ok {
			t.Fatalf("missing fragment %d", i)
		}
		reassembled = append(reassembled, part...)
	}
	if !bytes.Equal(reassembled, payload) {
		t.Fatalf("reassembled payload mismatch: %d bytes", len(reassembled))
	}
}

func TestUDPStreamFallback(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		_, _ = pc.WriteTo(buf[:n], addr)
	}()
	host, portStr, _ := net.SplitHostPort(pc.LocalAddr().String())

	rig := newRig(t, rigConfig{NoDatagrams: true,
		ServerConfig: ServerConfig{AllowPrivateTargets: true}})
	st := rig.open(t)
	defer st.Close()
	if err := st.WriteFrame(TypeUDPOpen, 0, UDPOpen{Req: 3}.Append(nil)); err != nil {
		t.Fatal(err)
	}
	f := rig.read(t, st)
	ok, err := ParseUDPOK(f.Payload)
	if err != nil || ok.Code != ResultOK || ok.Session != 0 {
		t.Fatalf("UDP_OK = %+v, %v; want stream fallback", ok, err)
	}
	pkt := UDPPkt{Addr: net.JoinHostPort(host, portStr), Data: []byte("ping")}
	if err := st.WriteFrame(TypeUDPPkt, 0, pkt.Append(nil)); err != nil {
		t.Fatal(err)
	}
	f = rig.read(t, st)
	if f.Type != TypeUDPPkt {
		t.Fatalf("type %#x, want UDP_PKT", f.Type)
	}
	reply, err := ParseUDPPkt(f.Payload)
	if err != nil || string(reply.Data) != "ping" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
}

// splitForTest fragments a UDP payload the way the exit does, for the client side of the test.
func splitForTest(session uint32, packet uint16, addr string, payload []byte) []Fragment {
	const maxFragment = 1200
	overhead := 5 + 2 + 1 + 1 + 1 + len(addr)
	chunk := maxFragment - overhead
	if chunk < 1 {
		chunk = 1
	}
	count := (len(payload) + chunk - 1) / chunk
	if count == 0 {
		count = 1
	}
	out := make([]Fragment, 0, count)
	for i := 0; i < count; i++ {
		end := min((i+1)*chunk, len(payload))
		start := min(i*chunk, len(payload))
		f := Fragment{Session: uint64(session), Packet: packet, Index: uint8(i), Count: uint8(count)}
		if i == 0 {
			f.Addr = addr
		}
		f.Data = payload[start:end]
		out = append(out, f)
	}
	return out
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("bad port %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}
