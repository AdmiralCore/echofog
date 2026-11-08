package scenarios

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"sync"
	"time"

	quic "github.com/apernet/quic-go"
	"golang.org/x/crypto/curve25519"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/carrier/ws2"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/probe"
	"github.com/echofog/echofog-core/testbed/probe/r2flight"
)

// The fake addresses the censor sees (TEST-NET-3). The node hosts the cover's name at its own
// address by assumption: that is how a node is deployed (the cover is where the node is), and it is
// what D3 checks the SNI against.
var (
	fakeNoise3 = netip.MustParseAddrPort("203.0.113.11:443")
	fakeQUIC   = netip.MustParseAddrPort("203.0.113.12:443")
	fakeTCP    = netip.MustParseAddrPort("203.0.113.13:443")
	fakeWS2    = netip.MustParseAddrPort("203.0.113.14:443")
	fakeWeb    = netip.MustParseAddrPort("203.0.113.30:443")
)

const (
	coverName = "cover.example"
	cdnName   = "cdn.example"
	webName   = "web.example"
	wsPath    = "/s/4f9c1e7a2b6d8035"
	siteText  = "plain site"
)

// capturedCarrier is what one carrier left: the flows the censor read from the tap and the report
// of the prober against the listener.
type capturedCarrier struct {
	Name       string
	Flows      []censor.Flow
	Probe      probe.Report
	NotChecked []string
	Err        error // a setup or traffic failure: the rows say not measured
}

// advance moves a clock the node and the clients share forward, so a replay "after the window" is
// really after it, and nothing sleeps.
func advance(c *shiftClock) func(time.Duration) {
	return func(d time.Duration) { c.shift.Add(int64(d)) }
}

const probeWindow = 20 * time.Minute

func redirectTo(addr string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

func altDialer(network string) probe.Dialer {
	return func(ctx context.Context, n, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: 3 * time.Second}
		if network == "udp" {
			d.LocalAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2)}
		} else {
			d.LocalAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}
		}
		return d.DialContext(ctx, n, addr)
	}
}

func selfSignedCert(names ...string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

func cardWith(pub []byte) *objects.Card {
	return &objects.Card{Body: &pb.NodeCard{MaskKeys: []*pb.MaskKey{{
		Key: keys.New(keys.X25519, pub), NotAfter: uint64(time.Now().Add(24 * time.Hour).Unix()),
	}}}}
}

// finish closes the tap, ends the pcap and reads the flows.
func finish(out *capturedCarrier, closeTap func() error, wait func() ([]censor.Flow, error)) {
	_ = closeTap()
	flows, err := wait()
	if err != nil && out.Err == nil {
		out.Err = fmt.Errorf("analyse the capture: %w", err)
	}
	out.Flows = flows
}

// ---------------------------------------------------------------------------- noise3

func captureNoise3(ctx context.Context, sessions, bulk int) (out capturedCarrier) {
	out.Name = "noise3"
	pw, wait := newCapture()
	clock := &shiftClock{}
	var priv [32]byte
	_, _ = rand.Read(priv[:])
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		out.Err = err
		return
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		out.Err = err
		return
	}
	profile, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		_ = pc.Close()
		out.Err = err
		return
	}
	l, err := noise3.NewListener(noise3.ListenerConfig{
		Conn: pc, Keys: []noise3.MaskKey{{Private: priv, NotAfter: clock.Now().Add(24 * time.Hour)}}, Profile: profile, Clock: clock,
	})
	if err != nil {
		_ = pc.Close()
		out.Err = err
		return
	}
	srv := startServer(l.Accept)
	defer func() { _ = l.Close(); srv.stop() }()
	tap, err := NewUDPTap(pc.LocalAddr().String(), fakeNoise3, pw)
	if err != nil {
		out.Err = err
		return
	}
	defer finish(&out, tap.Close, wait)

	host, port, _ := net.SplitHostPort(tap.Addr())
	pn, _ := net.LookupPort("udp", port)
	params, err := objects.Marshal(&pb.SuiteParams{Shape: &pb.ShapeProfile{Id: shape.IDDefaultUDP}})
	if err != nil {
		out.Err = err
		return
	}
	addr := &pb.Addr{Suite: objects.SuiteNoise3, Host: host, Port: uint32(pn), Mask: 1, Params: params}
	card := cardWith(pub)
	d := noise3.NewDialer(noise3.DialerConfig{Clock: clock})
	out.Err = runSessions(ctx, sessions, bulk, func(c context.Context) (mux.Carrier, error) {
		return d.Dial(c, carrier.DialRequest{Card: card, Addr: addr})
	})
	if out.Err != nil {
		return
	}
	// no cover: a hidden node owes a stranger silence
	tg := probe.Target{Name: "noise3", Network: "udp", Addr: pc.LocalAddr().String(), AltDial: altDialer("udp")}
	in := probe.Inputs{FirstFlight: tap.FirstClientDatagram(), Window: probeWindow, Wait: advance(clock), Listen: 600 * time.Millisecond}
	out.Probe = confirmed(probe.Run(ctx, tg, in), func() probe.Report { return probe.Run(ctx, tg, in) })
	return
}

// confirmed repeats a probe run that found an answer of the node's own, up to twice: an answer
// counts only when every repeat gives it. A difference that comes and goes (a TCP reset that
// overtakes an alert on one run) is a race of the machine, not a behaviour of the node, and the
// outcome says so in its note.
func confirmed(first probe.Report, rerun func() probe.Report) probe.Report {
	for try := 0; try < 2 && first.OwnAnswers() > 0; try++ {
		again := rerun()
		if len(again.Outcomes) != len(first.Outcomes) {
			break
		}
		for i, o := range first.Outcomes {
			if o.Own && !o.Expected && !again.Outcomes[i].Own {
				first.Outcomes[i].Own = false
				first.Outcomes[i].Note = "unconfirmed: the node answered differently once and as the cover did on a repeat"
			}
		}
	}
	return first
}

// ---------------------------------------------------------------------------- reality2-quic

// quicCover is a QUIC cover site on loopback. Every source address that talks to it gets a
// QUIC server of its own (same certificate and config), as a first-time visitor would find a
// server: a QUIC server keeps state by connection ID and answers a repeated Initial differently
// the second time, so one shared server would make the node (whose strangers reach the cover from
// its own sockets) and a probe sent straight to the cover see different servers.
func quicCover(cert tls.Certificate) (*net.UDPAddr, func(), error) {
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, nil, err
	}
	tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h3"}, MinVersion: tls.VersionTLS13}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var closers []func()
	toChild := map[string]*net.UDPConn{}

	newChild := func(src *net.UDPAddr) (*net.UDPConn, error) {
		cc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return nil, err
		}
		ln, err := quic.Listen(cc, tlsConf, &quic.Config{})
		if err != nil {
			_ = cc.Close()
			return nil, err
		}
		go func() {
			for {
				c, err := ln.Accept(ctx)
				if err != nil {
					return
				}
				go func() { <-ctx.Done(); _ = c.CloseWithError(0, "") }()
			}
		}()
		r, err := net.DialUDP("udp", nil, cc.LocalAddr().(*net.UDPAddr))
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
		closers = append(closers, func() { _ = ln.Close(); _ = r.Close() })
		go func() {
			buf := make([]byte, 64<<10)
			for {
				n, err := r.Read(buf)
				if err != nil {
					return
				}
				_, _ = front.WriteToUDP(buf[:n], src)
			}
		}()
		return r, nil
	}

	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, src, err := front.ReadFromUDP(buf)
			if err != nil {
				return
			}
			mu.Lock()
			r, ok := toChild[src.String()]
			if !ok {
				if r, err = newChild(src); err == nil {
					toChild[src.String()] = r
				}
			}
			mu.Unlock()
			if r != nil {
				_, _ = r.Write(buf[:n])
			}
		}
	}()
	return front.LocalAddr().(*net.UDPAddr), func() {
		cancel()
		_ = front.Close()
		mu.Lock()
		for _, c := range closers {
			c()
		}
		mu.Unlock()
	}, nil
}

func captureReality2QUIC(ctx context.Context, sessions, bulk int) (out capturedCarrier) {
	out.Name = "reality2-quic"
	pw, wait := newCapture()
	clock := &shiftClock{}
	coverCert, err := selfSignedCert(coverName)
	if err != nil {
		out.Err = err
		return
	}
	coverAddr, stopCover, err := quicCover(coverCert)
	if err != nil {
		out.Err = err
		return
	}
	defer stopCover()
	// a twin of the cover with the same certificate: the reference of the prober
	twinAddr, stopTwin, err := quicCover(coverCert)
	if err != nil {
		out.Err = err
		return
	}
	defer stopTwin()
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		out.Err = err
		return
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		out.Err = err
		return
	}
	tap, err := NewUDPTap(udp.LocalAddr().String(), fakeQUIC, pw)
	if err != nil {
		_ = udp.Close()
		out.Err = err
		return
	}
	defer finish(&out, tap.Close, wait)
	// the client dials the tap's own address (the QUIC dialer opens its socket itself), so that is
	// the address the token is bound to
	host, port, _ := net.SplitHostPort(tap.Addr())
	pn, _ := net.LookupPort("udp", port)
	own, err := selfSignedCert(coverName)
	if err != nil {
		_ = udp.Close()
		out.Err = err
		return
	}
	l, err := reality2.Listen(udp, reality2.ListenerConfig{
		Cover:     reality2.CoverProfile{Group: tls.X25519, Cert: &own},
		Masks:     []reality2.MaskKey{{Private: sk}},
		Addrs:     []string{reality2.AddressString(host, uint32(pn))},
		CoverAddr: coverAddr,
		Now:       clock.Now,
	})
	if err != nil {
		_ = udp.Close()
		out.Err = err
		return
	}
	srv := startServer(l.Accept)
	defer func() { _ = l.Close(); srv.stop() }()

	params, err := objects.Marshal(&pb.SuiteParams{Sni: coverName})
	if err != nil {
		out.Err = err
		return
	}
	addr := &pb.Addr{Suite: objects.SuiteReality2QUIC, Host: host, Port: uint32(pn), Mask: 1, Params: params}
	card := cardWith(sk.PublicKey().Bytes())
	d := reality2.NewDialer(reality2.DialerConfig{Group: tls.X25519, Now: clock.Now})
	out.Err = runSessions(ctx, sessions, bulk, func(c context.Context) (mux.Carrier, error) {
		return d.Dial(c, carrier.DialRequest{Card: card, Addr: addr})
	})
	if out.Err != nil {
		return
	}
	tg := probe.Target{Name: "reality2-quic", Network: "udp", Addr: udp.LocalAddr().String(), Cover: twinAddr.String(), SNI: coverName, AltDial: altDialer("udp")}
	in := probe.Inputs{FirstFlight: tap.FirstClientDatagram(), Window: probeWindow, Wait: advance(clock), Listen: 600 * time.Millisecond}
	// The node holds a lone Initial for PendingTTL (500 ms, wire.md 3.3) before the cover sees it,
	// so its answer comes later than a direct cover answer: the replay waits long enough for it.
	slow := in
	slow.Listen = 2 * time.Second
	run := func() probe.Report {
		rep := probe.Report{Target: tg.Name}
		rep.Outcomes = append(rep.Outcomes, probe.Replay(ctx, tg, slow)...)
		rep.Outcomes = append(rep.Outcomes, probe.Malformed(ctx, tg, in)...)
		rep.Outcomes = append(rep.Outcomes, probe.CoverCheck(ctx, tg, in)...)
		rep.Outcomes = append(rep.Outcomes, probe.PostSuspicion(ctx, tg, in)...)
		rep.Outcomes = append(rep.Outcomes, probe.TokenProbe(ctx, tg, in)...)
		return rep
	}
	out.Probe = confirmed(run(), run)
	out.NotChecked = append(out.NotChecked,
		"reality2-quic probe: the replay of a lone first Initial is answered by the node about 500 ms late (it holds a partial flight for PendingTTL before the cover sees it; the probe compares the answers, not the delay)",
		"reality2-quic probe: the cover is a bare QUIC server (a fresh one for every source address, so the node and the reference both meet a first-time visitor), not a website",
		"reality2-quic probe: the fresh valid and foreign token probes (D15) and the HTTP/3 cover check (D13) need a QUIC first-flight builder and an HTTP/3 reader that testbed/probe does not have")
	return
}

// ---------------------------------------------------------------------------- reality2-tcp

// tlsCover is a TLS server standing for the cover site: its own certificate, one line, then it
// reads whatever comes.
func tlsCover(cert tls.Certificate) (string, func(), error) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}})
	if err != nil {
		return "", nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Write([]byte("the cover answers"))
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }, nil
}

func captureReality2TCP(ctx context.Context, sessions, bulk int) (out capturedCarrier) {
	out.Name = "reality2-tcp"
	pw, wait := newCapture()
	clock := &shiftClock{}
	coverCert, err := selfSignedCert(coverName)
	if err != nil {
		out.Err = err
		return
	}
	coverAddr, stopCover, err := tlsCover(coverCert)
	if err != nil {
		out.Err = err
		return
	}
	defer stopCover()
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		out.Err = err
		return
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		out.Err = err
		return
	}
	own, err := selfSignedCert(coverName)
	if err != nil {
		_ = raw.Close()
		out.Err = err
		return
	}
	tokenAddr := reality2.AddressString(fakeTCP.Addr().String(), uint32(fakeTCP.Port()))
	l, err := reality2.ListenTCP(raw, reality2.TCPListenerConfig{
		Cover:     reality2.TCPCover{Group: tls.X25519MLKEM768, ALPN: "h2", Cert: &own},
		Masks:     []reality2.MaskKey{{Private: sk}},
		Addrs:     []string{tokenAddr},
		CoverAddr: coverAddr,
		Now:       clock.Now,
	})
	if err != nil {
		_ = raw.Close()
		out.Err = err
		return
	}
	srv := startServer(l.Accept)
	defer func() { _ = l.Close(); srv.stop() }()
	tap, err := NewTCPTap(raw.Addr().String(), fakeTCP, pw)
	if err != nil {
		out.Err = err
		return
	}
	defer finish(&out, tap.Close, wait)

	params, err := objects.Marshal(&pb.SuiteParams{Sni: coverName})
	if err != nil {
		out.Err = err
		return
	}
	addr := &pb.Addr{Suite: objects.SuiteReality2TCP, Host: fakeTCP.Addr().String(), Port: uint32(fakeTCP.Port()), Mask: 1, Params: params}
	card := cardWith(sk.PublicKey().Bytes())
	d := reality2.NewTCPDialer(reality2.TCPDialerConfig{Now: clock.Now})
	out.Err = runSessions(ctx, sessions, bulk, func(c context.Context) (mux.Carrier, error) {
		return d.Dial(c, carrier.DialRequest{Card: card, Addr: addr, DialContext: redirectTo(tap.Addr())})
	})
	if out.Err != nil {
		return
	}

	// the prober, as the integration test of testbed/probe does it: a legitimate first flight is
	// admitted once, then replayed; fresh valid and foreign tokens for the token probe
	now := clock.Now()
	pub := sk.PublicKey().Bytes()
	first, err := r2flight.Flight(pub, tokenAddr, coverName, now)
	if err != nil {
		out.Err = err
		return
	}
	valid, err := r2flight.Flight(pub, tokenAddr, coverName, now)
	if err != nil {
		out.Err = err
		return
	}
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		out.Err = err
		return
	}
	foreign, err := r2flight.Flight(other.PublicKey().Bytes(), tokenAddr, coverName, now)
	if err != nil {
		out.Err = err
		return
	}
	if err := sendOnce(raw.Addr().String(), first); err != nil {
		out.Err = fmt.Errorf("admit the first flight: %w", err)
		return
	}
	tg := probe.Target{Name: "reality2-tcp", Network: "tcp", Addr: raw.Addr().String(), Cover: coverAddr, SNI: coverName, AltDial: altDialer("tcp")}
	in := probe.Inputs{FirstFlight: first, Valid: valid, Foreign: foreign, Window: probeWindow, Wait: advance(clock), Listen: 800 * time.Millisecond}
	out.Probe = probe.Run(ctx, tg, in)
	return
}

// sendOnce writes b to addr and reads the first bytes that come back.
func sendOnce(addr string, b []byte) error {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Write(b); err != nil {
		return err
	}
	_ = c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	got := make([]byte, 16)
	n, _ := io.ReadFull(c, got)
	if n < 5 {
		return errors.New("no answer to the first flight")
	}
	return nil
}

// ---------------------------------------------------------------------------- ws2

// cdn is an HTTPS front on loopback that forwards to a plain HTTP backend, as a CDN does.
func cdn(cert tls.Certificate, backend string) (string, func(), error) {
	target, err := url.Parse("http://" + backend)
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{
		Handler:           httputil.NewSingleHostReverseProxy(target),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}))
	}()
	return ln.Addr().String(), func() { _ = srv.Close() }, nil
}

func siteHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = io.WriteString(w, siteText)
			return
		}
		http.NotFound(w, r)
	})
}

func captureWS2(ctx context.Context, sessions, bulk int) (out capturedCarrier) {
	out.Name = "ws2"
	pw, wait := newCapture()
	clock := &shiftClock{}
	cdnCert, err := selfSignedCert(cdnName)
	if err != nil {
		out.Err = err
		return
	}
	roots := x509.NewCertPool()
	roots.AddCert(cdnCert.Leaf)
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		out.Err = err
		return
	}
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		out.Err = err
		return
	}
	own, err := selfSignedCert(coverName)
	if err != nil {
		_ = backend.Close()
		out.Err = err
		return
	}
	cdnHostPort := net.JoinHostPort(cdnName, "443")
	node, err := ws2.Listen(backend, ws2.Config{
		Path:  wsPath,
		Cover: reality2.TCPCover{Group: tls.X25519MLKEM768, ALPN: "h2", Cert: &own},
		Masks: []reality2.MaskKey{{Private: sk}},
		Addrs: []string{cdnHostPort},
		Site:  siteHandler(),
		Now:   clock.Now,
	})
	if err != nil {
		_ = backend.Close()
		out.Err = err
		return
	}
	srv := startServer(node.Accept)
	defer func() { _ = node.Close(); srv.stop() }()
	cdnAddr, stopCDN, err := cdn(cdnCert, backend.Addr().String())
	if err != nil {
		out.Err = err
		return
	}
	defer stopCDN()
	tap, err := NewTCPTap(cdnAddr, fakeWS2, pw)
	if err != nil {
		out.Err = err
		return
	}
	defer finish(&out, tap.Close, wait)

	params, err := objects.Marshal(&pb.SuiteParams{Sni: coverName})
	if err != nil {
		out.Err = err
		return
	}
	addr := &pb.Addr{Suite: objects.SuiteWS2, Url: "wss://" + cdnHostPort + wsPath, Mask: 1, Params: params}
	card := cardWith(sk.PublicKey().Bytes())
	d := ws2.NewDialer(ws2.DialerConfig{RootCAs: roots, Now: clock.Now})
	out.Err = runSessions(ctx, sessions, bulk, func(c context.Context) (mux.Carrier, error) {
		return d.Dial(c, carrier.DialRequest{Card: card, Addr: addr, DialContext: redirectTo(tap.Addr())})
	})
	if out.Err != nil {
		return
	}

	// The prober compares the CDN in front of the node with a CDN in front of a plain site that has
	// the same pages: the node adds nothing a stranger can see on any path but its secret one.
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		out.Err = err
		return
	}
	plainSrv := &http.Server{Handler: siteHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = plainSrv.Serve(plain) }()
	defer plainSrv.Close()
	refAddr, stopRef, err := cdn(cdnCert, plain.Addr().String())
	if err != nil {
		out.Err = err
		return
	}
	defer stopRef()
	out.Probe = ws2Probe(ctx, cdnAddr, refAddr)
	out.NotChecked = append(out.NotChecked,
		"ws2 probe: a WebSocket upgrade on the secret path with a garbage first message is not compared with anything (the node closes it with 1008 after 50-500 ms by design, wire.md 4.4; the reference site has no WebSocket); the path is secret, so a stranger does not reach it",
		"ws2 probe: the TCP-level cases (replay, malformed first record, post-suspicion) hit the CDN, not the node, so they say nothing about it and are not run")
	return
}

// ws2Probe asks the node's CDN and the reference CDN the same questions over HTTPS and compares the
// status and the body: the pages a stranger can ask for.
func ws2Probe(ctx context.Context, nodeAddr, refAddr string) probe.Report {
	rep := probe.Report{Target: "ws2"}
	get := func(addr, path string) probe.Answer {
		tr := &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{ServerName: cdnName, InsecureSkipVerify: true}, // #nosec G402 -- the lab CDN is self-signed
			DialContext:       redirectTo(addr),
		}
		defer tr.CloseIdleConnections()
		cl := &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+cdnName+path, nil)
		if err != nil {
			return probe.Answer{Class: "failed", Detail: err.Error()}
		}
		resp, err := cl.Do(req)
		if err != nil {
			return probe.Answer{Class: "failed", Detail: "request"}
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return probe.Answer{Class: "http", Detail: fmt.Sprintf("%d/%d bytes/%t", resp.StatusCode, len(body), bytes.Equal(body, []byte(siteText)))}
	}
	for _, c := range []struct{ det, name, path string }{
		{"D13", "front page", "/"},
		{"D12", "secret path, no upgrade", wsPath},
		{"D12", "other path", "/admin"},
	} {
		o := probe.Outcome{Detector: c.det, Case: c.name, Node: get(nodeAddr, c.path), Cover: get(refAddr, c.path)}
		o.Own = !probe.Equivalent(o.Node, o.Cover)
		rep.Outcomes = append(rep.Outcomes, o)
	}
	return rep
}
