package profile

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	quic "github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Target is one cover to measure.
type Target struct {
	// Host labels the stored profile.
	Host string
	// Addr is host:port of the cover's TCP 443 or UDP 443.
	Addr string
	// SNI is the server name the Chrome-shaped ClientHello carries.
	SNI string
}

// ProbeConfig configures a Prober.
type ProbeConfig struct {
	// Samples is the number of connections per cover and transport; the rule of T2.6b-1 is 5.
	Samples int
	// Gap is the least time between two connection starts. The measurement of live sites uses
	// one second; local servers use zero.
	Gap time.Duration
	// Settle is how long the connection stays quiet after the handshake so that the server's
	// post-handshake records arrive before the request.
	Settle time.Duration
	// Timeout bounds one sample.
	Timeout time.Duration
	// NoRequest skips the HEAD request (local servers do not speak HTTP).
	NoRequest bool
	// Now and Sleep are the injectable clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Prober measures covers with the Chrome-shaped ClientHello, paced by Gap.
type Prober struct {
	cfg  ProbeConfig
	mu   sync.Mutex
	last time.Time
}

// NewProber applies the defaults of cfg.
func NewProber(cfg ProbeConfig) *Prober {
	if cfg.Samples <= 0 {
		cfg.Samples = 5
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 300 * time.Millisecond
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	return &Prober{cfg: cfg}
}

// pace blocks until at least Gap has passed since the previous connection start.
func (p *Prober) pace(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.Gap <= 0 {
		return nil
	}
	now := p.cfg.Now()
	if !p.last.IsZero() {
		if wait := p.last.Add(p.cfg.Gap).Sub(now); wait > 0 {
			if err := p.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
			now = p.cfg.Now()
		}
	}
	p.last = now
	return nil
}

type sampleInfo struct {
	alpn         string
	leafKey      string
	certVerified bool
	requestOK    bool
	requestInfo  string
}

// MeasureTCP connects Samples times over TCP+TLS with the Chrome ClientHello (handshake, a quiet
// settle time, one HEAD) and folds the observations into a profile.
func (p *Prober) MeasureTCP(ctx context.Context, t Target) (*CoverProfile, error) {
	return p.measure(ctx, t, TCP, p.sampleTCP)
}

// MeasureQUIC is MeasureTCP over QUIC with HTTP/3 (the cover must really serve it, wire.md §3.4).
func (p *Prober) MeasureQUIC(ctx context.Context, t Target) (*CoverProfile, error) {
	return p.measure(ctx, t, QUIC, p.sampleQUIC)
}

func (p *Prober) measure(ctx context.Context, t Target, tr Transport,
	sample func(context.Context, Target) (Vector, sampleInfo, error)) (*CoverProfile, error) {
	addr, err := resolveV4First(ctx, t.Addr)
	if err != nil {
		return nil, err
	}
	t.Addr = addr
	var (
		vs   []Vector
		last sampleInfo
		alpn = map[string]bool{}
	)
	for i := 0; i < p.cfg.Samples; i++ {
		v, info, err := sample(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("profile: %s sample %d of %s: %w", tr, i+1, t.Host, err)
		}
		vs = append(vs, v)
		alpn[info.alpn] = true
		last = info
	}
	prof := Aggregate(vs)
	prof.Transport, prof.Host, prof.SNI, prof.Addr = tr, t.Host, t.SNI, t.Addr
	prof.MeasuredAt = p.cfg.Now().Unix()
	prof.ALPN, prof.LeafKey, prof.CertVerified = last.alpn, last.leafKey, last.certVerified
	prof.RequestOK, prof.RequestInfo = last.requestOK, last.requestInfo
	if len(alpn) > 1 {
		prof.Unstable = append(prof.Unstable, ItemALPN)
	}
	return prof, nil
}

func resolveV4First(ctx context.Context, hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", err
	}
	if net.ParseIP(host) != nil {
		return hostport, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("profile: resolve %s: %v", host, err)
	}
	pick := ips[0].IP
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			pick = ip.IP
			break
		}
	}
	return net.JoinHostPort(pick.String(), port), nil
}

// recConn records every chunk read from and written to the connection, in order.
type recConn struct {
	net.Conn
	mu     sync.Mutex
	events []Event
}

func (c *recConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.add(true, b[:n])
	}
	return n, err
}

func (c *recConn) Write(b []byte) (int, error) {
	// Recorded before the send: the peer's answer can be read before Write returns.
	c.add(false, b)
	return c.Conn.Write(b)
}

func (c *recConn) add(fromServer bool, b []byte) {
	c.mu.Lock()
	c.events = append(c.events, Event{FromServer: fromServer, Data: append([]byte(nil), b...)})
	c.mu.Unlock()
}

func (c *recConn) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

func (p *Prober) sampleTCP(ctx context.Context, t Target) (Vector, sampleInfo, error) {
	var info sampleInfo
	if err := p.pace(ctx); err != nil {
		return Vector{}, info, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return Vector{}, info, err
	}
	rec := &recConn{Conn: raw}
	var keylog bytes.Buffer // the secrets of this one connection, written during the handshake
	uc := utls.UClient(rec, &utls.Config{ServerName: t.SNI, InsecureSkipVerify: true, KeyLogWriter: &keylog}, utls.HelloChrome_Auto)
	defer uc.Close()
	deadline, _ := ctx.Deadline()
	_ = uc.SetDeadline(deadline)
	if err := uc.HandshakeContext(ctx); err != nil {
		return Vector{}, info, fmt.Errorf("handshake: %w", err)
	}
	// Let the server's post-handshake records arrive: crypto/tls reads them only on a Read.
	_ = uc.SetReadDeadline(p.cfg.Now().Add(p.cfg.Settle))
	var scratch [4096]byte
	var pre []byte // application bytes read while settling (an HTTP/2 SETTINGS frame), replayed
	for {
		n, err := uc.Read(scratch[:])
		pre = append(pre, scratch[:n]...)
		if err != nil || n == 0 {
			var ne net.Error
			if err != nil && !(errors.As(err, &ne) && ne.Timeout()) {
				return Vector{}, info, fmt.Errorf("settle read: %w", err)
			}
			break
		}
	}
	_ = uc.SetDeadline(deadline)
	events := rec.snapshot()
	v, err := ObserveTCPKeyed(events, ParseKeyLog(keylog.Bytes()))
	if err != nil {
		return v, info, err
	}
	st := uc.ConnectionState()
	info.alpn = st.NegotiatedProtocol
	info.leafKey, info.certVerified = describeLeaf(st.PeerCertificates, t.SNI)
	if !p.cfg.NoRequest {
		info.requestOK, info.requestInfo = headTCP(ctx, &replayConn{Conn: uc, pre: pre}, st.NegotiatedProtocol, t.SNI)
	}
	return v, info, nil
}

// replayConn serves bytes already read before it reads from the connection.
type replayConn struct {
	net.Conn
	pre []byte
}

func (c *replayConn) Read(b []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(b, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

func headTCP(ctx context.Context, c net.Conn, alpn, host string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return false, err.Error()
	}
	var resp *http.Response
	if alpn == "h2" {
		cc, err := (&http2.Transport{}).NewClientConn(c)
		if err != nil {
			return false, "h2: " + err.Error()
		}
		resp, err = cc.RoundTrip(req)
		if err != nil {
			return false, "h2: " + err.Error()
		}
	} else {
		if err := req.Write(c); err != nil {
			return false, "http/1.1: " + err.Error()
		}
		resp, err = http.ReadResponse(bufio.NewReader(c), req)
		if err != nil {
			return false, "http/1.1: " + err.Error()
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return true, fmt.Sprintf("%s %d", resp.Proto, resp.StatusCode)
}

func describeLeaf(chain []*x509.Certificate, sni string) (string, bool) {
	if len(chain) == 0 {
		return "", false
	}
	leaf := chain[0]
	var kind string
	switch k := leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		kind = "ecdsa-" + k.Curve.Params().Name
	case *rsa.PublicKey:
		kind = fmt.Sprintf("rsa-%d", k.N.BitLen())
	case ed25519.PublicKey:
		kind = "ed25519"
	default:
		kind = leaf.PublicKeyAlgorithm.String()
	}
	pool := x509.NewCertPool()
	for _, c := range chain[1:] {
		pool.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: sni, Intermediates: pool})
	return kind, err == nil
}

// recPacketConn records every datagram of a QUIC connection.
type recPacketConn struct {
	net.PacketConn
	mu     sync.Mutex
	events []Event
}

func (c *recPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, a, err := c.PacketConn.ReadFrom(b)
	if n > 0 {
		c.add(true, b[:n])
	}
	return n, a, err
}

func (c *recPacketConn) WriteTo(b []byte, a net.Addr) (int, error) {
	// Recorded before the send: the peer's answer can be read before WriteTo returns.
	c.add(false, b)
	return c.PacketConn.WriteTo(b, a)
}

func (c *recPacketConn) add(fromServer bool, b []byte) {
	c.mu.Lock()
	c.events = append(c.events, Event{FromServer: fromServer, Data: append([]byte(nil), b...)})
	c.mu.Unlock()
}

func (c *recPacketConn) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

func (p *Prober) sampleQUIC(ctx context.Context, t Target) (Vector, sampleInfo, error) {
	var info sampleInfo
	if err := p.pace(ctx); err != nil {
		return Vector{}, info, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	raddr, err := net.ResolveUDPAddr("udp", t.Addr)
	if err != nil {
		return Vector{}, info, err
	}
	network := "udp4"
	if raddr.IP.To4() == nil {
		network = "udp6"
	}
	udp, err := net.ListenUDP(network, nil)
	if err != nil {
		return Vector{}, info, err
	}
	defer udp.Close()
	rec := &recPacketConn{PacketConn: udp}
	conn, err := quic.Dial(ctx, rec, raddr, &tls.Config{
		ServerName:         t.SNI,
		NextProtos:         []string{"h3"},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
	}, &quic.Config{ChromeParrot: true})
	if err != nil {
		return Vector{}, info, fmt.Errorf("handshake: %w", err)
	}
	defer conn.CloseWithError(0, "")
	if err := p.cfg.Sleep(ctx, p.cfg.Settle); err != nil {
		return Vector{}, info, err
	}
	events := rec.snapshot()
	v, err := ObserveQUIC(events)
	if err != nil {
		return v, info, err
	}
	st := conn.ConnectionState().TLS
	info.alpn = st.NegotiatedProtocol
	info.leafKey, info.certVerified = describeLeaf(st.PeerCertificates, t.SNI)
	if !p.cfg.NoRequest {
		info.requestOK, info.requestInfo = headQUIC(ctx, conn, t.SNI)
	}
	return v, info, nil
}

func headQUIC(ctx context.Context, conn *quic.Conn, host string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return false, err.Error()
	}
	tr := &http3.Transport{}
	resp, err := tr.NewClientConn(conn).RoundTrip(req)
	if err != nil {
		return false, "h3: " + err.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return true, fmt.Sprintf("%s %d", resp.Proto, resp.StatusCode)
}
