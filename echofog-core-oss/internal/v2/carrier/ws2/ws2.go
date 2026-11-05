// Package ws2 is the ws2 masking suite (wire.md §4.4): reality2-tcp inside the binary messages of
// a WebSocket that reaches the node through a CDN or a reverse proxy.
package ws2

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

var errTODO = errors.New("ws2: not implemented")

// MaxMessage is the largest binary message either side sends or accepts (wire.md §4.4).
const MaxMessage = 16 << 10

// ParseURL returns the token address (host:port, port 443 by default) and the request path of a
// ws2 address URL.
func ParseURL(s string) (hostPort, path string, err error) { return "", "", errTODO }

// Config configures a Listener.
type Config struct {
	Path               string
	Cover              reality2.TCPCover
	ServerTLS          *tls.Config
	Masks              []reality2.MaskKey
	Addrs              []string
	Replay             *reality2.Replay
	Site               http.Handler
	ClientIPHeader     string
	MaxConns           int
	MaxPreToken        int
	MaxPerAddr         int
	FirstRecordTimeout time.Duration
	Shape              *shape.Profile
	Delay              func() time.Duration
	Now                func() time.Time
}

// Listener is the server side of ws2.
type Listener struct{}

var _ carrier.Listener = (*Listener)(nil)

// New makes a Listener that is also an http.Handler.
func New(cfg Config) (*Listener, error) { return nil, errTODO }

// Listen serves the Listener over plain HTTP on ln.
func Listen(ln net.Listener, cfg Config) (*Listener, error) { return nil, errTODO }

func (l *Listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {}

func (l *Listener) Accept(ctx context.Context) (mux.Carrier, error) { return nil, errTODO }
func (l *Listener) Addr() net.Addr                                  { return nil }
func (l *Listener) Close() error                                    { return nil }

// DialerConfig configures a Dialer.
type DialerConfig struct {
	Hello   utls.ClientHelloID
	RootCAs *x509.CertPool
	Now     func() time.Time
}

// Dialer dials ws2 addresses.
type Dialer struct{}

var _ carrier.Dialer = (*Dialer)(nil)

func NewDialer(cfg DialerConfig) *Dialer { return &Dialer{} }

func (d *Dialer) Dial(ctx context.Context, req carrier.DialRequest) (mux.Carrier, error) {
	return nil, errTODO
}

// Register installs the suite in reg.
func Register(reg *carrier.Registry, d *Dialer, l *Listener) error { return errTODO }

// newConn adapts a WebSocket to a net.Conn of binary messages; remote overrides RemoteAddr.
func newConn(ws any, remote net.Addr) net.Conn { return nil }
