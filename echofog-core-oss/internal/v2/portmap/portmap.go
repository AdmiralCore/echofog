// Package portmap asks the home router to forward the node's ports: PCP, NAT-PMP, then UPnP
// (T6.5). The external address a router reports is only a candidate: it is exposed through
// Candidates once a peer has confirmed the public IP (T3.4, Confirm). With Config.Enabled false
// (profile port_mapping, off on mobile) the package sends nothing to anyone.
package portmap

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// Proto is the transport of a forwarded port.
type Proto string

// Transports a mapping can carry.
const (
	UDP Proto = "udp"
	TCP Proto = "tcp"
)

// PacketConn is a UDP socket connected to the gateway.
type PacketConn interface {
	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	Close() error
}

// HTTPDoer is the HTTP client used for UPnP. It must not follow redirects.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config configures a Manager. Nil fields select the production implementation.
type Config struct {
	// Enabled is the port_mapping profile flag. False: no packet is ever sent.
	Enabled bool
	// Clock is the injectable time source (AD13).
	Clock carrier.Clock
	// Gateway returns the default gateway.
	Gateway func() (netip.Addr, error)
	// Local returns our address on the gateway's network.
	Local func(gw netip.Addr) (netip.Addr, error)
	// Dial opens a UDP socket connected to a gateway port.
	Dial func(ctx context.Context, remote netip.AddrPort) (PacketConn, error)
	// HTTP is used for the UPnP description and control requests.
	HTTP HTTPDoer
	// SSDP returns UPnP device description URLs found on the network.
	SSDP func(ctx context.Context) ([]string, error)
	// Rand returns a value in [0,1): jitter and port choice.
	Rand func() float64
	// Nonce is the source of PCP mapping nonces.
	Nonce io.Reader
	// Lifetime is the mapping lifetime asked for; zero means one hour.
	Lifetime time.Duration
	// Port is the PCP and NAT-PMP server port; zero means 5351.
	Port uint16
	// OnChange is called, outside any lock, after the candidate set may have changed.
	OnChange func()
}

// Mapping is one forwarded port.
type Mapping struct {
	Proto    Proto
	Internal uint16
	External netip.AddrPort
	// Via is the protocol that opened it: "PCP", "NAT-PMP" or "UPnP".
	Via string
}

// ErrDisabled is returned by Add when port mapping is off.
var ErrDisabled = errors.New("portmap: port mapping is disabled")

// ErrTooMany is returned by Add when the port limit is reached.
var ErrTooMany = errors.New("portmap: too many ports")

// Manager keeps a set of ports forwarded and renewed.
type Manager struct{}

// New returns a Manager. Nothing is sent until the first Add.
func New(cfg Config) *Manager { return &Manager{} }

// Add asks for the port to be forwarded and keeps it so.
func (m *Manager) Add(proto Proto, port uint16) error { return nil }

// Remove stops renewing a port and asks the gateway to drop it.
func (m *Manager) Remove(proto Proto, port uint16) {}

// Mappings returns the ports the gateway has forwarded to a public address.
func (m *Manager) Mappings() []Mapping { return nil }

// Confirm tells the manager the public IP peers see (T3.4 Observer).
func (m *Manager) Confirm(ip netip.Addr) {}

// Candidates returns the mappings whose external IP peers have confirmed.
func (m *Manager) Candidates() []Mapping { return nil }

// Close drops every mapping and stops the manager.
func (m *Manager) Close(ctx context.Context) error { return nil }
