package noise3

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	quic "github.com/apernet/quic-go"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/packet"
	"github.com/echofog/echofog-core/internal/v2/carrier/quicx"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// DialerConfig configures the noise3 dialer.
type DialerConfig struct {
	// Clock is the injectable time source; nil means the system clock.
	Clock Clock
	// TLSConfig is the inner QUIC client TLS configuration; nil means quicx.Noise3TLSConfig.
	TLSConfig *tls.Config
	// QUICConfig is the inner QUIC configuration; nil means quicx.Noise3QUICConfig.
	QUICConfig *quic.Config
	// DialContext opens the outer UDP socket; nil means net.Dialer. DialRequest.DialContext
	// overrides it.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// LowPower disables idle padding (platforms.md §2).
	LowPower bool
	// MaxDatagramLen is the outer noise3 payload ceiling (path MTU minus IP and UDP headers).
	// Zero means packet.DefaultDatagramLen (1172, an IPv4 path of MTU 1200); values above 1452 are
	// clamped to it. It must be at least 900 (the lower bound of init_len, see doc.go).
	MaxDatagramLen int
}

// Dialer opens noise3 carriers. It satisfies carrier.Dialer and is registered under
// objects.SuiteNoise3.
type Dialer struct {
	cfg DialerConfig
}

var _ carrier.Dialer = (*Dialer)(nil)

// NewDialer returns a dialer. The zero DialerConfig is usable.
func NewDialer(cfg DialerConfig) *Dialer { return &Dialer{cfg: cfg} }

// Dial opens one noise3 flow to the address of a verified card and returns the inner QUIC
// connection as a mux.Carrier (wire.md §2, §5).
func (d *Dialer) Dial(ctx context.Context, req carrier.DialRequest) (mux.Carrier, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Card == nil || req.Addr == nil {
		return nil, fmt.Errorf("%w: card or address missing", ErrBadConfig)
	}
	if req.Addr.GetSuite() != objects.SuiteNoise3 {
		return nil, fmt.Errorf("%w: suite %d is not noise3", ErrBadConfig, req.Addr.GetSuite())
	}
	host := req.Addr.GetHost()
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	port := req.Addr.GetPort()
	if host == "" || port == 0 || port > 65535 {
		return nil, fmt.Errorf("%w: bad noise3 address", ErrBadConfig)
	}
	clock := d.cfg.Clock
	if clock == nil {
		clock = newSystemClock()
	}
	maskPub, err := cardMaskKey(req.Card, req.Addr, clock.Now())
	if err != nil {
		return nil, err
	}
	profile, err := addressProfile(req.Addr)
	if err != nil {
		return nil, err
	}
	tlsConf := d.cfg.TLSConfig
	if tlsConf == nil {
		client, _, err := quicx.Noise3TLSConfig()
		if err != nil {
			return nil, fmt.Errorf("noise3: inner TLS: %w", err)
		}
		tlsConf = client
	}
	quicConf := d.cfg.QUICConfig
	ceiling := l0MaxDatagram(d.cfg.MaxDatagramLen)
	flight := flightSize(profile, ceiling, nil)
	quicConf, err = innerQUICConfig(quicConf, maxPadding(profile), ceiling, flight-packet.InitOverhead)
	if err != nil {
		return nil, err
	}

	address := net.JoinHostPort(host, strconv.Itoa(int(port)))
	dial := d.cfg.DialContext
	if req.DialContext != nil {
		dial = req.DialContext
	}
	if _, err := netip.ParseAddr(host); err != nil && dial == nil {
		return nil, fmt.Errorf("%w: %q", ErrDNSName, host)
	}
	dialFn := func(ctx context.Context) (net.PacketConn, net.Addr, error) {
		var (
			c   net.Conn
			err error
		)
		if dial != nil {
			c, err = dial(ctx, "udp", address)
		} else {
			var nd net.Dialer
			c, err = nd.DialContext(ctx, "udp", address)
		}
		if err != nil {
			return nil, nil, err
		}
		return udpPacketConn(c), c.RemoteAddr(), nil
	}

	first, remote, err := dialFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("noise3: dial %s: %w", address, err)
	}
	if remote == nil {
		remote = &net.UDPAddr{}
	}
	dc := newDialConn(d.cfg, clock, profile, maskPub, dialFn, remote)
	dc.mu.Lock()
	dc.conn = first
	dc.mu.Unlock()
	go dc.readFrom(first)

	conn, err := quic.Dial(ctx, dc, clientVAddr, tlsConf, quicConf)
	if err != nil {
		_ = dc.Close()
		return nil, fmt.Errorf("noise3: inner QUIC: %w", err)
	}
	go func() {
		<-conn.Context().Done()
		_ = dc.Close()
	}()
	return quicx.WrapNoise3(conn, true), nil
}
