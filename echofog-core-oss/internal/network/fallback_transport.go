package network

import (
	"context"
	"errors"
	"fmt"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Transport modes for FallbackTransport.
const (
	ModeAuto = "auto" // QUIC first, TCP when QUIC does not get through
	ModeQUIC = "quic"
	ModeTCP  = "tcp" // networks where UDP is blocked outright
)

const (
	quicAttemptTimeout = 4 * time.Second
	// tcpPreferenceTTL: once QUIC failed for an address, use TCP first for a while.
	tcpPreferenceTTL = 10 * time.Minute
)

// FallbackTransport is the node's Transport: it listens on QUIC and on TCP (same port number)
// and dials QUIC first, falling back to TCP/TLS mimicry when UDP is blocked or throttled.
type FallbackTransport struct {
	QUIC *QuicTransport
	TCP  *TCPTransport // nil: QUIC only

	mode     string
	tcpAddr  string
	tcpRPort int // when non-zero, dial peers' TCP fallback on this fixed port (e.g. 443)
	advPort  int // when non-zero, the port announced to inbound peers for the TCP fallback

	mu      sync.Mutex
	preferT map[string]time.Time // addr -> until when TCP goes first

	connChan  chan NewConnEvent
	closeOnce sync.Once
	done      chan struct{}
	logger    *slog.Logger
}

// NewFallbackTransport combines q and t. tcpListen is where t listens; empty means t raises no
// inbound listener and is used only to dial out (a pure client).
func NewFallbackTransport(q *QuicTransport, t *TCPTransport, mode, tcpListen string) *FallbackTransport {
	if mode == "" {
		mode = ModeAuto
	}
	f := &FallbackTransport{
		QUIC:     q,
		TCP:      t,
		mode:     mode,
		tcpAddr:  tcpListen,
		preferT:  make(map[string]time.Time),
		connChan: make(chan NewConnEvent, 32),
		done:     make(chan struct{}),
		logger:   slog.Default().With("component", "transport"),
	}
	var wg sync.WaitGroup
	forward := func(ch <-chan NewConnEvent) {
		defer wg.Done()
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				select {
				case f.connChan <- ev:
				case <-f.done:
					_ = ev.Conn.CloseWithError(ErrCodeNormalClose, "shutting down")
					return
				}
			case <-f.done:
				return
			}
		}
	}
	wg.Add(1)
	go forward(q.ConnChan())
	if t != nil {
		wg.Add(1)
		go forward(t.ConnChan())
	}
	go func() { wg.Wait(); close(f.connChan) }()
	return f
}

func (f *FallbackTransport) SetLogger(l *slog.Logger) {
	if l != nil {
		f.logger = l
	}
}

// SetTCPRemotePort makes TCP dials go to this fixed port (e.g. 443) instead of the address's own
// port, for networks that run the TCP fallback on a standard port separate from QUIC.
func (f *FallbackTransport) SetTCPRemotePort(p int) { f.tcpRPort = p }

// SetAdvertisedPort sets the TCP fallback port announced to inbound peers (for a port map).
func (f *FallbackTransport) SetAdvertisedPort(p int) { f.advPort = p }

// SetCaps sets what this node announces to peers, on both transports.
func (f *FallbackTransport) SetCaps(c handshake.Capability) {
	f.QUIC.SetCaps(c)
	if f.TCP != nil {
		f.TCP.SetCaps(c)
	}
}

// tcpDialAddr rewrites addr's port to the fixed TCP fallback port when one is configured.
func (f *FallbackTransport) tcpDialAddr(addr string) string {
	if f.tcpRPort == 0 {
		return addr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return net.JoinHostPort(host, strconv.Itoa(f.tcpRPort))
}

func (f *FallbackTransport) Addr() net.Addr                { return f.QUIC.Addr() }
func (f *FallbackTransport) ConnChan() <-chan NewConnEvent { return f.connChan }
func (f *FallbackTransport) GetSTUNConn() net.PacketConn   { return f.QUIC.GetSTUNConn() }

// TCPAddr is where TCP peers are accepted (nil when not listening).
func (f *FallbackTransport) TCPAddr() net.Addr {
	if f.TCP == nil {
		return nil
	}
	return f.TCP.Addr()
}

func (f *FallbackTransport) ListenEarly(ctx context.Context) error {
	if err := f.QUIC.ListenEarly(ctx); err != nil {
		return err
	}
	if f.TCP == nil {
		return nil
	}
	addr := f.tcpAddr
	if addr == "" {
		// A pure client: the TCP transport exists only to dial out when UDP is blocked. It raises no
		// inbound listener, so it never presents a server certificate or a mimicked SNI to a prober.
		// A node that wants to accept TCP peers sets network.tcp_listen_addr explicitly.
		return nil
	}
	return f.listenTCP(addr)
}

// ListenTCP raises the inbound TCP listener on addr after start-up: a public node that had no
// camouflage target at start runs on UDP only, and turns TCP on once it has one (the caller sets
// it with TCPTransport.SetCamouflage first).
func (f *FallbackTransport) ListenTCP(addr string) error {
	if f.TCP == nil {
		return errors.New("no TCP transport")
	}
	if f.TCP.Addr() != nil {
		return nil // already listening; a second listener would leak the first
	}
	return f.listenTCP(addr)
}

func (f *FallbackTransport) listenTCP(addr string) error {
	quicAddr := f.QUIC.Addr().String() // same port number, TCP instead of UDP
	if err := f.TCP.Listen(addr); err != nil {
		// a privileged port such as :443 may fail to bind (no root, a container): fall back to
		// the QUIC port so the node is still reachable over TCP, rather than not at all
		if addr != quicAddr {
			f.logger.Warn("TCP fallback could not bind the configured address, using the QUIC port instead", "addr", addr, "fallback", quicAddr, "err", err)
			if err2 := f.TCP.Listen(quicAddr); err2 != nil {
				f.logger.Warn("TCP fallback listener unavailable", "addr", quicAddr, "err", err2)
				return nil
			}
		} else {
			// not fatal: this node can still dial out over TCP, it just cannot be reached by it
			f.logger.Warn("TCP fallback listener unavailable", "addr", addr, "err", err)
			return nil
		}
	}
	// announce the port peers should dial us back on: the fixed network port if set, else the
	// actual TCP listener port (which with a 443 listener is not the QUIC port)
	adv := f.advPort
	if adv == 0 {
		adv = f.tcpRPort
	}
	if adv == 0 {
		if ta, ok := f.TCP.Addr().(*net.TCPAddr); ok {
			adv = ta.Port
		}
	}
	f.TCP.SetAdvertisedPort(adv)
	return nil
}

// ExpectPeer tells both transports whose address addr is; see PeerExpecter.
func (f *FallbackTransport) ExpectPeer(addr string, id types.PeerID) {
	f.QUIC.ExpectPeer(addr, id)
	if f.TCP != nil {
		f.TCP.ExpectPeer(f.tcpDialAddr(addr), id)
	}
}

// DialEarly connects to addr ("host:port", the peer's QUIC address; its TCP listener uses the
// same port number).
func (f *FallbackTransport) DialEarly(ctx context.Context, addr string) error {
	if f.TCP == nil || f.mode == ModeQUIC {
		return f.QUIC.DialEarly(ctx, addr)
	}
	if f.mode == ModeTCP {
		return f.TCP.Dial(ctx, f.tcpDialAddr(addr))
	}
	if f.tcpFirst(addr) {
		if err := f.TCP.Dial(ctx, f.tcpDialAddr(addr)); err == nil {
			return nil
		}
		return f.QUIC.DialEarly(ctx, addr)
	}

	// QUIC gets at most half of the caller's budget, so TCP still has time when UDP is dead
	budget := quicAttemptTimeout
	if d, ok := ctx.Deadline(); ok {
		if half := time.Until(d) / 2; half < budget {
			budget = half
		}
	}
	qctx, cancel := context.WithTimeout(ctx, budget)
	qerr := f.QUIC.DialEarly(qctx, addr)
	cancel()
	if qerr == nil || ctx.Err() != nil {
		return qerr
	}
	if errors.Is(qerr, errSelfDial) {
		return qerr
	}
	terr := f.TCP.Dial(ctx, f.tcpDialAddr(addr))
	if terr != nil {
		return fmt.Errorf("quic: %v; tcp: %w", qerr, terr)
	}
	f.mu.Lock()
	if len(f.preferT) > 4096 {
		f.preferT = make(map[string]time.Time)
	}
	f.preferT[addr] = time.Now().Add(tcpPreferenceTTL)
	f.mu.Unlock()
	f.logger.Info("QUIC did not get through, connected over TCP", "addr", addr, "quic_err", qerr)
	return nil
}

func (f *FallbackTransport) tcpFirst(addr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	until, ok := f.preferT[addr]
	if ok && time.Now().After(until) {
		delete(f.preferT, addr)
		return false
	}
	return ok
}

func (f *FallbackTransport) Close() error {
	var err error
	f.closeOnce.Do(func() {
		close(f.done)
		if f.TCP != nil {
			_ = f.TCP.Close()
		}
		err = f.QUIC.Close()
	})
	return err
}
