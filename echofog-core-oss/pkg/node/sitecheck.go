package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// siteCheckRoots verifies site certificates; nil means the system roots (tests set their own).
var siteCheckRoots *x509.CertPool

const (
	siteCheckTimeout = 8 * time.Second
	siteCheckWorkers = 6
	// MaxSiteChecks bounds one check run.
	MaxSiteChecks = 32
)

// Reach is how one site answered on one path.
type Reach struct {
	OK     bool   `json:"ok"`
	Ms     int    `json:"ms"`
	Code   string `json:"code,omitempty"`   // why it did not open, one of the Reach* codes, for the UI to word
	Reason string `json:"reason,omitempty"` // the same in English, with the error itself when no code fits
}

// Why a site did not open (Reach.Code).
const (
	ReachPayment = "payment" // the exit asks for payment
	ReachDNS     = "dns"     // the name did not resolve: a DNS block, or no such site
	ReachCert    = "cert"    // a certificate that does not verify: the connection is intercepted
	ReachTimeout = "timeout" // no answer
	ReachReset   = "reset"   // the connection was cut, as DPI blocking does
	ReachRefused = "refused" // nothing listens there
	ReachExit    = "exit"    // the exit could not reach the site
	ReachOther   = "other"
)

// SiteCheck is the result for one site: directly and, when an exit was given, through it.
type SiteCheck struct {
	Host   string `json:"host"`
	Direct Reach  `json:"direct"`
	Exit   *Reach `json:"exit,omitempty"`
}

// CheckSites opens a TLS connection to each host (port 443, or the port given) and verifies its
// certificate, directly and through exit when it is not nil. A TLS handshake is what blocking by
// DPI breaks, and a certificate that does not verify means the connection was intercepted, so
// this tells whether the site really opens, not only whether a TCP port answers.
func (n *Node) CheckSites(ctx context.Context, exit *types.PeerID, hosts []string) ([]SiteCheck, error) {
	if len(hosts) > MaxSiteChecks {
		return nil, errors.New("too many sites in one check")
	}
	out := make([]SiteCheck, len(hosts))
	for i, h := range hosts {
		host, err := normalizeSite(h)
		if err != nil {
			return nil, err
		}
		out[i].Host = host
	}
	if exit != nil {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := swarmConnector{n}.Ensure(cctx, *exit)
		cancel()
		if err != nil {
			return nil, errors.New("the exit cannot be reached: " + err.Error())
		}
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < siteCheckWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				target := out[i].Host
				out[i].Direct = n.reachDirect(ctx, target)
				if exit != nil {
					r := n.reachThrough(ctx, *exit, target)
					out[i].Exit = &r
				}
			}
		}()
	}
	for i := range out {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out, nil
}

// normalizeSite accepts "youtube.com", "https://www.youtube.com/watch" or "host:port" and returns
// host:port.
func normalizeSite(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	host, port := s, "443"
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	}
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " \t\\@") {
		return "", errors.New("not a site: " + s)
	}
	return net.JoinHostPort(host, port), nil
}

func (n *Node) reachDirect(ctx context.Context, target string) Reach {
	ctx, cancel := context.WithTimeout(ctx, siteCheckTimeout)
	defer cancel()
	host, _, _ := net.SplitHostPort(target)
	start := time.Now()
	d := tls.Dialer{NetDialer: &net.Dialer{}, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: siteCheckRoots}}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return failed(err)
	}
	_ = conn.Close()
	return Reach{OK: true, Ms: int(time.Since(start).Milliseconds())}
}

func (n *Node) reachThrough(ctx context.Context, exit types.PeerID, target string) Reach {
	ctx, cancel := context.WithTimeout(ctx, siteCheckTimeout+4*time.Second)
	defer cancel()
	host, _, _ := net.SplitHostPort(target)
	start := time.Now()
	stream, err := exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(n.Swarm, exit)).DialExitStreamConfirmed(ctx, target)
	if err != nil {
		return failed(err)
	}
	conn := &streamConn{rwc: stream}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: siteCheckRoots})
	if err := tc.HandshakeContext(ctx); err != nil {
		return failed(err)
	}
	return Reach{OK: true, Ms: int(time.Since(start).Milliseconds())}
}

func failed(err error) Reach {
	code := reachCode(err)
	r := Reach{Code: code, Reason: reachWords[code]}
	if code == ReachOther {
		r.Reason = err.Error()
	}
	return r
}

var reachWords = map[string]string{
	ReachPayment: "the exit asks for payment",
	ReachDNS:     "name not found",
	ReachCert:    "wrong certificate (intercepted)",
	ReachTimeout: "no answer",
	ReachReset:   "connection reset (blocked)",
	ReachRefused: "refused",
	ReachExit:    "the exit could not reach it",
}

// reachCode says why a check failed.
func reachCode(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	switch {
	case errors.Is(err, exit_gateway.ErrPaymentRequired):
		return ReachPayment
	case errors.As(err, &dnsErr):
		return ReachDNS
	case errors.As(err, &certErr), errors.As(err, &unknownAuth), errors.As(err, &hostErr):
		return ReachCert
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return ReachTimeout
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		return ReachReset
	case errors.Is(err, syscall.ECONNREFUSED):
		return ReachRefused
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "actively refused") || strings.Contains(msg, "connection refused"):
		return ReachRefused
	case strings.Contains(msg, "could not reach the target"):
		return ReachExit
	case strings.Contains(msg, "forcibly closed") || strings.Contains(msg, "reset by peer"):
		return ReachReset
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return ReachTimeout
	case strings.Contains(msg, "no such host"):
		return ReachDNS
	}
	return ReachOther
}

// streamConn lets TLS run over a tunnel stream. Deadlines are carried by the context: when it
// ends the stream is closed.
type streamConn struct {
	rwc  io.ReadWriteCloser
	once sync.Once
}

func (c *streamConn) Read(p []byte) (int, error)  { return c.rwc.Read(p) }
func (c *streamConn) Write(p []byte) (int, error) { return c.rwc.Write(p) }
func (c *streamConn) Close() error {
	var err error
	c.once.Do(func() { err = c.rwc.Close() })
	return err
}
func (c *streamConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *streamConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "exit" }
func (dummyAddr) String() string  { return "exit" }
