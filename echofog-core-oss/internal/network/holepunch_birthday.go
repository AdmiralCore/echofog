package network

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	mrand "math/rand/v2"

	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/network/dpi"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Birthday hole punching reaches a peer behind a symmetric NAT, which the ordinary DCUtR punch
// cannot: a symmetric NAT hands out a different external port for every destination, so the port a
// relay saw is not the one a packet to the peer comes from. The idea is the birthday paradox, as
// HyperDHT/Holepunch and Tailscale do it: the symmetric side opens many sockets at once (each gets
// its own external port), the cone side sprays packets across many ports of the symmetric side's
// IP, and with enough of each a packet lands on an open mapping. A carrier NAT that hands each
// subscriber a small block of ports needs only a few hundred of each to make it near-certain.
//
// Roles. The symmetric side runs BirthdayDial: it opens n UDP sockets, sends a small noise packet
// from each toward the cone peer's fixed address (opening n NAT mappings), and watches all n for a
// packet from the peer. The cone side runs BirthdaySpray: from its own listening socket it sends m
// noise packets to the symmetric side's IP across the port range the NAT was seen to use. When a
// spray packet reaches one of the symmetric side's sockets, that socket learns the path is open and
// dials the cone peer over QUIC from it; the others are closed. The packets are the same small
// random datagrams the ordinary punch sends (see PunchHole), so they add no new wire fingerprint.
//
// The three pacers in this file share one loop: every socket gets a packet in the first
// n/birthdayRate of the window, the rate stays near birthdayRate, and the spray budget fits inside
// the window instead of running past it. A per-socket timer would multiply the rate by n; a
// per-packet timer reset would accumulate the OS's wake-up lateness and lose packets at the end.

const (
	// maxBirthdaySockets caps how many sockets the symmetric side opens at once.
	maxBirthdaySockets = 256
	// maxBirthdaySpray caps how many packets the cone side sends in one attempt. It is the packet
	// budget that fits the punch window at birthdayRate (900 / 320 ≈ 2.8 s) with room for jitter.
	maxBirthdaySpray = 900
	// birthdayTimeout bounds the punch phase of one attempt.
	birthdayTimeout = 3 * time.Second
	// birthdayRate paces the punch to about this many packets a second (with jitter), so a burst of
	// hundreds of packets does not itself stand out to a censor on the path.
	birthdayRate = 320
	// birthdayNarrowRange is the widest observed port block still treated as a narrow range worth
	// sweeping directly; a wider spread means the NAT picks ports anywhere and the spray goes random
	// over the whole dynamic range.
	birthdayNarrowRange = 8000
	// birthdaySprayMargin is how far past the observed [lo, hi] the spray reaches on each side. The
	// observed range is only a few STUN samples (the NAT manager does not accumulate them), so it is a
	// small, random slice of the NAT's block, while the symmetric side's sockets land anywhere in the
	// whole block. The spray must therefore reach well past the samples to cover the block; the fixed
	// packet budget keeps the density up across several attempts (DCUtR's retry count).
	birthdaySprayMargin = 4000
	// birthdayDialBudget bounds the QUIC dial and handshake once a mapping is found.
	birthdayDialBudget = 5 * time.Second
	// maxBirthdayMutualSockets caps the sockets each side opens in the both-symmetric case; it is well
	// below the single-side cap because both ends run QUIC listeners on their sockets at once.
	maxBirthdayMutualSockets = 64
)

// ErrBirthdayNoSocket: not a single side socket could be opened (file-descriptor limit).
var ErrBirthdayNoSocket = errors.New("birthday punch: could not open any socket")

// birthdayDelay is the gap between two punch packets: a little over or under the rate's interval,
// so the average stays at birthdayRate while the flow is not machine-regular. The jitter is kept
// small (down to 3% under the interval) so the whole 900-packet budget fits inside the 3 s window
// even at the slow end (900 × 3.125 ms ≈ 2.81 s), leaving room for scheduling overhead.
func birthdayDelay() time.Duration {
	base := time.Second / birthdayRate
	// #nosec G404 -- pacing jitter, not a secret
	return base - base/32 + time.Duration(mrand.IntN(int(base/32)+1))
}

// birthdayPacer spaces packets from one fixed start instead of resetting a timer per packet. A
// reset is measured from the moment the timer wakes, so every late wake-up (Windows timers are
// routinely a millisecond or two late) would push every later packet back; over 900 packets that
// drift cut the spray budget short by a fifth. With absolute deadlines a late wake is absorbed by a
// shorter wait for the next packet, so the whole budget fits the window at the target rate.
type birthdayPacer struct {
	start time.Time
	due   time.Time
	timer *time.Timer
}

func newBirthdayPacer() *birthdayPacer {
	now := time.Now()
	return &birthdayPacer{start: now, due: now, timer: time.NewTimer(time.Hour)}
}

func (p *birthdayPacer) stop() { p.timer.Stop() }

// paceWait says why a wait ended.
type paceWait int

const (
	paceGo   paceWait = iota // the next packet is due
	paceStop                 // the stop channel fired (the peer arrived)
	paceCtx                  // the context ended (the window ran out)
)

// wait blocks until the next packet is due. A nil stop channel is never ready.
func (p *birthdayPacer) wait(ctx context.Context, stop <-chan struct{}) paceWait {
	if d := time.Until(p.due); d > 0 {
		p.timer.Reset(d)
		select {
		case <-ctx.Done():
			p.timer.Stop()
			return paceCtx
		case <-stop:
			p.timer.Stop()
			return paceStop
		case <-p.timer.C:
		}
		return paceGo
	}
	// behind schedule (a long wake-up or a stop that filled the scheduler): check the exits without
	// sleeping, so the schedule catches up rather than sliding for the rest of the window
	select {
	case <-ctx.Done():
		return paceCtx
	case <-stop:
		return paceStop
	default:
		return paceGo
	}
}

// advance schedules the next packet one (jittered) interval after the previous deadline.
func (p *birthdayPacer) advance() { p.due = p.due.Add(birthdayDelay()) }

// birthdayPaceLoop sends one packet through each of n writers in round-robin at about birthdayRate
// until stop closes or ctx ends. All three birthday pacers run on this loop, so the rate is the
// same however many sockets or transports a role owns.
func birthdayPaceLoop(ctx context.Context, stop <-chan struct{}, n int, write func(int, []byte)) {
	if n < 1 {
		return
	}
	payload := make([]byte, punchPayloadLen)
	pacer := newBirthdayPacer()
	defer pacer.stop()
	for i := 0; ; i++ {
		if pacer.wait(ctx, stop) != paceGo {
			return
		}
		if _, err := rand.Read(payload); err != nil {
			return
		}
		write(i%n, payload)
		pacer.advance()
	}
}

// BirthdayDial opens n fresh UDP sockets, punches each toward remoteAddr so the NAT maps a port for
// it, and watches all of them for a packet from the peer. The first socket to hear from the peer
// dials it over QUIC; the rest are closed. The connection arrives on ConnChan like any other. It is
// the symmetric side's half of a birthday punch; the peer must run BirthdaySpray at the same time.
func (q *QuicTransport) BirthdayDial(ctx context.Context, remoteAddr string, peer types.PeerID, n int) error {
	if n < 1 {
		n = 1
	}
	if n > maxBirthdaySockets {
		n = maxBirthdaySockets
	}
	ua, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return err
	}
	key := punchKey{addr: ua.String(), peer: peer}
	if !q.arrivals.begin(key) {
		return ErrPunchActive
	}
	defer q.arrivals.end(key)

	// Learn the peer's key before the first packet: the punch then carries the same obfuscation as
	// every other datagram to that node, and the peer can open it with its own key.
	q.ExpectPeer(ua.String(), peer)

	punchCtx, cancel := context.WithTimeout(ctx, birthdayTimeout)
	defer cancel()

	// one fresh socket per mapping we want the NAT to open
	socks := make([]net.PacketConn, 0, n)
	for i := 0; i < n; i++ {
		c, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if e != nil {
			// a descriptor limit (common on Windows): go on with what opened, fail only if none did
			break
		}
		socks = append(socks, q.wrapBirthdayConn(c))
	}
	if len(socks) == 0 {
		return ErrBirthdayNoSocket
	}

	remoteIP := ua.AddrPort().Addr().Unmap()
	hits := make(chan net.PacketConn, len(socks))
	stop := make(chan struct{})
	var once sync.Once
	stopAll := func() { once.Do(func() { close(stop) }) }
	var wg sync.WaitGroup

	// a reader per socket: the first packet from the peer's IP wins that socket
	for _, s := range socks {
		wg.Add(1)
		go func(s net.PacketConn) {
			defer wg.Done()
			buf := make([]byte, 1500)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				_, from, e := s.ReadFrom(buf)
				if e != nil {
					if ne, ok := e.(net.Error); ok && ne.Timeout() {
						continue
					}
					return
				}
				if ap, ok := from.(*net.UDPAddr); ok && ap.AddrPort().Addr().Unmap() == remoteIP {
					select {
					case hits <- s:
					default:
					}
					return
				}
			}
		}(s)
	}

	// one paced sender, round-robin across the sockets: each socket gets a packet in the first
	// len(socks)/birthdayRate of the window, so every NAT mapping opens and stays fresh. A per-socket
	// timer, or a per-socket interval, would either multiply the rate by n or starve all but the
	// first few sockets inside the window.
	wg.Add(1)
	go func() {
		defer wg.Done()
		birthdayPaceLoop(punchCtx, stop, len(socks), func(i int, payload []byte) {
			_, _ = socks[i].WriteTo(payload, ua)
		})
	}()

	var winner net.PacketConn
	select {
	case winner = <-hits:
	case <-punchCtx.Done():
	}
	stopAll()
	// close every socket but the winner (on timeout winner is nil, so all close); the winner's own
	// reader has already returned, so clearing its deadline hands a clean socket to the QUIC dial
	for _, s := range socks {
		if s != winner {
			_ = s.Close()
		}
	}
	wg.Wait()
	if winner == nil {
		return ErrHolePunchTimeout
	}
	_ = winner.SetReadDeadline(time.Time{})

	// the dial gets its own budget: the handshake outlives the punch phase
	dctx, dcancel := context.WithTimeout(ctx, birthdayDialBudget)
	defer dcancel()
	if err := q.birthdayDialOn(dctx, winner, ua, peer, true); err != nil {
		_ = winner.Close()
		return err
	}
	return nil
}

// wrapBirthdayConn puts the node's obfuscation on a fresh socket: the ordinary punch goes through
// the (already wrapped) main socket, so a raw birthday packet would be the only unsealed random
// datagram the node ever sends.
func (q *QuicTransport) wrapBirthdayConn(c *net.UDPConn) net.PacketConn {
	if q.obfs != nil {
		return dpi.WrapPacketConn(c, q.obfs)
	}
	return c
}

// birthdayDialOn dials peer over QUIC on an already-opened socket, the same way an ephemeral noise
// dial does (see dial): a one-connection transport runs on the socket (obfuscated when the node uses
// obfuscation), the connection is authenticated and handed to the swarm. dialable says whether the
// peer's address is a stable one (a listening socket) or a guessed ephemeral port. The socket is
// closed when the connection ends.
func (q *QuicTransport) birthdayDialOn(ctx context.Context, pconn net.PacketConn, target *net.UDPAddr, peer types.PeerID, dialable bool) error {
	q.ExpectPeer(target.String(), peer)

	tr := &quic.Transport{Conn: pconn}
	tlsCfg := q.tlsConfigFor(target.String())
	// The punch is a noise flow from beginning to end: a REALITY SNI picked up for this peer would
	// mask the first Initial for a camouflage listener that is not there, and the birthday sockets
	// have no cover site anyway.
	tlsCfg.ServerName = ""
	tlsCfg.NextProtos = []string{"echo-fog-protocole"}
	conn, err := tr.Dial(ctx, target, tlsCfg, q.quicCgf)
	if err != nil {
		_ = tr.Close()
		_ = pconn.Close()
		return fmt.Errorf("birthday dial: %w", err)
	}
	go func() {
		<-conn.Context().Done()
		_ = tr.Close()
		_ = pconn.Close()
	}()
	q.applyCongestion(conn)

	actx := WithExpectedPeer(ctx, peer)
	peerRes, err := q.authenticator.AuthenticateOutbound(actx, WrapQUIC(conn))
	if err != nil {
		conn.CloseWithError(ErrCodeAuthFailed, "auth failed")
		return fmt.Errorf("birthday dial auth: %w", err)
	}
	if peerRes.PeerID != peer {
		conn.CloseWithError(ErrCodeAuthFailed, "unexpected peer")
		return &PeerMismatchError{Got: peerRes.PeerID}
	}
	q.newConn(conn, true, conn.RemoteAddr().String(), dialable, peerRes.PeerID, peerRes.PubKey, peerRes.Caps, false, false)
	q.logger.Info("Birthday punch reached a peer behind a symmetric NAT", "peer", peerRes.PeerID, "remote", conn.RemoteAddr().String())
	return nil
}

// BirthdaySpray sprays m small noise packets from this node's listening socket across the peer's
// port range at ip, stopping as soon as an authenticated connection from peer arrives. lo..hi is the
// port block the peer's NAT was seen to use (from its NAT report); when it is a narrow block the
// spray sweeps around it, otherwise it goes random over the dynamic port range. It is the cone
// side's half of a birthday punch; the peer must run BirthdayDial at the same time.
func (q *QuicTransport) BirthdaySpray(ctx context.Context, ip string, lo, hi uint16, m int, peer types.PeerID) error {
	if m < 1 {
		m = 1
	}
	if m > maxBirthdaySpray {
		m = maxBirthdaySpray
	}
	host, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("birthday spray: bad ip %q: %w", ip, err)
	}
	host = host.Unmap()
	if !q.birthdayTargetOK(host) {
		return fmt.Errorf("birthday spray: %q is not a unicast address", ip)
	}
	// Every packet goes to a different guessed port, so the peer is known by IP; without this the
	// packets would be sealed with our own key and the symmetric side could not open them.
	q.ExpectPeerIP(host.String(), peer)

	arrived, cancel := q.arrivals.watch(peer)
	defer cancel()
	sctx, stop := context.WithTimeout(ctx, birthdayTimeout)
	defer stop()

	ports := birthdayPorts(lo, hi, m)
	payload := make([]byte, punchPayloadLen)
	if _, err := rand.Read(payload); err != nil {
		return err
	}
	q.logger.Debug("Birthday spray: sending", "target", host, "ports", len(ports), "lo", lo, "hi", hi)
	return q.birthdaySprayPorts(sctx, ports, func(p uint16) error {
		dst := net.UDPAddrFromAddrPort(netip.AddrPortFrom(host, p))
		if _, e := q.tr.WriteTo(payload, dst); e != nil {
			return fmt.Errorf("birthday spray: %w", e)
		}
		return nil
	}, arrived)
}

// birthdayTargetOK rejects addresses a peer must not make this node spray toward. A node bound to
// loopback is a test or single-machine setup; there loopback targets are allowed so two local
// nodes (and the tests) can punch. Everything else must be global unicast.
func (q *QuicTransport) birthdayTargetOK(host netip.Addr) bool {
	if !host.IsValid() || host.IsUnspecified() || host.IsMulticast() || host.IsLinkLocalUnicast() {
		return false
	}
	if host.IsLoopback() {
		la, ok := q.addrNet.(*net.UDPAddr)
		return ok && la.IP.IsLoopback()
	}
	return host.IsGlobalUnicast()
}

// birthdaySprayPorts sends ports at birthdayRate, stopping when the peer arrives, the window ends,
// or the list is exhausted. The list is already within the packet budget; the rate and the window
// guarantee the last packet goes out before the deadline, so a full sweep is not cut short.
func (q *QuicTransport) birthdaySprayPorts(ctx context.Context, ports []uint16, send func(uint16) error, arrived <-chan struct{}) error {
	if len(ports) > maxBirthdaySpray {
		ports = ports[:maxBirthdaySpray]
	}
	pacer := newBirthdayPacer()
	defer pacer.stop()
	for _, p := range ports {
		switch pacer.wait(ctx, arrived) {
		case paceGo:
		case paceStop:
			return nil
		default:
			select { // it may have arrived in the same instant
			case <-arrived:
				return nil
			default:
			}
			return ErrHolePunchTimeout
		}
		if err := send(p); err != nil {
			return err
		}
		pacer.advance()
	}
	// keep waiting out the window: the peer's dial and handshake may still be in flight
	select {
	case <-arrived:
		return nil
	case <-ctx.Done():
		return ErrHolePunchTimeout
	}
}

// birthdayPorts picks the m ports to spray. A narrow observed block (a carrier NAT's port range) is
// swept, widened by birthdaySprayMargin on each side to cover the neighbours; a wide or unknown
// spread goes random over the dynamic range 1024..65535.
func birthdayPorts(lo, hi uint16, m int) []uint16 {
	out := make([]uint16, 0, m)
	if lo >= 1024 && hi >= lo && int(hi-lo) <= birthdayNarrowRange {
		// The observed [lo, hi] comes from only a few samples (a handful of STUN answers), so it is a
		// subset of the NAT's real block: the symmetric side's sockets take ports across the whole
		// block, most outside the samples. Spray a window widened well past the samples on each side,
		// so it covers the block even when the samples clustered in one part of it.
		base := int(lo) - birthdaySprayMargin
		if base < 1024 {
			base = 1024
		}
		top := int(hi) + birthdaySprayMargin
		if top > 65535 {
			top = 65535
		}
		width := top - base + 1
		// sweep the widened block, shuffling the offsets so the ports do not go out in order
		offs := make([]int, width)
		for i := range offs {
			offs[i] = i
		}
		mrand.Shuffle(width, func(i, j int) { offs[i], offs[j] = offs[j], offs[i] }) // #nosec G404
		for i := 0; i < m; i++ {
			out = append(out, uint16(base+offs[i%width]))
		}
		return out
	}
	for i := 0; i < m; i++ {
		out = append(out, uint16(1024+mrand.IntN(65535-1024+1))) // #nosec G404
	}
	return out
}

// BirthdayMutual is the both-symmetric case. Neither side has a stable address, so each socket aims
// at exactly one guessed port of the peer (a symmetric NAT gives a fresh external port per
// destination, so one destination per socket keeps that port stable). A link forms only on the
// socket pair where each guessed the other's external port — a double birthday, which pays off only
// when both port ranges are narrow. The lower peer ID dials, the higher listens (decided by the
// caller via asDialer), so exactly one QUIC client meets one QUIC server.
func (q *QuicTransport) BirthdayMutual(ctx context.Context, ip string, lo, hi uint16, n int, peer types.PeerID, asDialer bool) error {
	if n < 1 {
		n = 1
	}
	if n > maxBirthdayMutualSockets {
		n = maxBirthdayMutualSockets
	}
	host, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("birthday mutual: bad ip %q: %w", ip, err)
	}
	host = host.Unmap()
	if !q.birthdayTargetOK(host) {
		return fmt.Errorf("birthday mutual: %q is not a unicast address", ip)
	}
	key := punchKey{addr: "mutual:" + ip, peer: peer}
	if !q.arrivals.begin(key) {
		return ErrPunchActive
	}
	defer q.arrivals.end(key)

	// Both ends guess ports, so the key must be known for every port of the peer's host, not for one
	// address. Without it the first obfuscated packet from a guessed port would be dropped.
	q.ExpectPeerIP(host.String(), peer)

	ports := birthdayPorts(lo, hi, n) // one guessed target port per socket
	socks := make([]net.PacketConn, 0, n)
	targets := make([]*net.UDPAddr, 0, n)
	for i := 0; i < n; i++ {
		c, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if e != nil {
			break
		}
		socks = append(socks, q.wrapBirthdayConn(c))
		targets = append(targets, net.UDPAddrFromAddrPort(netip.AddrPortFrom(host, ports[i])))
	}
	if len(socks) == 0 {
		return ErrBirthdayNoSocket
	}

	if asDialer {
		return q.birthdayMutualDial(ctx, socks, targets, peer)
	}
	return q.birthdayMutualListen(ctx, socks, targets, peer)
}

// birthdayMutualDial punches each socket's single target and, when a packet comes back from exactly
// that target (the one socket pair that matched), dials it over QUIC from the same socket.
func (q *QuicTransport) birthdayMutualDial(ctx context.Context, socks []net.PacketConn, targets []*net.UDPAddr, peer types.PeerID) error {
	punchCtx, cancel := context.WithTimeout(ctx, birthdayTimeout)
	defer cancel()
	stop := make(chan struct{})
	var once sync.Once
	stopAll := func() { once.Do(func() { close(stop) }) }
	var wg sync.WaitGroup
	type hit struct {
		conn   net.PacketConn
		target *net.UDPAddr
	}
	hits := make(chan hit, len(socks))

	for i := range socks {
		wg.Add(1)
		go func(s net.PacketConn, target *net.UDPAddr) {
			defer wg.Done()
			want := target.AddrPort()
			buf := make([]byte, 1500)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				_, from, e := s.ReadFrom(buf)
				if e != nil {
					if ne, ok := e.(net.Error); ok && ne.Timeout() {
						continue
					}
					return
				}
				// only the exact target counts: a packet from another of the peer's ports arrived on a
				// mapping we did not open toward it, so dialing it would use a fresh external port the
				// peer never saw
				if ap, ok := from.(*net.UDPAddr); ok && ap.AddrPort() == want {
					select {
					case hits <- hit{s, target}:
					default:
					}
					return
				}
			}
		}(socks[i], targets[i])
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		birthdayPaceLoop(punchCtx, stop, len(socks), func(i int, payload []byte) {
			_, _ = socks[i].WriteTo(payload, targets[i])
		})
	}()

	var won hit
	select {
	case won = <-hits:
	case <-punchCtx.Done():
	}
	stopAll()
	for _, s := range socks {
		if s != won.conn {
			_ = s.Close()
		}
	}
	wg.Wait()
	if won.conn == nil {
		return ErrHolePunchTimeout
	}
	_ = won.conn.SetReadDeadline(time.Time{})
	dctx, dcancel := context.WithTimeout(ctx, birthdayDialBudget)
	defer dcancel()
	// The target is a guessed ephemeral port, not where the peer listens: the connection it carries
	// is useful, but its address must not enter the peer store or the DHT.
	if err := q.birthdayDialOn(dctx, won.conn, won.target, peer, false); err != nil {
		_ = won.conn.Close()
		return err
	}
	return nil
}

// birthdayMutualListen runs a QUIC listener on each socket, punches each socket's single target to
// open its mapping, and waits for the dialer to arrive. A socket that accepts a connection stays
// open for that connection's life; the rest close when the attempt ends.
func (q *QuicTransport) birthdayMutualListen(ctx context.Context, socks []net.PacketConn, targets []*net.UDPAddr, peer types.PeerID) error {
	arrived, cancel := q.arrivals.watch(peer)
	defer cancel()
	listenCtx, stop := context.WithTimeout(ctx, birthdayTimeout+birthdayDialBudget)
	defer stop()

	trs := make([]*quic.Transport, 0, len(socks))
	ltargets := make([]*net.UDPAddr, 0, len(socks)) // kept aligned with trs
	for i, s := range socks {
		tr := &quic.Transport{Conn: s}
		ln, e := tr.ListenEarly(q.tlsCfg, q.quicCgf)
		if e != nil {
			_ = tr.Close()
			_ = s.Close()
			continue
		}
		trs = append(trs, tr)
		ltargets = append(ltargets, targets[i])
		go q.birthdayAccept(listenCtx, ln, tr, s, peer)
	}
	if len(trs) == 0 {
		return ErrBirthdayNoSocket
	}
	go birthdayPaceLoop(listenCtx, nil, len(trs), func(i int, payload []byte) {
		_, _ = trs[i].WriteTo(payload, ltargets[i])
	})

	select {
	case <-arrived:
		// the connection lives on its socket; its own accept goroutine keeps that socket open and
		// closes it when the connection ends. Nothing to tear down here.
		return nil
	case <-listenCtx.Done():
		return ErrHolePunchTimeout
	}
}

// birthdayAccept accepts inbound connections on ln, authenticates each, and hands it to the swarm,
// keeping tr and its socket alive for every accepted connection's lifetime; once ctx ends and no
// connection is left, it closes tr and the socket. It applies the same inbound limits as the main
// listener (per-address guard and a handshake slot) and accepts only the peer this punch was
// arranged with: these sockets are ephemeral and must not become a second, unguarded door.
func (q *QuicTransport) birthdayAccept(ctx context.Context, ln *quic.EarlyListener, tr *quic.Transport, pconn net.PacketConn, peer types.PeerID) {
	var live sync.WaitGroup
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			break
		}
		q.applyCongestion(conn)
		ip := addrOfNet(conn.RemoteAddr())
		releaseIP, allowed := q.guard.Load().Allow(ip)
		if !allowed {
			conn.CloseWithError(ErrCodeSpamDetected, "too many attempts")
			continue
		}
		select {
		case q.handshakeSem <- struct{}{}:
		default:
			releaseIP()
			conn.CloseWithError(ErrCodeSpamDetected, "handshake overloaded")
			continue
		}
		live.Add(1)
		go func(c *quic.Conn) {
			defer func() { <-q.handshakeSem }()
			defer releaseIP()
			peerRes, aerr := q.authenticator.AuthenticateInbound(ctx, WrapQUIC(c))
			if aerr != nil {
				q.guard.Load().Fail(ip)
				c.CloseWithError(ErrCodeAuthFailed, "auth failed")
				live.Done()
				return
			}
			if peerRes.PeerID != peer {
				q.guard.Load().Fail(ip)
				c.CloseWithError(ErrCodeAuthFailed, "unexpected peer")
				live.Done()
				return
			}
			q.guard.Load().Success(ip)
			// The source port is an ephemeral NAT mapping, not where the peer listens: keep it out
			// of the peer store and the DHT.
			q.newConn(c, false, c.RemoteAddr().String(), false, peerRes.PeerID, peerRes.PubKey, peerRes.Caps, false, peerRes.Caps.Has(handshake.CapPoolLink))
			q.logger.Info("Birthday punch (both symmetric) reached a peer", "peer", peerRes.PeerID, "remote", c.RemoteAddr().String())
			go func() { <-c.Context().Done(); live.Done() }()
		}(conn)
	}
	live.Wait()
	_ = tr.Close()
	_ = pconn.Close()
}
