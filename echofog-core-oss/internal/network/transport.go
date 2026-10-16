package network

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/dpi"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/quic-go/quic-go"
)

type QuicErrorCode = quic.ApplicationErrorCode

const (
	ErrCodeNoError QuicErrorCode = iota
	ErrCodeProtocolViolation
	ErrCodeSpamDetected
	ErrCodeStreamError
	ErrCodeAuthFailed
	ErrCodeNormalClose
)

// errSelfDial: the address is this node's own listener.
var errSelfDial = errors.New("DialEarly: address is already in use")

// GetQuicConfig returns the QUIC settings shared by the listener and every dial.
func GetQuicConfig() *quic.Config {
	return &quic.Config{
		Allow0RTT:             true,
		KeepAlivePeriod:       10 * time.Second,
		MaxIdleTimeout:        30 * time.Second,
		MaxIncomingStreams:    1000,
		MaxIncomingUniStreams: 1000,
		EnableDatagrams:       true,
	}
}

// Transport defines the common L4 P2P transport interface.
type Transport interface {
	Addr() net.Addr
	ListenEarly(ctx context.Context) error
	DialEarly(ctx context.Context, addr string) error
	ConnChan() <-chan NewConnEvent
	Close() error
	GetSTUNConn() net.PacketConn
}

// TransportOptions configures advanced network socket behavior
type TransportOptions struct {
	DirectSocket      bool // Bypass DPI/STUN multiplexing for raw 1Gbps+ performance on public IPs
	SocketBufferBytes int  // Kernel UDP socket buffer size, default 32MB if DirectSocket
	// Obfs turns on per-node UDP obfuscation: every packet looks like random bytes, keyed by the
	// peer ID of the node it goes to. Dialing then needs the peer ID (see ExpectPeer).
	Obfs bool
	// OwnID is this node's peer ID; it keys the packets that peers send to us.
	OwnID types.PeerID
	// ObfsSecret is an optional extra secret mixed into every key, for a private network.
	ObfsSecret string
}

type QuicTransport struct {
	tr              *quic.Transport
	tlsCfg          *tls.Config
	quicCgf         *quic.Config
	protocolVersion uint32

	addrNet net.Addr
	addrStr string

	connChan chan NewConnEvent

	authenticator  PeerAuthenticator
	advertisedPort atomic.Uint32 // public port peers dial us on (port map); 0: the UDP port
	handshakeSem   chan struct{}
	logger         *slog.Logger

	closeOnce      sync.Once
	dialMu         sync.Mutex
	dialing        map[string]*pendingDial           // addr -> dial in flight
	sessionCaches  map[string]tls.ClientSessionCache // addr -> its own 0-RTT ticket store
	Mux            *dpi.MuxedConn
	directStunConn net.PacketConn
	directSocket   bool
	udpConn        *net.UDPConn
	closed         chan struct{}  // closed by Close; connChan itself is never closed (senders race)
	arrivals       arrivalWaiters // hole punching waits for the peer to show up

	obfs       *dpi.NodeObfs // nil: packets go out as they are
	obfsSecret string
	book       peerBook
	guard      atomic.Pointer[handshake.Guard] // limits what one address can do before it is authenticated
}

// NewQUICTransport creates a QUIC transport listening on addr with default socket options.
func NewQUICTransport(addr string, tlsCfg *tls.Config, quicCgf *quic.Config, signer handshake.SignerGetterKey, protocolVersion uint32) (*QuicTransport, error) {
	return NewQUICTransportWithOptions(addr, tlsCfg, quicCgf, signer, protocolVersion, TransportOptions{
		DirectSocket: false,
	})
}

// NewQUICTransportWithOptions creates a QUIC transport with explicit socket and obfuscation options.
func NewQUICTransportWithOptions(addr string, tlsCfg *tls.Config, quicCgf *quic.Config, signer handshake.SignerGetterKey, protocolVersion uint32, opts TransportOptions) (*QuicTransport, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve UDP address %s: %w", addr, err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen UDP on %s: %w", addr, err)
	}

	targetBufferSize := opts.SocketBufferBytes
	if targetBufferSize <= 0 {
		if opts.DirectSocket {
			targetBufferSize = 32 * 1024 * 1024 // 32MB for high-throughput
		} else {
			targetBufferSize = 7500000 // 7.5MB default
		}
	}

	if err = udpConn.SetReadBuffer(targetBufferSize); err != nil {
		slog.Warn("Network: Could not set UDP read buffer to max", "err", err)
	}
	if err = udpConn.SetWriteBuffer(targetBufferSize); err != nil {
		slog.Warn("Network: Could not set UDP write buffer to max", "err", err)
	}

	var tr *quic.Transport
	var mux *dpi.MuxedConn
	var obfs *dpi.NodeObfs
	if opts.Obfs {
		obfs, err = dpi.NewNodeObfs(dpi.NodeKey(opts.ObfsSecret, opts.OwnID))
		if err != nil {
			_ = udpConn.Close()
			return nil, fmt.Errorf("network: obfuscation: %w", err)
		}
	}
	obfsActive := obfs != nil

	if opts.DirectSocket {
		// High-Throughput Direct Mode (Public IP / Exit Nodes / Satellite):
		// Connect raw or obfuscated *net.UDPConn directly to quic.Transport!
		// Enables hardware GSO & batch syscalls, achieving 1 Gbps+ throughput.
		var pConn net.PacketConn = udpConn
		if obfsActive {
			pConn = dpi.WrapPacketConn(udpConn, obfs)
			slog.Info("Network: per-node UDP obfuscation active (DirectSocket)", "addr", addr)
		}

		tr = &quic.Transport{
			Conn: pConn,
		}
		slog.Info("Network: High-throughput DirectSocket mode initialized (1Gbps+ capable, GSO enabled)", "addr", addr, "obfs", obfsActive)
	} else {
		// NAT Traversal Mode:
		// Multiplex STUN and QUIC over the same local port for UDP Hole Punching.
		mux, err = dpi.NewMuxedConn(udpConn)
		if err != nil || mux == nil {
			_ = udpConn.Close()
			return nil, fmt.Errorf("network: could not create DPI muxed connection: %w", err)
		}

		var pConn net.PacketConn = mux.GetQUICConn()
		if obfsActive {
			pConn = dpi.WrapPacketConn(pConn, obfs)
			slog.Info("Network: per-node UDP obfuscation active (Muxed)", "addr", addr)
		}

		tr = &quic.Transport{
			Conn: pConn,
		}
	}

	const maxConcurrentHandshakes = 64

	qt := &QuicTransport{
		tr:              tr,
		tlsCfg:          tlsCfg,
		quicCgf:         quicCgf,
		connChan:        make(chan NewConnEvent, 32),
		closed:          make(chan struct{}),
		protocolVersion: protocolVersion,
		addrNet:         udpConn.LocalAddr(),
		udpConn:         udpConn,
		addrStr:         addr,
		Mux:             mux,
		obfs:            obfs,
		obfsSecret:      opts.ObfsSecret,
		directSocket:    opts.DirectSocket,
		authenticator:   NewDefaultPeerAuthenticator(signer, protocolVersion, 5*time.Second),
		handshakeSem:    make(chan struct{}, maxConcurrentHandshakes),
		logger:          slog.Default().With("component", "quic_transport"),
	}

	qt.guard.Store(handshake.NewGuard(handshake.GuardConfig{}))
	qt.announceListenPort()
	return qt, nil
}

// BindToInterface pins the node's UDP socket to a network interface (0 unpins). Whole-computer
// mode needs it on Windows so peer traffic keeps using the physical link instead of the TUN.
func (q *QuicTransport) BindToInterface(ifIndex int) error {
	if q.udpConn == nil {
		return errors.New("transport has no UDP socket")
	}
	return bindUDPToInterface(q.udpConn, ifIndex)
}

// UDPPort is the local port of the node's UDP socket.
func (q *QuicTransport) UDPPort() int {
	if a, ok := q.addrNet.(*net.UDPAddr); ok {
		return a.Port
	}
	return 0
}

// SetLogger sets the logger for this transport.
func (q *QuicTransport) SetLogger(logger *slog.Logger) {
	if logger != nil {
		q.logger = logger
	}
}

// SetAuthenticator sets the peer handshake used to authenticate new connections.
func (q *QuicTransport) SetAuthenticator(a PeerAuthenticator) {
	if a != nil {
		q.authenticator = a
		q.announceListenPort()
	}
}

// SetAdvertisedPort sets the port announced to the peers this node dials, when it differs from
// the UDP port because of a port map; 0 announces the UDP port.
func (q *QuicTransport) SetAdvertisedPort(p int) {
	q.advertisedPort.Store(uint32(p))
	q.announceListenPort()
}

func (q *QuicTransport) announceListenPort() {
	p := int(q.advertisedPort.Load())
	if p == 0 {
		p = q.UDPPort()
	}
	if s, ok := q.authenticator.(ListenPortSetter); ok {
		s.SetListenPort(p)
	}
}

// STUNSharesQUICPort reports whether the socket GetSTUNConn returns is the QUIC port itself (muxed
// mode). In DirectSocket mode it is a separate socket, and what STUN says about it is the
// mapping of another port, useless for reaching our QUIC listener.
func (q *QuicTransport) STUNSharesQUICPort() bool { return q.Mux != nil }

// GetSTUNConn returns the packet connection STUN shares with QUIC, or nil when they are separate.
func (q *QuicTransport) GetSTUNConn() net.PacketConn {
	if q.Mux != nil {
		return q.Mux.GetSTUNConn()
	}
	// In DirectSocket mode, create a dedicated ephemeral socket for STUN discovery if needed
	if q.directStunConn == nil {
		conn, err := net.ListenPacket("udp", ":0")
		if err != nil {
			q.logger.Warn("QuicTransport: failed to create ephemeral STUN conn", "err", err)
			return nil
		}
		q.directStunConn = conn
	}
	return q.directStunConn
}

// Addr returns the local UDP address the transport is bound to.
func (q *QuicTransport) Addr() net.Addr {
	return q.addrNet
}

// Close stops listening and tears the transport down.
func (q *QuicTransport) Close() error {
	var err error
	q.closeOnce.Do(func() {
		if q.Mux != nil {
			q.Mux.Close()
		}
		if q.directStunConn != nil {
			_ = q.directStunConn.Close()
		}
		close(q.closed)
		err = q.tr.Close()
	})
	return err
}

// ConnChan delivers each newly authenticated peer connection.
func (q *QuicTransport) ConnChan() <-chan NewConnEvent {
	return q.connChan
}

// SetCaps sets what this node announces to peers in the handshake.
func (q *QuicTransport) SetCaps(c handshake.Capability) {
	if s, ok := q.authenticator.(CapsSetter); ok {
		s.SetCaps(c)
	}
}

// SetGuard replaces the per-address limits of inbound connections (tests, tuning).
func (q *QuicTransport) SetGuard(g *handshake.Guard) {
	if g != nil {
		q.guard.Store(g)
	}
}

func addrOfNet(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.AddrPort().Addr().Unmap()
	case *net.TCPAddr:
		return v.AddrPort().Addr().Unmap()
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}
