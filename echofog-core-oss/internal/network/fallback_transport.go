package network

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// transportRaceStagger: the preferred transport gets this head start before the other is
	// launched alongside it, so a working network pays nothing for the race.
	transportRaceStagger = 300 * time.Millisecond
	// envMemoryTTL: how long the transport that won on a given local network is remembered.
	envMemoryTTL = 30 * time.Minute
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
	noDial  atomic.Bool          // an isolated node: it only accepts connections

	envMu    sync.Mutex
	envCache string               // fingerprint of the current local network, cached briefly
	envAt    time.Time            // when envCache was taken
	envMem   map[string]envRecord // network fingerprint -> the transport that won there last

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
	if t != nil {
		t.SetGuard(q.Guard()) // one set of per-address limits for the node, whatever the transport
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

// SetAdvertisedPort sets the port announced to the peers this node dials, for QUIC and the TCP
// fallback (for a port map).
func (f *FallbackTransport) SetAdvertisedPort(p int) {
	f.advPort = p
	f.QUIC.SetAdvertisedPort(p)
}

// SetCaps sets what this node announces to peers, on both transports.
func (f *FallbackTransport) SetCaps(c handshake.Capability) {
	f.QUIC.SetCaps(c)
	if f.TCP != nil {
		f.TCP.SetCaps(c)
	}
}

// SetAccess sets the hidden-node gate and the invitation keyring on both transports; hide stops
// announcing where this node listens.
func (f *FallbackTransport) SetAccess(g *access.Gate, k *access.Keyring, hide bool) {
	f.QUIC.SetAccess(g, k, hide)
	if f.TCP != nil {
		f.TCP.SetAccess(g, k, hide)
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

// SetCamouflage configures both QUIC and TCP transports to mimic the camouflage site.
func (f *FallbackTransport) SetCamouflage(cert tls.Certificate, serverName, fallbackAddr string) {
	if f.QUIC != nil {
		f.QUIC.SetCamouflage(cert, serverName, fallbackAddr)
	}
	if f.TCP != nil {
		f.TCP.SetCamouflage(cert, serverName, fallbackAddr)
	}
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

// SetRegionNoise installs the region policy on the QUIC transport: a camouflaged peer the policy
// places in this node's own region is dialed over noise instead of QUIC-REALITY. The TCP fallback
// keeps its TLS mimicry — a domestic network rarely blocks UDP, so the noise QUIC path is the one
// used there, and TCP mimicry stays available for the cross-border case.
func (f *FallbackTransport) SetRegionNoise(fn func(addr string) bool) {
	f.QUIC.SetRegionNoise(fn)
}

// ExpectPeerSNI tells both transports whose address addr is and its camouflage SNI; see PeerExpecter.
func (f *FallbackTransport) ExpectPeerSNI(addr string, id types.PeerID, sni string) {
	f.QUIC.ExpectPeerSNI(addr, id, sni)
	if f.TCP != nil {
		f.TCP.ExpectPeerSNI(f.tcpDialAddr(addr), id, sni)
	}
}

// ErrNoDial: this node is isolated and connects to nobody; the people it invited connect to it.
var ErrNoDial = errors.New("this node is isolated: it does not connect to other nodes")

// SetNoDial stops (or allows again) every outbound connection: an isolated node only accepts.
func (f *FallbackTransport) SetNoDial(on bool) { f.noDial.Store(on) }

// DialEarly connects to addr ("host:port", the peer's QUIC address; its TCP listener uses the
// same port number).
func (f *FallbackTransport) DialEarly(ctx context.Context, addr string) error {
	if f.noDial.Load() {
		return ErrNoDial
	}
	if f.TCP == nil || f.mode == ModeQUIC {
		return f.QUIC.DialEarly(ctx, addr)
	}
	if f.mode == ModeTCP {
		return f.TCP.Dial(ctx, f.tcpDialAddr(addr))
	}
	return f.raceDial(ctx, addr)
}

// raceDial launches the QUIC and TCP-fallback dials together (happy-eyeballs at the transport
// level) and takes whichever connects first, cancelling the loser. The transport known to work on
// this network — this address stalled QUIC before (preferT), or UDP lost here last time
// (per-network memory) — gets a head start, so a settled network pays nothing for the race while a
// freshly UDP-blocked one no longer waits out a full QUIC timeout before it ever tries TCP. If both
// transports still connect, the swarm drops the duplicate by peer ID.
func (f *FallbackTransport) raceDial(ctx context.Context, addr string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tcpFirst := f.tcpFirst(addr) || f.envPrefersTCP()

	type result struct {
		tcp bool
		err error
	}
	results := make(chan result, 2)

	quicBudget := quicAttemptTimeout
	if d, ok := ctx.Deadline(); ok {
		if left := time.Until(d); left < quicBudget {
			quicBudget = left
		}
	}
	dialQUIC := func() {
		qctx, qcancel := context.WithTimeout(ctx, quicBudget)
		err := f.QUIC.DialEarly(qctx, addr)
		qcancel()
		results <- result{tcp: false, err: err}
	}
	dialTCP := func() {
		results <- result{tcp: true, err: f.TCP.Dial(ctx, f.tcpDialAddr(addr))}
	}

	// start the preferred transport now, the other after the stagger (or at once if the first one
	// fails before then)
	var startOther func()
	if tcpFirst {
		go dialTCP()
		startOther = func() { go dialQUIC() }
	} else {
		go dialQUIC()
		startOther = func() { go dialTCP() }
	}

	timer := time.NewTimer(transportRaceStagger)
	defer timer.Stop()

	var quicErr, tcpErr error
	haveQUICErr, haveTCPErr := false, false
	for pending := 1; pending > 0; {
		select {
		case <-timer.C:
			if startOther != nil {
				startOther()
				startOther = nil
				pending++
			}
		case r := <-results:
			pending--
			if r.err == nil {
				cancel() // the loser gives up; if it still connects the swarm drops the duplicate
				if r.tcp {
					f.onTCPWon(addr)
				} else {
					f.recordEnv(false)
				}
				return nil
			}
			if r.tcp {
				tcpErr, haveTCPErr = r.err, true
			} else {
				quicErr, haveQUICErr = r.err, true
				if errors.Is(r.err, errSelfDial) {
					return r.err
				}
			}
			if startOther != nil {
				startOther() // the first transport failed before the stagger: start the other now
				startOther = nil
				pending++
			}
		}
	}

	switch {
	case haveQUICErr && haveTCPErr:
		return fmt.Errorf("quic: %v; tcp: %w", quicErr, tcpErr)
	case haveTCPErr:
		return tcpErr
	default:
		return quicErr
	}
}

// onTCPWon records that TCP got through on this network and this address: TCP goes first next time
// there, so a UDP-blocked network stops paying the QUIC head start on every connection.
func (f *FallbackTransport) onTCPWon(addr string) {
	f.recordEnv(true)
	if !f.tcpFirst(addr) {
		f.logger.Info("QUIC did not get through, connected over TCP", "addr", addr)
	}
	f.preferTCP(addr)
}

// PreferTCP makes the dials to addr try TCP first for a while: QUIC got through the handshake
// there but then stopped carrying anything, as networks that throttle UDP after the first packets
// do. A dial that finds no TCP listener still falls back to QUIC.
func (f *FallbackTransport) PreferTCP(addr string) {
	if f.TCP == nil || f.mode != ModeAuto || addr == "" {
		return
	}
	if !f.tcpFirst(addr) {
		f.logger.Info("QUIC stalled after connecting: the next connections there try TCP first", "addr", addr, "for", tcpPreferenceTTL)
	}
	f.preferTCP(addr)
}

func (f *FallbackTransport) preferTCP(addr string) {
	f.mu.Lock()
	if len(f.preferT) > 4096 {
		f.preferT = make(map[string]time.Time)
	}
	f.preferT[addr] = time.Now().Add(tcpPreferenceTTL)
	f.mu.Unlock()
}

// IsIdleTimeout tells whether err is a QUIC connection that ended because nothing came from the
// peer for the idle timeout, keep-alives included: the path stopped carrying UDP.
func IsIdleTimeout(err error) bool {
	var idle *quic.IdleTimeoutError
	return errors.As(err, &idle)
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

// envRecord remembers which transport won on a given local network, and when.
type envRecord struct {
	tcp bool
	at  time.Time
}

// networkEnv fingerprints the local network environment as the sorted set of active, non-loopback
// interface addresses — a cheap, portable stand-in for "which network am I on". It changes when the
// device moves to another network, so the memory of a censored network does not bias the dials once
// the device is back on an open one. The value is cached for a few seconds to keep per-dial cost low.
func (f *FallbackTransport) networkEnv() string {
	f.envMu.Lock()
	if f.envCache != "" && time.Since(f.envAt) < 5*time.Second {
		env := f.envCache
		f.envMu.Unlock()
		return env
	}
	f.envMu.Unlock()

	ifaces, _ := net.Interfaces()
	var parts []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				parts = append(parts, ifi.Name+":"+ipn.IP.String())
			}
		}
	}
	sort.Strings(parts)
	env := strings.Join(parts, ",")

	f.envMu.Lock()
	f.envCache, f.envAt = env, time.Now()
	f.envMu.Unlock()
	return env
}

// envPrefersTCP reports whether TCP won on the current network recently.
func (f *FallbackTransport) envPrefersTCP() bool {
	env := f.networkEnv()
	f.envMu.Lock()
	defer f.envMu.Unlock()
	r, ok := f.envMem[env]
	if !ok {
		return false
	}
	if time.Since(r.at) > envMemoryTTL {
		delete(f.envMem, env)
		return false
	}
	return r.tcp
}

// recordEnv remembers that tcp (or QUIC) won on the current network.
func (f *FallbackTransport) recordEnv(tcp bool) {
	env := f.networkEnv()
	f.envMu.Lock()
	if f.envMem == nil || len(f.envMem) > 64 {
		f.envMem = make(map[string]envRecord)
	}
	f.envMem[env] = envRecord{tcp: tcp, at: time.Now()}
	f.envMu.Unlock()
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
