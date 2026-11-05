package noise3

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	quic "github.com/apernet/quic-go"
	"golang.org/x/crypto/curve25519"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/elligator"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/packet"
	"github.com/echofog/echofog-core/internal/v2/carrier/quicx"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// ListenerConfig configures a noise3 listener. Conn, Keys and at least one usable masking key
// are required; everything else has a production default.
type ListenerConfig struct {
	// Conn is the outer UDP socket. The listener owns it and closes it in Close.
	Conn net.PacketConn
	// Keys are the live masking keys (at most 8). A key with NotAfter in the past is ignored.
	Keys []MaskKey
	// KV is the namespace for the persisted replay filter (store.KV satisfies it). Nil disables
	// persistence across restarts.
	KV KV
	// Profile is the shape profile this listener publishes (wire.md §9); nil is default-udp.
	Profile *shape.Profile
	// TLSConfig is the inner QUIC TLS configuration; nil means quicx.Noise3TLSConfig's server.
	TLSConfig *tls.Config
	// QUICConfig is the inner QUIC configuration; nil means quicx.Noise3QUICConfig.
	QUICConfig *quic.Config
	// Clock is the injectable time source; nil means the system clock.
	Clock Clock
	// Rand draws the random pending-flow eviction; nil uses math/rand/v2. It is not used for
	// keys: those come from crypto/rand.
	Rand *mrand.Rand
	// LowPower disables idle padding (platforms.md §2).
	LowPower bool
	// MaxDatagramLen is the outer noise3 payload ceiling (path MTU minus IP and UDP headers).
	// Zero means packet.MaxDatagramLen (1452, an IPv4 1500-byte path); values above it are
	// clamped to it. It must be at least 71+1200, otherwise an Init cannot carry the inner QUIC
	// Initial (see doc.go).
	MaxDatagramLen int
	// MaxPending, MaxPendingPerIP and MaxFlows override the §10 pending-flow limits (4096, 64)
	// and the local total-flow bound (4096). Tests use smaller values; zero means the default.
	MaxPending      int
	MaxPendingPerIP int
	MaxFlows        int
}

// Listener is one noise3 UDP port. It authenticates Init datagrams, keeps pending flows, feeds
// their inner datagrams to one inner QUIC listener and returns mux.Carrier connections on Accept.
type Listener struct {
	conn     net.PacketConn
	inner    *queueConn
	tr       *quic.Transport
	quicLn   *quic.Listener
	keys     []maskKey
	profile  *shape.Profile
	clock    Clock
	rnd      *mrand.Rand
	lowPower bool
	replay   *replayCache
	budget   *budgets

	mu              sync.Mutex
	flows           map[netip.AddrPort]*flow
	rmu             sync.Mutex
	routes          map[netip.AddrPort]*routeConn // DialFrom: datagrams from these remotes go to the dial
	byVAddr         map[uint64]*flow
	ipFlows         map[netip.Addr][]*flow
	recent          []*flow
	pending         int
	pendingIP       map[netip.Addr]int
	nextID          uint64
	lastSweep       time.Time
	closed          bool
	maxPending      int
	maxPendingPerIP int
	maxFlows        int
	maxDatagram     int

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

var _ carrier.Listener = (*Listener)(nil)

// NewListener starts a listener on an already opened socket.
func NewListener(cfg ListenerConfig) (*Listener, error) {
	if cfg.Conn == nil {
		return nil, fmt.Errorf("%w: nil socket", ErrBadConfig)
	}
	if len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("%w: no masking keys", ErrBadConfig)
	}
	if len(cfg.Keys) > MaxMaskKeys {
		return nil, fmt.Errorf("%w: %d masking keys, at most %d", ErrBadConfig, len(cfg.Keys), MaxMaskKeys)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = newSystemClock()
	}
	profile := cfg.Profile
	if profile == nil {
		profile, _ = shape.Builtin(shape.IDDefaultUDP)
	}
	if profile == nil {
		return nil, fmt.Errorf("%w: shape profile", ErrBadConfig)
	}
	tlsConf := cfg.TLSConfig
	if tlsConf == nil {
		_, server, err := quicx.Noise3TLSConfig()
		if err != nil {
			return nil, fmt.Errorf("noise3: inner TLS: %w", err)
		}
		tlsConf = server
	}
	quicConf := cfg.QUICConfig
	quicConf, err := innerQUICConfig(quicConf, maxPadding(profile), l0MaxDatagram(cfg.MaxDatagramLen))
	if err != nil {
		return nil, err
	}

	l := &Listener{
		conn:            cfg.Conn,
		profile:         profile,
		clock:           clock,
		rnd:             cfg.Rand,
		lowPower:        cfg.LowPower,
		maxPending:      cfg.MaxPending,
		maxPendingPerIP: cfg.MaxPendingPerIP,
		maxFlows:        cfg.MaxFlows,
		maxDatagram:     l0MaxDatagram(cfg.MaxDatagramLen),
		budget:          newBudgets(),
		flows:           make(map[netip.AddrPort]*flow),
		byVAddr:         make(map[uint64]*flow),
		ipFlows:         make(map[netip.Addr][]*flow),
		pendingIP:       make(map[netip.Addr]int),
		nextID:          1,
		lastSweep:       clock.Now(),
		done:            make(chan struct{}),
	}
	if l.maxPending <= 0 {
		l.maxPending = MaxPendingFlows
	}
	if l.maxPendingPerIP <= 0 {
		l.maxPendingPerIP = MaxPendingPerIP
	}
	if l.maxFlows <= 0 {
		l.maxFlows = MaxPendingFlows
	}
	l.replay = newReplayCache(cfg.KV, clock.Now(), nil)
	for _, k := range cfg.Keys {
		pub, err := maskPublic(k.Private)
		if err != nil {
			return nil, fmt.Errorf("noise3: masking key: %w", err)
		}
		l.keys = append(l.keys, maskKey{secret: k.Private, public: pub, notAfter: k.NotAfter})
	}
	l.inner = newQueueConn(flowAddr{}, l.innerWrite)
	tr := &quic.Transport{Conn: l.inner, VerifySourceAddress: l.underLoad}
	ln, err := tr.Listen(tlsConf, quicConf)
	if err != nil {
		return nil, fmt.Errorf("noise3: inner QUIC listen: %w", err)
	}
	l.tr = tr
	l.quicLn = ln
	l.wg.Add(1)
	go l.readLoop()
	l.scheduleSweep()
	return l, nil
}

// SetKeys replaces the live masking keys (rotation, wire.md §2.1). Init datagrams for a key that
// is no longer listed are dropped from now on; established flows are not touched.
func (l *Listener) SetKeys(keys []MaskKey) error {
	if len(keys) == 0 {
		return fmt.Errorf("%w: no masking keys", ErrBadConfig)
	}
	if len(keys) > MaxMaskKeys {
		return fmt.Errorf("%w: %d masking keys, at most %d", ErrBadConfig, len(keys), MaxMaskKeys)
	}
	set := make([]maskKey, 0, len(keys))
	for _, k := range keys {
		pub, err := maskPublic(k.Private)
		if err != nil {
			return fmt.Errorf("%w: masking key: %v", ErrBadConfig, err)
		}
		set = append(set, maskKey{secret: k.Private, public: pub, notAfter: k.NotAfter})
	}
	l.mu.Lock()
	l.keys = set
	l.mu.Unlock()
	return nil
}

// liveKeys returns the current key set; the slice is never modified after it is published.
func (l *Listener) liveKeys() []maskKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.keys
}

// Revoke stops accepting Init datagrams for the masking key with this public value at once. The
// last remaining key cannot be revoked: use SetKeys with a replacement.
func (l *Listener) Revoke(public [32]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	idx := -1
	for i := range l.keys {
		if l.keys[i].public == public {
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: unknown masking key", ErrBadConfig)
	}
	if len(l.keys) == 1 {
		return fmt.Errorf("%w: the last masking key cannot be revoked", ErrBadConfig)
	}
	set := make([]maskKey, 0, len(l.keys)-1)
	set = append(set, l.keys[:idx]...)
	set = append(set, l.keys[idx+1:]...)
	l.keys = set
	return nil
}

// Listen binds addr and starts a listener.
func Listen(addr string, cfg ListenerConfig) (*Listener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("noise3: resolve %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("noise3: listen %q: %w", addr, err)
	}
	cfg.Conn = conn
	l, err := NewListener(cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return l, nil
}

// Accept returns the next inner QUIC connection as a carrier.
func (l *Listener) Accept(ctx context.Context) (mux.Carrier, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := l.quicLn.Accept(ctx)
	if err != nil {
		return nil, err
	}
	if a, ok := conn.RemoteAddr().(flowAddr); ok {
		l.confirmFlow(a.id)
	}
	return quicx.WrapNoise3(conn, false), nil
}

// Addr returns the bound UDP address.
func (l *Listener) Addr() net.Addr { return l.conn.LocalAddr() }

// Close stops the listener. It is idempotent.
func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		close(l.done)
		if l.conn != nil {
			err = l.conn.Close()
		}
		if l.quicLn != nil {
			_ = l.quicLn.Close()
		}
		if l.tr != nil {
			_ = l.tr.Close()
		}
		if l.inner != nil {
			_ = l.inner.Close()
		}
		l.replay.flush(l.clock.Now())
		l.wg.Wait()
	})
	return err
}

func (l *Listener) readLoop() {
	defer l.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, addr, err := l.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:n]...)
		l.handle(pkt, addr, l.clock.Now())
	}
}

func (l *Listener) handle(d []byte, addr net.Addr, now time.Time) {
	src, ok := addrPort(addr)
	if !ok {
		return
	}
	if rc := l.route(src); rc != nil {
		rc.q.deliver(d, addr)
		return
	}
	l.maybeSweep(now)
	l.replay.maybeFlush(now)

	l.mu.Lock()
	f := l.flows[src]
	var succ *flow
	if f != nil {
		succ = f.successor
	}
	l.mu.Unlock()

	if f != nil {
		if n, inner, prev, status := f.openData(d, now); status != dataNoMatch {
			if status == dataOK {
				l.deliverData(f, src, n, prev, len(d), inner, now)
			}
			return
		}
		if succ != nil {
			// The packet may belong to the handshake that replaces this flow (A-2): the old flow
			// stays in place until the new one proves itself with an authentic data packet.
			if n, inner, prev, status := succ.openData(d, now); status != dataNoMatch {
				if status == dataOK {
					l.promote(f, succ, src)
					l.deliverData(succ, src, n, prev, len(d), inner, now)
				}
				return
			}
		}
		if len(d) < packet.MinInitLen {
			return
		}
		if !l.budget.allow(budgetKey(src.Addr()), now) {
			return
		}
		l.handleInit(d, src, l.liveKeys(), now)
		return
	}

	// A datagram from an unknown address is tried against established flows first (NAT
	// rebinding), then treated as a candidate Init. Both are charged to the stranger budget.
	if !l.budget.allow(budgetKey(src.Addr()), now) {
		return
	}
	if l.rebind(d, src, now) {
		return
	}
	if len(d) < packet.MinInitLen {
		return
	}
	l.handleInit(d, src, l.liveKeys(), now)
}

// promote makes the successor the flow of src after its first authentic data packet (A-2).
func (l *Listener) promote(old, succ *flow, src netip.AddrPort) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.flows[src] != old || old.successor != succ || succ.isClosed() {
		return
	}
	old.successor = nil
	succ.parent = nil
	l.removeLocked(old)
	l.flows[src] = succ
}

// deliverData records an authenticated data packet and hands its inner datagram to QUIC.
func (l *Listener) deliverData(f *flow, src netip.AddrPort, n, prev uint64, size int, inner []byte, now time.Time) {
	old, moved := f.acceptFrom(src, n, prev, size, now)
	if moved {
		l.changeAddr(f, old, src)
	}
	if f.markEstablished() {
		l.establish(f)
	}
	l.touchRecent(f)
	l.inner.deliver(inner, f.vaddr)
}

// rebind tries the datagram against established flows of the same source IP and then the most
// recently active flows that have been silent for more than a second (wire.md §2.6).
func (l *Listener) rebind(d []byte, src netip.AddrPort, now time.Time) bool {
	l.mu.Lock()
	sameIP := append([]*flow(nil), l.ipFlows[src.Addr()]...)
	recent := append([]*flow(nil), l.recent...)
	l.mu.Unlock()

	seen := make(map[*flow]struct{}, len(sameIP)+len(recent))
	try := func(f *flow) bool {
		if _, ok := seen[f]; ok {
			return false
		}
		seen[f] = struct{}{}
		n, inner, prev, status := f.openData(d, now)
		if status == dataNoMatch {
			return false
		}
		if status == dataOK {
			l.deliverData(f, src, n, prev, len(d), inner, now)
		}
		return true
	}
	for _, f := range sameIP {
		if !f.isEstablished() {
			continue
		}
		if try(f) {
			return true
		}
	}
	tried := 0
	for _, f := range recent {
		if !f.isEstablished() {
			continue
		}
		if f.silentSince(now) < silentRebind {
			continue
		}
		if tried >= recentRebindFlows {
			break
		}
		tried++
		if try(f) {
			return true
		}
	}
	return false
}

type dataStatus uint8

const (
	dataNoMatch dataStatus = iota
	dataOK
	dataDuplicate
)

func (l *Listener) handleInit(d []byte, src netip.AddrPort, keys []maskKey, now time.Time) {
	repr := d[0:32]
	u := elligator.Forward(repr)
	var matched packet.Keys
	var found bool
	for i := range keys {
		mk := &keys[i]
		if !mk.notAfter.IsZero() && now.After(mk.notAfter) {
			continue
		}
		z, err := curve25519.X25519(mk.secret[:], u)
		if err != nil || allZero(z) {
			continue
		}
		k, err := packet.Derive(z, mk.public[:], u, repr)
		if err != nil {
			continue
		}
		// Compare every key: the loop does not stop at the match.
		if subtle.ConstantTimeCompare(k.Sel[:], d[32:48]) == 1 {
			matched = k
			found = true
		}
	}
	if !found {
		return
	}
	init, err := packet.OpenInit(matched, d)
	if err != nil {
		return
	}
	ts := int64(init.TS)
	if diff := int64(now.Unix()) - ts; diff > int64(initTimeWindow/time.Second) || diff < -int64(initTimeWindow/time.Second) {
		return
	}
	var sel [16]byte
	copy(sel[:], d[32:48])
	if l.replay.seen(sel[:], now) {
		return
	}

	l.mu.Lock()
	f := l.flows[src]
	l.mu.Unlock()
	if f != nil && f.isPending() && now.Sub(f.created()) >= pendingInitAge {
		// A pending flow older than 10 s is no longer the sibling window of §2.5 step 6:
		// the initiator evidently restarted, so this Init starts a new flow.
		l.mu.Lock()
		if l.flows[src] == f && f.isPending() {
			l.removeLocked(f)
		}
		l.mu.Unlock()
		l.mu.Lock()
		f = l.flows[src]
		l.mu.Unlock()
	}
	if f != nil && f.isEstablished() && newerInitTS(f.firstTimestamp(), init.TS) {
		// A new handshake on the same 4-tuple (A-2). It cannot use the old reply keys, but any
		// contact of a hidden node can build an authentic Init, so it must not cut a flow that
		// is still alive. If the old flow has been silent for its restart interval the client
		// has evidently restarted and the new Init replaces it. Otherwise the new Init starts
		// a successor flow and the old one stays until the successor's first data packet.
		l.mu.Lock()
		succ := f.successor
		silent := f.silentSince(now) >= f.silenceLimit
		l.mu.Unlock()
		switch {
		case silent:
			l.mu.Lock()
			if l.flows[src] == f && f.isEstablished() {
				if succ != nil {
					l.removeLocked(succ)
				}
				l.removeLocked(f)
			}
			f = l.flows[src]
			l.mu.Unlock()
		case succ != nil && !newerInitTS(succ.firstTimestamp(), init.TS):
			// A sibling Init of the successor handshake.
			succ.addKeys(matched)
			succ.acceptInit(len(d), now)
			l.inner.deliver(init.Inner, succ.vaddr)
			return
		default:
			if succ != nil {
				l.mu.Lock()
				l.removeLocked(succ)
				l.mu.Unlock()
			}
			if nf := l.newFlowFor(src, matched, len(d), init.TS, now, f); nf != nil {
				l.inner.deliver(init.Inner, nf.vaddr)
			}
			return
		}
	}
	if f != nil {
		f.addKeys(matched)
		f.acceptInit(len(d), now)
		l.inner.deliver(init.Inner, f.vaddr)
		return
	}
	f = l.newFlow(src, matched, len(d), init.TS, now)
	if f == nil {
		return
	}
	l.inner.deliver(init.Inner, f.vaddr)
}

func (l *Listener) newFlow(src netip.AddrPort, keys packet.Keys, initial int, ts uint32, now time.Time) *flow {
	return l.newFlowFor(src, keys, initial, ts, now, nil)
}

// newFlowFor creates a flow; with a parent it is that flow's successor (A-2) and is not entered in
// the address table until it takes over.
func (l *Listener) newFlowFor(src netip.AddrPort, keys packet.Keys, initial int, ts uint32, now time.Time, parent *flow) *flow {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	ip := src.Addr()
	if l.pendingIP[ip] >= l.maxPendingPerIP {
		if !l.evictPendingLocked(ip) {
			return nil
		}
	}
	if l.pending >= l.maxPending {
		if !l.evictPendingLocked(netip.Addr{}) {
			return nil
		}
	}
	if parent == nil && len(l.flows) >= l.maxFlows {
		if !l.evictPendingLocked(netip.Addr{}) {
			// The local total-flow bound is full of established flows. §2.5 step 7 only
			// allows evicting a pending flow, so the new Init is dropped instead.
			return nil
		}
	}
	if l.pendingIP[ip] >= l.maxPendingPerIP || l.pending >= l.maxPending || (parent == nil && len(l.flows) >= l.maxFlows) {
		return nil
	}
	id := l.nextID
	l.nextID++
	f := &flow{
		l:           l,
		vaddr:       flowAddr{id: id},
		src:         src,
		keys:        []packet.Keys{keys},
		firstTS:     ts,
		inBytes:     int64(initial),
		createdAt:   now,
		lastActive:  now,
		sender:      shape.NewSender(l.profile, nil, l.lowPower),
		maxDatagram: l.maxDatagram,
	}
	f.silenceLimit = restartMin + time.Duration(randIntN(l.rnd, int((restartMax-restartMin)/time.Millisecond)+1))*time.Millisecond
	if parent != nil {
		if parent.successor != nil || l.flows[src] != parent {
			return nil
		}
		parent.successor = f
		f.parent = parent
	} else {
		l.flows[src] = f
	}
	l.byVAddr[id] = f
	l.ipFlows[ip] = append(l.ipFlows[ip], f)
	l.pending++
	l.pendingIP[ip]++
	return f
}

// evictPendingLocked removes a random pending flow of ip, or of any IP when ip is invalid. It
// never evicts an established flow (wire.md §2.5 step 7) and reports whether a flow was removed.
func (l *Listener) evictPendingLocked(ip netip.Addr) bool {
	var candidates []*flow
	consider := func(f *flow) {
		if ip.IsValid() && f.address().Addr() != ip {
			return
		}
		if f.isPending() {
			candidates = append(candidates, f)
		}
	}
	for _, f := range l.flows {
		consider(f)
		if f.successor != nil {
			consider(f.successor)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	l.removeLocked(candidates[randIntN(l.rnd, len(candidates))])
	return true
}

func (l *Listener) removeLocked(f *flow) {
	if f.isClosed() {
		return
	}
	src := f.address()
	if l.flows[src] == f {
		delete(l.flows, src)
	}
	if p := f.parent; p != nil {
		if p.successor == f {
			p.successor = nil
		}
		f.parent = nil
	}
	if s := f.successor; s != nil {
		// The old flow goes away: its successor, if it is still alive, takes the slot.
		f.successor = nil
		s.parent = nil
		if l.flows[src] == nil && !s.isClosed() {
			l.flows[src] = s
		} else {
			l.removeLocked(s)
		}
	}
	delete(l.byVAddr, f.vaddr.id)
	l.dropIPLocked(f, src.Addr())
	l.dropRecentLocked(f)
	if f.isPending() {
		if l.pending > 0 {
			l.pending--
		}
		ip := src.Addr()
		if n := l.pendingIP[ip]; n > 1 {
			l.pendingIP[ip] = n - 1
		} else {
			delete(l.pendingIP, ip)
		}
	}
	f.close()
}

func (l *Listener) dropIPLocked(f *flow, ip netip.Addr) {
	list := l.ipFlows[ip]
	for i, x := range list {
		if x == f {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(l.ipFlows, ip)
	} else {
		l.ipFlows[ip] = list
	}
}

func (l *Listener) dropRecentLocked(f *flow) {
	for i, x := range l.recent {
		if x == f {
			l.recent = append(l.recent[:i], l.recent[i+1:]...)
			return
		}
	}
}

// establish moves a flow out of the pending counters.
func (l *Listener) establish(f *flow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending > 0 {
		l.pending--
	}
	ip := f.address().Addr()
	if n := l.pendingIP[ip]; n > 1 {
		l.pendingIP[ip] = n - 1
	} else {
		delete(l.pendingIP, ip)
	}
}

// changeAddr updates the address indexes after a successful NAT rebinding.
func (l *Listener) changeAddr(f *flow, old, new netip.AddrPort) {
	if old == new {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.flows[old] == f {
		delete(l.flows, old)
	}
	if l.flows[new] != nil {
		l.removeLocked(l.flows[new])
	}
	l.flows[new] = f
	oldIP, newIP := old.Addr(), new.Addr()
	if oldIP != newIP {
		list := l.ipFlows[oldIP]
		for i, x := range list {
			if x == f {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(l.ipFlows, oldIP)
		} else {
			l.ipFlows[oldIP] = list
		}
		l.ipFlows[newIP] = append(l.ipFlows[newIP], f)
	}
}

// touchRecent keeps the last 64 active flows in recency order for rebinding searches.
func (l *Listener) touchRecent(f *flow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropRecentLocked(f)
	l.recent = append(l.recent, nil)
	copy(l.recent[1:], l.recent[:len(l.recent)-1])
	l.recent[0] = f
	if len(l.recent) > 64 {
		l.recent = l.recent[:64]
	}
}

// underLoad tells the inner QUIC stack to require address validation with a Retry when the
// listener is close to its pending-flow limits (wire.md §2.5 step 9). The Retry travels inside
// noise3 and is invisible on the wire.
func (l *Listener) underLoad(net.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pending*4 >= l.maxPending*3 || len(l.flows)*4 >= l.maxFlows*3
}

// confirmFlow marks the flow whose inner QUIC handshake just completed; after that the flow is
// no longer subject to the handshake anti-amplification cap.
func (l *Listener) confirmFlow(id uint64) {
	l.mu.Lock()
	f := l.byVAddr[id]
	l.mu.Unlock()
	if f != nil {
		f.confirm()
	}
}

// scheduleSweep removes idle flows even when no packet arrives. The delay carries the ±20%
// jitter of AD13; the clock is injectable, so tests that do not need the sweep never see it.
func (l *Listener) scheduleSweep() {
	const jitter = int64(sweepEvery / 5)
	d := sweepEvery + time.Duration(mrand.Int64N(2*jitter+1)) - time.Duration(jitter)
	l.clock.AfterFunc(d, func() {
		select {
		case <-l.done:
			return
		default:
		}
		l.sweep(l.clock.Now())
		l.scheduleSweep()
	})
}

func (l *Listener) sweep(now time.Time) {
	// Persist the replay filter from the timer as well: a kill -9 must not lose more than the
	// flush interval of selectors (wire.md §2.5 step 5). This is done before the listener lock
	// because the KV write must not block packet processing.
	l.replay.maybeFlush(now)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSweep = now
	var expired []*flow
	for _, f := range l.flows {
		if f.idleFor(now) > flowIdleTimeout {
			expired = append(expired, f)
			continue
		}
		if s := f.successor; s != nil {
			if s.idleFor(now) > flowIdleTimeout {
				expired = append(expired, s)
			} else if f.silentSince(now) >= f.silenceLimit {
				// The old flow is silent: the successor takes over (removeLocked promotes it).
				expired = append(expired, f)
			}
		}
	}
	for _, f := range expired {
		l.removeLocked(f)
	}
}

func (l *Listener) maybeSweep(now time.Time) {
	l.mu.Lock()
	due := now.Sub(l.lastSweep) >= sweepEvery
	l.mu.Unlock()
	if due {
		l.sweep(now)
	}
}

// innerWrite is the WriteTo of the virtual PacketConn the inner QUIC listener reads and writes.
func (l *Listener) innerWrite(p []byte, addr net.Addr) (int, error) {
	a, ok := addr.(flowAddr)
	if !ok {
		return len(p), nil
	}
	l.mu.Lock()
	f := l.byVAddr[a.id]
	l.mu.Unlock()
	if f == nil {
		return len(p), nil
	}
	return f.send(p, l.clock.Now())
}

func allZero(b []byte) bool {
	var v byte
	for _, x := range b {
		v |= x
	}
	return v == 0
}

// flow is one L0 flow: a source IP:port with its keys, packet numbers and reply address.
type flow struct {
	l     *Listener
	vaddr flowAddr
	src   netip.AddrPort

	keys    []packet.Keys // first Init keys, then siblings
	firstTS uint32        // ts of the first Init of the flow

	sender *shape.Sender

	mu          sync.Mutex
	closed      bool
	createdAt   time.Time
	lastActive  time.Time
	established bool
	confirmed   bool
	addrAuthed  bool

	inBytes     int64
	outBytes    int64
	sendN       uint64
	replay      packet.Window
	maxDatagram int

	cand      netip.AddrPort
	candCount int
	candMax   uint64

	// silenceLimit is the uniform(12, 18) s inbound silence after which a new Init replaces the
	// flow (A-2). successor and parent link a flow to the handshake that may replace it; both
	// are guarded by Listener.mu.
	silenceLimit time.Duration
	successor    *flow
	parent       *flow
}

func (f *flow) isPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.established
}

func (f *flow) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *flow) address() netip.AddrPort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.src
}

func (f *flow) created() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createdAt
}

func (f *flow) firstTimestamp() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.firstTS
}

// newerInitTS distinguishes a new handshake from a late sibling of the current one. The initiator
// stamps every Init with its own clock; the Inits of one handshake share the same second, while a
// dead-flow restart is at least uniform(12,18) s later (wire.md §2.4, §2.6).
func newerInitTS(old, next uint32) bool {
	return int64(next) > int64(old)+1
}

func (f *flow) isEstablished() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.established
}

func (f *flow) idleFor(now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return now.Sub(f.lastActive)
}

func (f *flow) silentSince(now time.Time) time.Duration { return f.idleFor(now) }

func (f *flow) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *flow) confirm() {
	f.mu.Lock()
	f.confirmed = true
	f.mu.Unlock()
}

func (f *flow) markEstablished() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.established {
		return false
	}
	f.established = true
	return true
}

func (f *flow) addKeys(k packet.Keys) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.keys = append(f.keys, k)
	if len(f.keys) > 4 {
		// Keep the first Init's keys: the responder always answers with them, and they
		// select the flow (wire.md §2.5 step 6). The three newest siblings are enough to
		// decrypt a fresh Init.
		f.keys = append(f.keys[:1], f.keys[len(f.keys)-3:]...)
	}
}

func (f *flow) acceptInit(size int, now time.Time) {
	f.mu.Lock()
	f.inBytes += int64(size)
	f.lastActive = now
	f.mu.Unlock()
}

// openData tries the Init key set and its siblings and the replay window. The returned prev is
// the highest number seen before this packet, which the reply-address rule needs.
func (f *flow) openData(d []byte, now time.Time) (n uint64, inner []byte, prev uint64, status dataStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, nil, 0, dataNoMatch
	}
	highest := f.replay.Highest()
	for i := range f.keys {
		k := &f.keys[i]
		n, inner, err := packet.OpenData(k.KI2R, k.HPI2R, d, highest)
		if err != nil {
			continue
		}
		if !f.replay.Check(n) {
			return 0, nil, highest, dataDuplicate
		}
		f.replay.Commit(n)
		f.lastActive = now
		return n, inner, highest, dataOK
	}
	return 0, nil, highest, dataNoMatch
}

// acceptFrom applies the address rules and returns the previous address when the reply address
// must move (wire.md §2.6). The caller updates the listener's indexes outside f.mu.
func (f *flow) acceptFrom(src netip.AddrPort, n, prev uint64, size int, now time.Time) (old netip.AddrPort, moved bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if src == f.src {
		f.inBytes += int64(size)
		f.addrAuthed = true
		f.lastActive = now
		f.candCount = 0
		f.cand = netip.AddrPort{}
		return src, false
	}
	if f.cand != src {
		f.cand = src
		f.candCount = 0
		f.candMax = 0
	}
	if n > prev && n > f.candMax {
		f.candCount++
		f.candMax = n
	} else {
		f.candCount = 0
		f.candMax = n
	}
	if f.candCount < replyMovePackets {
		return f.src, false
	}
	old = f.src
	f.src = src
	f.inBytes = int64(size)
	f.outBytes = 0
	f.addrAuthed = false
	f.candCount = 0
	f.cand = netip.AddrPort{}
	f.lastActive = now
	return old, true
}

// send wraps one inner datagram in a data packet and enforces the anti-amplification cap.
func (f *flow) send(inner []byte, now time.Time) (int, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return len(inner), nil
	}
	if packet.DataOverhead+len(inner) > f.maxDatagram {
		f.mu.Unlock()
		return 0, packet.ErrMalformed
	}
	pad := f.sender.PadFor(packet.DataOverhead + len(inner))
	if room := f.maxDatagram - packet.DataOverhead - len(inner); pad > room {
		pad = room
	}
	key := &f.keys[0]
	dg, err := packet.SealData(key.KR2I, key.HPR2I, f.sendN, inner, pad)
	if err != nil {
		f.mu.Unlock()
		return 0, err
	}
	if !f.maySendLocked(len(dg)) {
		f.mu.Unlock()
		return len(inner), nil
	}
	f.sendN++
	f.outBytes += int64(len(dg))
	src := f.src
	f.mu.Unlock()
	_, _ = f.l.conn.WriteTo(dg, net.UDPAddrFromAddrPort(src))
	return len(inner), nil
}

func (f *flow) maySendLocked(size int) bool {
	if f.confirmed && f.addrAuthed {
		return true
	}
	return f.outBytes+int64(size) <= ampFactor*f.inBytes
}
