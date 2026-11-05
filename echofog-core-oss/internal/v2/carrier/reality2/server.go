package reality2

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	quic "github.com/apernet/quic-go"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/quicx"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// CoverProfile is the manually configured cover of T2.6a. T2.6b measures it from the real
// site and re-checks it every uniform(5, 7) h; until then the operator supplies the group and
// the certificate the node must mirror. A listener without both refuses to start: it cannot
// reproduce an unknown cover, and serving with the wrong one is what a censor recognises.
type CoverProfile struct {
	// Group is the key exchange group the cover selects.
	Group tls.CurveID
	// Cert is the cover's certificate, re-issued with the cover's exact length (T2.6b).
	Cert *tls.Certificate
}

// ListenerConfig configures a Listener.
type ListenerConfig struct {
	// Cover is the manual cover profile; required.
	Cover CoverProfile
	// Masks are the live masking keys (at most 8, wire.md §2.1 D10).
	Masks []MaskKey
	// Addrs are the exact host:port strings this node's cards list.
	Addrs []string
	// Replay is the replay cache shared by every listener with the same masking key. A nil
	// cache is created locally, but then it is not shared.
	Replay *Replay
	// CoverAddr is the cover site's UDP address; required.
	CoverAddr *net.UDPAddr
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
	// OnError receives replay persistence errors and cover socket errors; nil ignores them.
	OnError func(error)
	// ServerTLS, when set, is the TLS config of the admitted connections instead of one built
	// from Cover. The own-cover mode (T2.6b-2) passes the very config its website uses, so the
	// two cannot drift apart. NextProtos must contain "h3"; Cover.Cert may then be nil.
	ServerTLS *tls.Config
	// QUICConfig, when set, replaces the default QUIC config. The own-cover mode passes the
	// config its HTTP/3 website uses, so the transport parameters match.
	QUICConfig *quic.Config
}

// Listener accepts reality2-quic connections: the gate verifies tokens, the cover answers
// everyone else, and admitted connections are handed to the QUIC stack.
type Listener struct {
	gate   *Gate
	ql     *quic.Listener
	cover  *UDPCover
	replay *Replay

	closeOnce sync.Once
	closeErr  error
}

// ErrCover reports a listener without a reproducible cover profile.
var ErrCover = errors.New("reality2: the cover profile is required until T2.6b")

// Listen wraps conn (a bound UDP socket) in the server gate and starts the QUIC listener.
// The listener owns conn and closes it.
func Listen(conn net.PacketConn, cfg ListenerConfig) (*Listener, error) {
	if cfg.ServerTLS == nil && (cfg.Cover.Group == 0 || cfg.Cover.Cert == nil) {
		return nil, ErrCover
	}
	if cfg.CoverAddr == nil {
		return nil, errors.New("reality2: cover address is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	cover, err := NewUDPCover(CoverConfig{
		Addr:    cfg.CoverAddr,
		Reply:   func(p []byte, src net.Addr) { _, _ = conn.WriteTo(p, src) },
		Now:     now,
		OnError: cfg.OnError,
	})
	if err != nil {
		return nil, err
	}
	replay := cfg.Replay
	if replay == nil {
		replay = NewReplay(ReplayConfig{Now: now, OnError: cfg.OnError})
	}
	replay.acquire()
	gate, err := NewGate(conn, GateConfig{
		Masks:  cfg.Masks,
		Addrs:  cfg.Addrs,
		Cover:  cover,
		Replay: replay,
		Now:    now,
	})
	if err != nil {
		replay.release()
		_ = cover.Close()
		return nil, err
	}

	serverTLS := cfg.ServerTLS
	if serverTLS == nil {
		_, serverTLS = quicx.Reality2TLSConfig(cfg.Cover.Group, cfg.Cover.Cert)
	}
	qconf := cfg.QUICConfig
	if qconf == nil {
		qconf = &quic.Config{
			EnableDatagrams:       true,
			MaxIncomingStreams:    256,
			MaxIncomingUniStreams: -1, // mux.Carrier has no unidirectional streams
		}
	}
	ql, err := quic.Listen(gate, serverTLS, qconf)
	if err != nil {
		replay.release()
		_ = cover.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("reality2: listen: %w", err)
	}
	return &Listener{gate: gate, ql: ql, cover: cover, replay: replay}, nil
}

// Accept returns the next admitted connection as a carrier.
func (l *Listener) Accept(ctx context.Context) (mux.Carrier, error) {
	conn, err := l.ql.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return quicx.Wrap(conn, false), nil
}

// AcceptConn returns the next admitted connection as the raw QUIC connection, for a caller
// that needs its remote address before it wraps it (the own-cover mode drains its own check
// connections). Use Accept or AcceptConn, not both.
func (l *Listener) AcceptConn(ctx context.Context) (*quic.Conn, error) { return l.ql.Accept(ctx) }

// Wrap makes a carrier of a connection returned by AcceptConn.
func Wrap(conn *quic.Conn) mux.Carrier { return quicx.Wrap(conn, false) }

// Addr is the local address of the gate's socket.
func (l *Listener) Addr() net.Addr { return l.gate.LocalAddr() }

// Close stops the listener, the replay flusher and every cover socket. It is idempotent.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.replay.release()
		err := l.ql.Close()
		if cerr := l.cover.Close(); err == nil {
			err = cerr
		}
		if cerr := l.gate.Close(); err == nil {
			err = cerr
		}
		l.closeErr = err
	})
	return l.closeErr
}

var _ carrier.Listener = (*Listener)(nil)
