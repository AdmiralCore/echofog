package main

// The real v2 carriers in the live lab (T15.0f): reality2-tcp, ws2 and reality2-quic, each a listener
// of internal/v2/carrier in the container x1 and a dialer of the same package in b1. The packets
// cross the lab's routers (gw_b) and the lab's wan bridge; nothing is a tap on loopback. The keys
// derive from the run's seed, as for noise3, so the client needs no card file.
//
// The traffic is what a person does through a tunnel: pages of varied size with a short think time
// ("visit": a few short sessions) or one session of minutes ("long").

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	quic "github.com/apernet/quic-go"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/carrier/ws2"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/testbed/labkeys"
	"github.com/echofog/echofog-core/testbed/probe"
)

const (
	suiteNoise3 = "noise3"
	suiteR2TCP  = "reality2-tcp"
	suiteWS2    = "ws2"
	suiteR2QUIC = "reality2-quic"

	realCoverName = "cover.lab"
	realCDNName   = "cdn.lab"
	realWSPath    = "/s/4f9c1e7a2b6d8035"
	realSiteText  = "plain site"

	opPage      = 'P'
	headerLen   = 9
	maxPageSize = 1 << 20
	minSpacing  = 550 * time.Millisecond
)

// realKey is the masking key of one suite for the run's seed (see labkeys).
func realKey(seed int64, suite string) *ecdh.PrivateKey { return labkeys.Mask(seed, suite) }

func labCert(names ...string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		DNSNames:  names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(crand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// ---------------------------------------------------------------------------- the traffic

// serveOne answers one stream: a page of n bytes after reading the request.
func serveOne(st mux.RawStream) {
	defer st.Close()
	var h [headerLen]byte
	if _, err := io.ReadFull(st, h[:]); err != nil {
		return
	}
	n := int(binary.BigEndian.Uint32(h[1:5]))
	reqLen := int(binary.BigEndian.Uint32(h[5:9]))
	if h[0] != opPage {
		return
	}
	if reqLen > headerLen {
		if _, err := io.CopyN(io.Discard, st, int64(reqLen-headerLen)); err != nil {
			return
		}
	}
	page := make([]byte, min(n, maxPageSize))
	for i := range page {
		page[i] = byte('a' + i%26)
	}
	_, _ = st.Write(page)
}

func pageHeader(n, reqLen int) []byte {
	h := make([]byte, headerLen)
	h[0] = opPage
	binary.BigEndian.PutUint32(h[1:], uint32(n))
	binary.BigEndian.PutUint32(h[5:], uint32(reqLen))
	return h
}

func loadPage(ctx context.Context, c mux.Carrier, n, reqLen int) error {
	st, err := c.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	defer st.Close()
	req := make([]byte, reqLen)
	_, _ = crand.Read(req)
	copy(req, pageHeader(n, reqLen))
	if _, err := st.Write(req); err != nil {
		return fmt.Errorf("write the request: %w", err)
	}
	page := make([]byte, n)
	if got, err := io.ReadFull(st, page); err != nil {
		return fmt.Errorf("page of %d bytes: got %d: %w", n, got, err)
	}
	for i, b := range page {
		if b != byte('a'+i%26) {
			return fmt.Errorf("page of %d bytes: byte %d is wrong", n, i)
		}
	}
	return nil
}

// acceptLoop serves every carrier the listener accepts until ctx ends.
func acceptLoop(ctx context.Context, accept func(context.Context) (mux.Carrier, error)) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := accept(ctx)
		if err != nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			go func() { <-ctx.Done(); _ = c.Close() }()
			for {
				st, err := c.AcceptStream(ctx)
				if err != nil {
					return
				}
				go serveOne(st)
			}
		}()
	}
}

// ---------------------------------------------------------------------------- servers

type realServe struct {
	suite     string
	listen    string // host:port to bind
	advertise string // the address the token is bound to (the node's lab address)
	cover     string // reality2-tcp: the cover site's host:port
	seed      int64
	ready     string
	caOut     string // ws2: the CDN certificate (PEM) the client trusts
	refListen string // ws2: where the reference front (a plain site behind the same certificate) listens
}

func serveReal(ctx context.Context, s realServe) error {
	sk := realKey(s.seed, s.suite)
	own, err := labCert(realCoverName)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(s.listen)
	if err != nil {
		return err
	}
	switch s.suite {
	case suiteNoise3:
		pc, err := net.ListenPacket("udp", s.listen)
		if err != nil {
			return err
		}
		priv, _ := noise3Key(s.seed)
		profile, err := shape.Builtin(shape.IDDefaultUDP)
		if err != nil {
			_ = pc.Close()
			return err
		}
		l, err := noise3.NewListener(noise3.ListenerConfig{
			Conn:    pc,
			Keys:    []noise3.MaskKey{{Private: priv, NotAfter: time.Now().Add(24 * time.Hour)}},
			Profile: profile,
		})
		if err != nil {
			_ = pc.Close()
			return err
		}
		defer l.Close()
		markReady(s.ready, suiteNoise3)
		go func() { <-ctx.Done(); _ = l.Close() }()
		acceptLoop(ctx, l.Accept)
	case suiteR2TCP:
		raw, err := net.Listen("tcp", s.listen)
		if err != nil {
			return err
		}
		l, err := reality2.ListenTCP(raw, reality2.TCPListenerConfig{
			Cover:     reality2.TCPCover{Group: tls.X25519MLKEM768, ALPN: "h2", Cert: &own},
			Masks:     []reality2.MaskKey{{Private: sk}},
			Addrs:     []string{s.advertise},
			CoverAddr: s.cover,
		})
		if err != nil {
			_ = raw.Close()
			return err
		}
		defer l.Close()
		markReady(s.ready, suiteR2TCP)
		go func() { <-ctx.Done(); _ = l.Close() }()
		acceptLoop(ctx, l.Accept)
	case suiteWS2:
		// a node on a loopback port behind an HTTPS front that forwards to it, as a CDN does
		backend, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		cdnCert, err := labCert(realCDNName)
		if err != nil {
			return err
		}
		if s.caOut != "" {
			p := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cdnCert.Certificate[0]})
			if err := os.WriteFile(s.caOut, p, 0o644); err != nil {
				return err
			}
		}
		node, err := ws2.Listen(backend, ws2.Config{
			Path:  realWSPath,
			Cover: reality2.TCPCover{Group: tls.X25519MLKEM768, ALPN: "h2", Cert: &own},
			Masks: []reality2.MaskKey{{Private: sk}},
			Addrs: []string{net.JoinHostPort(realCDNName, port)},
			Site:  realSite(),
		})
		if err != nil {
			_ = backend.Close()
			return err
		}
		defer node.Close()
		stopFront, err := cdnFront(cdnCert, s.listen, backend.Addr().String())
		if err != nil {
			return err
		}
		defer stopFront()
		// the reference: a front of the same certificate before a plain site with the same pages
		plain, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		plainSrv := &http.Server{Handler: realSite(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = plainSrv.Serve(plain) }()
		defer plainSrv.Close()
		stopRef, err := cdnFront(cdnCert, s.refListen, plain.Addr().String())
		if err != nil {
			return err
		}
		defer stopRef()
		markReady(s.ready, suiteWS2)
		go func() { <-ctx.Done(); _ = node.Close() }()
		acceptLoop(ctx, node.Accept)
	case suiteR2QUIC:
		coverAddr, stopCover, err := bareQUICCover(own, "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer stopCover()
		// the reference: a twin cover with the same certificate, reachable by the prober
		if s.refListen != "" {
			_, stopTwin, err := bareQUICCover(own, s.refListen)
			if err != nil {
				return err
			}
			defer stopTwin()
		}
		udp, err := net.ListenPacket("udp", s.listen)
		if err != nil {
			return err
		}
		l, err := reality2.Listen(udp, reality2.ListenerConfig{
			Cover:     reality2.CoverProfile{Group: tls.X25519, Cert: &own},
			Masks:     []reality2.MaskKey{{Private: sk}},
			Addrs:     []string{s.advertise},
			CoverAddr: coverAddr,
		})
		if err != nil {
			_ = udp.Close()
			return err
		}
		defer l.Close()
		markReady(s.ready, suiteR2QUIC)
		go func() { <-ctx.Done(); _ = l.Close() }()
		acceptLoop(ctx, l.Accept)
	default:
		return fmt.Errorf("suite %q", s.suite)
	}
	return nil
}

func markReady(path, what string) {
	if path != "" {
		_ = os.WriteFile(path, []byte(what+"\n"), 0o644)
	}
}

func realSite() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = io.WriteString(w, realSiteText)
			return
		}
		http.NotFound(w, r)
	})
}

// cdnFront is an HTTPS front that forwards to a plain HTTP backend. It returns a stop function.
func cdnFront(cert tls.Certificate, listen, backend string) (func(), error) {
	target, err := url.Parse("http://" + backend)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: httputil.NewSingleHostReverseProxy(target), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}))
	}()
	return func() { _ = srv.Close() }, nil
}

// bareQUICCover is the cover of reality2-quic: a QUIC server per source address, on loopback.
// listen is where the cover answers: loopback (":0" on 127.0.0.1) for the node's own cover, or a public
// port for the reference that the prober compares the node with.
func bareQUICCover(cert tls.Certificate, listen string) (*net.UDPAddr, func(), error) {
	la, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, nil, err
	}
	front, err := net.ListenUDP("udp", la)
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

// ---------------------------------------------------------------------------- client

func cardFor(pub []byte) *objects.Card {
	return &objects.Card{Body: &pb.NodeCard{MaskKeys: []*pb.MaskKey{{
		Key:      keys.New(keys.X25519, pub),
		NotAfter: uint64(time.Now().Add(time.Hour).Unix()),
	}}}}
}

// realDialer returns a function that opens one carrier of the suite to peer (host:port).
func realDialer(suite, peer, caFile string, seed int64) (func(context.Context) (mux.Carrier, error), error) {
	host, portStr, err := net.SplitHostPort(peer)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	if suite == suiteNoise3 {
		_, pub := noise3Key(seed)
		nparams, err := objects.Marshal(&pb.SuiteParams{Shape: &pb.ShapeProfile{Id: shape.IDDefaultUDP}})
		if err != nil {
			return nil, err
		}
		addr := &pb.Addr{Suite: objects.SuiteNoise3, Host: host, Port: uint32(port), Mask: 1, Params: nparams}
		d := noise3.NewDialer(noise3.DialerConfig{})
		return func(ctx context.Context) (mux.Carrier, error) {
			return d.Dial(ctx, carrier.DialRequest{Card: cardFor(pub), Addr: addr})
		}, nil
	}
	card := cardFor(realKey(seed, suite).PublicKey().Bytes())
	params, err := objects.Marshal(&pb.SuiteParams{Sni: realCoverName})
	if err != nil {
		return nil, err
	}
	switch suite {
	case suiteR2TCP:
		addr := &pb.Addr{Suite: objects.SuiteReality2TCP, Host: host, Port: uint32(port), Mask: 1, Params: params}
		d := reality2.NewTCPDialer(reality2.TCPDialerConfig{})
		return func(ctx context.Context) (mux.Carrier, error) {
			return d.Dial(ctx, carrier.DialRequest{Card: card, Addr: addr})
		}, nil
	case suiteR2QUIC:
		addr := &pb.Addr{Suite: objects.SuiteReality2QUIC, Host: host, Port: uint32(port), Mask: 1, Params: params}
		d := reality2.NewDialer(reality2.DialerConfig{Group: tls.X25519})
		return func(ctx context.Context) (mux.Carrier, error) {
			return d.Dial(ctx, carrier.DialRequest{Card: card, Addr: addr})
		}, nil
	case suiteWS2:
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("the CDN certificate: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("the CDN certificate is not PEM")
		}
		cdnHostPort := net.JoinHostPort(realCDNName, portStr)
		addr := &pb.Addr{Suite: objects.SuiteWS2, Url: "wss://" + cdnHostPort + realWSPath, Mask: 1, Params: params}
		d := ws2.NewDialer(ws2.DialerConfig{RootCAs: roots})
		redirect := func(ctx context.Context, network, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, network, peer)
		}
		return func(ctx context.Context) (mux.Carrier, error) {
			return d.Dial(ctx, carrier.DialRequest{Card: card, Addr: addr, DialContext: redirect})
		}, nil
	}
	return nil, fmt.Errorf("suite %q", suite)
}

// realClient runs sessions short sessions ("visit"), then, when d > 0, one session of that length
// ("long") that loads a page every one to three seconds.
func realClient(suite, peer, caFile string, seed int64, sessions int, d time.Duration) (result, error) {
	res := result{Mode: "real-" + suite, Carrier: suite}
	dial, err := realDialer(suite, peer, caFile, seed)
	if err != nil {
		return res, err
	}
	rng := mrand.New(mrand.NewSource(seed + 11))
	var last time.Time
	for i := 0; i < sessions; i++ {
		if w := time.Until(last.Add(minSpacing)); w > 0 {
			time.Sleep(w)
		}
		last = time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		c, err := dial(ctx)
		if err != nil {
			cancel()
			return res, fmt.Errorf("session %d: dial: %w", i+1, err)
		}
		res.Connections++
		for k := 0; k < 5; k++ {
			n := 300 + rng.Intn(60000)
			if err := loadPage(ctx, c, n, 400+rng.Intn(300)); err != nil {
				_ = c.Close()
				cancel()
				return res, fmt.Errorf("session %d page %d: %w", i+1, k+1, err)
			}
			res.Bytes += int64(n)
			res.Packets++
			time.Sleep(time.Duration(5+rng.Intn(35)) * time.Millisecond)
		}
		_ = c.Close()
		cancel()
	}
	if d > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), d+30*time.Second)
		defer cancel()
		dctx, dcancel := context.WithTimeout(ctx, 25*time.Second)
		c, err := dial(dctx)
		dcancel()
		if err != nil {
			return res, fmt.Errorf("long session: dial: %w", err)
		}
		defer c.Close()
		res.Connections++
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			n := 2000 + rng.Intn(150000)
			if err := loadPage(ctx, c, n, 400+rng.Intn(300)); err != nil {
				return res, fmt.Errorf("long session after %s: %w", time.Since(end.Add(-d)).Round(time.Second), err)
			}
			res.Bytes += int64(n)
			res.Packets++
			time.Sleep(time.Duration(1000+rng.Intn(2000)) * time.Millisecond)
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------- probe helpers

// sendHex writes the hex bytes to a TCP peer and reads the first bytes back: it admits a first
// flight, so a replay of it is a replay.
func sendHex(peer, hexText string) (result, error) {
	res := result{Mode: "send-hex", Carrier: "probe"}
	b, err := hex.DecodeString(hexText)
	if err != nil {
		return res, err
	}
	c, err := net.DialTimeout("tcp", peer, 3*time.Second)
	if err != nil {
		return res, err
	}
	defer c.Close()
	if _, err := c.Write(b); err != nil {
		return res, err
	}
	res.Bytes, res.Connections = int64(len(b)), 1
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 16)
	if n, _ := io.ReadFull(c, got); n < 5 {
		return res, errors.New("no answer to the first flight")
	}
	return res, nil
}

// ws2Probe asks the node's front and the reference front the same questions over HTTPS from the
// border and compares status and body (D12, D13). The output is a probe.Report as JSON.
func ws2Probe(node, ref string) error {
	rep := probe.Report{Target: "ws2"}
	get := func(addr, path string) probe.Answer {
		tr := &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{ServerName: realCDNName, InsecureSkipVerify: true}, // #nosec G402 -- the lab front is self-signed
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		}
		defer tr.CloseIdleConnections()
		cl := &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := cl.Get("https://" + realCDNName + path)
		if err != nil {
			return probe.Answer{Class: "failed", Detail: "request"}
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return probe.Answer{Class: "http", Detail: fmt.Sprintf("%d/%d bytes/%t", resp.StatusCode, len(body), bytes.Equal(body, []byte(realSiteText)))}
	}
	for _, c := range []struct{ det, name, path string }{
		{"D13", "front page", "/"},
		{"D12", "secret path, no upgrade", realWSPath},
		{"D12", "other path", "/admin"},
	} {
		o := probe.Outcome{Detector: c.det, Case: c.name, Node: get(node, c.path), Cover: get(ref, c.path)}
		o.Own = !probe.Equivalent(o.Node, o.Cover)
		rep.Outcomes = append(rep.Outcomes, o)
	}
	return json.NewEncoder(os.Stdout).Encode([]probe.Report{rep})
}
