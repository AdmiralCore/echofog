package exit_gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/ratelimit"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	resultOK         byte = 0
	resultDenied     byte = 1
	resultDialFailed byte = 2
	resultPayment    byte = 3
	// resultBusy: the exit is at a stream cap (the peer's, its address's or its own). An older
	// client reads it as a failed dial, which does not make it give up on the exit either.
	resultBusy byte = 4

	maxTargetLen = 300
)

var (
	ErrPaymentRequired = errors.New("exit node requires payment after the free test traffic")
	ErrExitDisabled    = errors.New("exit node is not accepting traffic")
	ErrExitDenied      = errors.New("exit node refused the target")
	ErrExitDialFailed  = errors.New("exit node could not reach the target")
	ErrExitBusy        = errors.New("exit node is serving too many streams, try again shortly")
	ErrExitProtocol    = errors.New("exit node sent an invalid reply")
	ErrBlockedAddress  = errors.New("target address is private, loopback or otherwise not allowed")
	ErrInvalidTarget   = errors.New("invalid target address")
	errNoStreamOpener  = errors.New("no stream opener configured")
)

// FramedStream is a bidirectional peer stream that carries one framed request/response before turning into a raw byte pipe
type FramedStream interface {
	io.ReadWriteCloser
	WriteFrame(msgType net_types.MessageType, data []byte) error
	ReadFrame() (net_types.MessageType, []byte, error)
}

// StreamOpener opens a new bidirectional stream to the chosen exit peer
type StreamOpener func(ctx context.Context) (FramedStream, error)

// P2PDialer implements StreamDialer by tunnelling every connection through one exit peer
type P2PDialer struct {
	open StreamOpener
}

func NewP2PDialer(open StreamOpener) *P2PDialer { return &P2PDialer{open: open} }

// DialExitStream opens a tunnel to targetAddr through the exit peer without waiting for the exit
// to answer, the way hysteria's fast open does: the request goes out and the stream is returned
// at once, so the application's first bytes leave together with it instead of one round trip
// plus the exit's own dial later. The exit's verdict is read on the first Read: a refusal comes
// back from there as ErrExitDenied, ErrPaymentRequired and so on. ctx bounds the wait for the
// verdict as well, so it must stay alive until the first Read returned; cancelling it earlier
// aborts the stream. Callers that need the verdict before anything else use
// DialExitStreamConfirmed.
func (d *P2PDialer) DialExitStream(ctx context.Context, targetAddr string) (io.ReadWriteCloser, error) {
	l, err := d.dialLazy(ctx, targetAddr)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// DialExitStreamConfirmed is DialExitStream that returns only once the exit has answered, with
// its refusal as the error: for checks that only want to know whether the exit carries a target.
func (d *P2PDialer) DialExitStreamConfirmed(ctx context.Context, targetAddr string) (io.ReadWriteCloser, error) {
	l, err := d.dialLazy(ctx, targetAddr)
	if err != nil {
		return nil, err
	}
	if err := l.Verdict(ctx); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (d *P2PDialer) dialLazy(ctx context.Context, targetAddr string) (*lazyExitStream, error) {
	if d == nil || d.open == nil {
		return nil, errNoStreamOpener
	}
	if len(targetAddr) == 0 || len(targetAddr) > maxTargetLen {
		return nil, ErrInvalidTarget
	}
	st, err := d.open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream to exit peer: %w", err)
	}
	if err := st.WriteFrame(net_types.TypeExitConnect, []byte(targetAddr)); err != nil {
		st.Close()
		return nil, err
	}
	return newLazyExitStream(ctx, st), nil
}

// verdictErr turns the exit's reply frame into what the client sees.
func verdictErr(t net_types.MessageType, b []byte) error {
	if t != net_types.TypeExitConnectResult || len(b) < 1 {
		return ErrExitProtocol
	}
	switch b[0] {
	case resultOK:
		return nil
	case resultDenied:
		return ErrExitDenied
	case resultPayment:
		return ErrPaymentRequired
	case resultBusy:
		return ErrExitBusy
	default:
		return ErrExitDialFailed
	}
}

// VerdictNotifier is implemented by streams whose exit answers after they were handed out.
type VerdictNotifier interface {
	// OnVerdict calls f once with the exit's answer: nil when it carries the stream. f is not
	// called when the stream was closed locally before the answer came.
	OnVerdict(f func(error))
}

// lazyExitStream is a tunnel whose exit has not answered yet. One goroutine reads the reply
// frame; Read waits for it, and Write goes out at once (the exit reads it after its dial).
type lazyExitStream struct {
	FramedStream
	ready  chan struct{} // closed once err is set
	err    error
	closed atomic.Bool

	mu       sync.Mutex
	notify   func(error)
	notified bool
}

func newLazyExitStream(ctx context.Context, st FramedStream) *lazyExitStream {
	l := &lazyExitStream{FramedStream: st, ready: make(chan struct{})}
	var timedOut atomic.Bool
	stop := context.AfterFunc(ctx, func() {
		timedOut.Store(true)
		_ = st.Close() // the exit never answered in time: the read below fails at once
	})
	go func() {
		t, b, err := st.ReadFrame()
		stop()
		switch {
		case timedOut.Load():
			err = ctx.Err()
		case err == nil:
			err = verdictErr(t, b)
		}
		if err != nil {
			_ = st.Close()
		}
		l.err = err
		close(l.ready)
		l.fire()
	}()
	return l
}

func (l *lazyExitStream) fire() {
	l.mu.Lock()
	f := l.notify
	if f == nil || l.notified {
		l.mu.Unlock()
		return
	}
	l.notified = true
	l.mu.Unlock()
	if l.closed.Load() && l.err != nil {
		return // closed by us before the answer: nothing was refused
	}
	f(l.err)
}

func (l *lazyExitStream) OnVerdict(f func(error)) {
	l.mu.Lock()
	l.notify = f
	l.mu.Unlock()
	select {
	case <-l.ready:
		l.fire()
	default:
	}
}

// Verdict waits for the exit's answer.
func (l *lazyExitStream) Verdict(ctx context.Context) error {
	select {
	case <-l.ready:
		return l.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *lazyExitStream) Read(p []byte) (int, error) {
	<-l.ready
	if l.err != nil {
		return 0, l.err
	}
	return l.FramedStream.Read(p)
}

// Write sends at once. A write that fails before the answer is in (the exit refused and reset the
// stream) reports the refusal rather than a bare stream error.
func (l *lazyExitStream) Write(p []byte) (int, error) {
	n, err := l.FramedStream.Write(p)
	if err != nil && !l.closed.Load() {
		<-l.ready
		if l.err != nil {
			return n, l.err
		}
	}
	return n, err
}

func (l *lazyExitStream) CloseWrite() error {
	if cw, ok := l.FramedStream.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

type deadliner interface {
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
}

// SetDeadline and SetReadDeadline reach the stream underneath; a deadline that passes before
// the exit answered fails the stream like any other read would.
func (l *lazyExitStream) SetDeadline(t time.Time) error {
	if d, ok := l.FramedStream.(deadliner); ok {
		return d.SetDeadline(t)
	}
	return nil
}

func (l *lazyExitStream) SetReadDeadline(t time.Time) error {
	if d, ok := l.FramedStream.(deadliner); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}

func (l *lazyExitStream) Close() error {
	l.closed.Store(true)
	return l.FramedStream.Close()
}

// ExitService is the egress side: it accepts tunnel requests from peers and splices them to the Internet
type ExitService struct {
	splicer      *ExitSplicer
	enabled      atomic.Bool
	allowPrivate atomic.Bool     // tests reach loopback; read by stream goroutines
	local        localNets       // this machine's own subnets, never reachable through the exit
	selfIPs      func() []net.IP // this exit's public addresses (guarded by policyMu)
	resolver     func(ctx context.Context, host string) ([]net.IP, error)
	dialTimeout  time.Duration

	accepted        atomic.Uint64
	refused         atomic.Uint64
	refusedBy       refusalCounts
	refusalLog      *logLimiter
	logRefusalsOnce sync.Once

	policyMu  sync.Mutex
	policy    Policy
	quotas    map[types.PeerID]*peerQuota
	freePeers map[types.PeerID]bool // served without quota or payment, whatever the price
	// access, when set, says who may use this exit at all: a hidden node serves only the peers it
	// invited, even over the connections it opened itself to strangers (relays, DHT nodes)
	access atomic.Pointer[func(types.PeerID) bool]

	// payments (nil: this exit takes none; a priced exit then only serves its free quota)
	ledger     *Ledger
	self       types.PeerID
	payAddress string
	onPayment  atomic.Pointer[func(micro, bytes int64)]

	// vouchers from clients' TON vaults (nil: this exit takes none)
	vouchers *VoucherBook

	// limits on what peers can take (limits and addrOf are guarded by slots.mu)
	limits  Limits
	addrOf  func(types.PeerID) string
	slots   streamSlots
	shaper  shaper
	queries *ratelimit.Keyed[types.PeerID] // account requests per peer
	offers  *ratelimit.Keyed[types.PeerID] // voucher offers per peer
}

// SetPayments lets clients pay for traffic: ledger holds their credit, self is this exit's peer ID
// (it takes part in each client's pay tag) and payAddress is the TON address transfers go to.
func (e *ExitService) SetPayments(l *Ledger, self types.PeerID, payAddress string) {
	e.policyMu.Lock()
	e.ledger, e.self, e.payAddress = l, self, payAddress
	e.policyMu.Unlock()
}

// Policy is the commercial offer of an exit: the price in micro-USDT per MB (5 cents a GB is about
// 49) and how many bytes each peer may use for free per day
type Policy struct {
	PricePerMB int64
	FreeBytes  int64
}

type peerQuota struct {
	used int64
	day  time.Time
}

// AllowPrivateTargets lets the exit reach loopback and LAN addresses. Only for tests and
// closed deployments: on a public exit it would expose the operator's own network.
func (e *ExitService) AllowPrivateTargets(on bool) { e.allowPrivate.Store(on) }

func NewExitService() *ExitService {
	e := &ExitService{
		splicer:     NewExitSplicer(8 * time.Second),
		dialTimeout: 8 * time.Second,
		quotas:      make(map[types.PeerID]*peerQuota),
		resolver: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		queries: ratelimit.NewKeyed[types.PeerID](queryRate, queryBurst, quotaPeers),
		offers:  ratelimit.NewKeyed[types.PeerID](offerRate, offerBurst, quotaPeers),
	}
	e.SetLimits(DefaultLimits())
	return e
}

func (e *ExitService) SetEnabled(on bool) { e.enabled.Store(on) }
func (e *ExitService) Enabled() bool      { return e.enabled.Load() }

// Stats returns tunnels served, tunnels refused and bytes relayed
func (e *ExitService) Stats() (accepted, refused, bytes uint64) {
	_, b := e.splicer.Stats()
	return e.accepted.Load(), e.refused.Load(), b
}

// HandleTunnel serves one incoming tunnel stream whose first frame carried the target address
func (e *ExitService) HandleTunnel(ctx context.Context, st FramedStream, target []byte) {
	e.HandleTunnelFrom(ctx, st, target, types.PeerID{})
}

// HandleTunnelFrom is HandleTunnel with the requesting peer known, so quotas apply per peer
func (e *ExitService) HandleTunnelFrom(ctx context.Context, st FramedStream, target []byte, peer types.PeerID) {
	defer st.Close()

	if !e.serves(peer) {
		e.refuse(peer, RefuseDisabled)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultDenied})
		return
	}

	if e.allowance(peer) == 0 {
		e.refuse(peer, RefusePayment)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultPayment})
		return
	}

	release, ok := e.acquire(peer, false)
	if !ok {
		e.refuse(peer, RefuseBusy)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultBusy})
		return
	}
	defer release()

	conn, code := e.connect(ctx, string(target))
	if code != resultOK {
		e.refuse(peer, refuseCode(code))
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{code})
		return
	}
	defer conn.Close()

	if err := st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultOK}); err != nil {
		return
	}
	e.accepted.Add(1)
	m := newMeter(e, peer)
	defer m.close()
	p, unpace := e.paceFor(peer)
	defer unpace()
	lim := e.CurrentLimits()
	_ = e.splicer.SpliceFor(ctx, &meteredStream{FramedStream: st, m: m, pace: p}, conn, lim.Idle, lim.MaxLifetime)
}

func (e *ExitService) connect(ctx context.Context, target string) (net.Conn, byte) {
	if len(target) == 0 || len(target) > maxTargetLen {
		return nil, resultDenied
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return nil, resultDenied
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 || e.portDenied(port) {
		return nil, resultDenied
	}

	dctx, cancel := context.WithTimeout(ctx, e.dialTimeout)
	defer cancel()

	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ips, err = e.resolver(dctx, host)
		if err != nil || len(ips) == 0 {
			return nil, resultDialFailed
		}
	}

	var allowed []net.IP
	for _, ip := range ips {
		if !e.targetBlocked(ip) {
			allowed = append(allowed, ip)
		}
	}
	if len(allowed) == 0 {
		return nil, resultDenied
	}
	if c := raceDial(dctx, allowed, portStr); c != nil {
		return c, resultOK
	}
	return nil, resultDialFailed
}

// raceStagger is how long one address gets before the next one is tried alongside it.
const raceStagger = 250 * time.Millisecond

// raceDial connects to the first address that answers, happy-eyeballs style (RFC 8305): IPv4
// and IPv6 alternate, and a new attempt starts every raceStagger or as soon as one fails. Tried
// one after another, a single blackholed address (often IPv6 on an exit without it) used up the
// whole dial budget before the working one was ever tried.
func raceDial(ctx context.Context, ips []net.IP, port string) net.Conn {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ips = interleaveFamilies(ips)
	type result struct {
		c   net.Conn
		err error
	}
	results := make(chan result, len(ips))
	var d net.Dialer
	next, running := 0, 0
	start := func() {
		ip := ips[next]
		next++
		running++
		go func() {
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			results <- result{c, err}
		}()
	}
	start()
	timer := time.NewTimer(raceStagger)
	defer timer.Stop()
	var won net.Conn
	for running > 0 {
		select {
		case r := <-results:
			running--
			if r.err == nil {
				if won == nil {
					won = r.c
					cancel() // the others give up; any that still connect are closed below
				} else {
					r.c.Close()
				}
			} else if won == nil && next < len(ips) {
				start() // a failure starts the next one at once
			}
		case <-timer.C:
			if won == nil && next < len(ips) {
				start()
				timer.Reset(raceStagger)
			}
		}
	}
	return won
}

// interleaveFamilies orders addresses v4, v6, v4, v6..., keeping each family's own order.
func interleaveFamilies(ips []net.IP) []net.IP {
	var v4, v6 []net.IP
	for _, ip := range ips {
		if ip.To4() != nil {
			v4 = append(v4, ip)
		} else {
			v6 = append(v6, ip)
		}
	}
	out := make([]net.IP, 0, len(ips))
	for i := 0; i < len(v4) || i < len(v6); i++ {
		if i < len(v4) {
			out = append(out, v4[i])
		}
		if i < len(v6) {
			out = append(out, v6[i])
		}
	}
	return out
}

// blockedIP rejects addresses that would let a remote peer reach the exit operator's own machine or LAN
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.Equal(net.IPv4bcast) || isCGNAT(ip)
}

func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64
}

func (e *ExitService) SetPolicy(p Policy) {
	e.policyMu.Lock()
	e.policy = p
	e.policyMu.Unlock()
}

// SetAccess limits the exit to the peers f admits; nil serves everybody.
func (e *ExitService) SetAccess(f func(types.PeerID) bool) {
	if f == nil {
		e.access.Store(nil)
		return
	}
	e.access.Store(&f)
}

// serves reports whether the exit is on and serves peer: a peer it does not admit is told the
// exit is off, as if it were not an exit at all.
func (e *ExitService) serves(peer types.PeerID) bool {
	if !e.enabled.Load() {
		return false
	}
	if f := e.access.Load(); f != nil {
		return (*f)(peer)
	}
	return true
}

// SetFreePeers names the peers this exit serves for free: no quota and no payment, whatever the
// price everybody else pays. The list replaces the previous one.
func (e *ExitService) SetFreePeers(ids []types.PeerID) {
	m := make(map[types.PeerID]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	e.policyMu.Lock()
	e.freePeers = m
	e.policyMu.Unlock()
}

// FreePeers returns how many peers are on the free list.
func (e *ExitService) FreePeers() int {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	return len(e.freePeers)
}

// freeLocked: the peer pays nothing here, because the exit is free or the peer is on the free list.
func (e *ExitService) freeLocked(peer types.PeerID) bool {
	return e.policy.PricePerMB <= 0 || e.freePeers[peer]
}

func (e *ExitService) CurrentPolicy() Policy {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	return e.policy
}

// allowance is how many bytes the peer may still use right now: its free quota for today plus
// whatever it has paid for. -1 means the exit is free for this peer.
func (e *ExitService) allowance(peer types.PeerID) int64 {
	e.policyMu.Lock()
	if e.freeLocked(peer) {
		e.policyMu.Unlock()
		return -1
	}
	q := e.quotaLocked(peer)
	total := e.policy.FreeBytes - q.used
	if total < 0 {
		total = 0
	}
	l, v, self := e.ledger, e.vouchers, e.self
	e.policyMu.Unlock()
	if l != nil {
		total += l.Available(PayTag(peer, self))
	}
	if v != nil {
		total += v.Available(PayTag(peer, self))
	}
	return total
}

// consume spends n bytes of the peer's allowance: the free quota first, then what vouchers paid
// for, then prepaid credit. It is
// shared by all of the peer's connections, so many parallel streams cannot multiply the quota. It
// returns false once the peer has nothing left and the stream must be cut.
func (e *ExitService) consume(peer types.PeerID, n int64) bool {
	if n <= 0 {
		return true
	}
	e.policyMu.Lock()
	if e.freeLocked(peer) {
		e.policyMu.Unlock()
		return true
	}
	q := e.quotaLocked(peer)
	freeLeft := e.policy.FreeBytes - q.used
	if freeLeft < 0 {
		freeLeft = 0
	}
	q.used += n
	l, v, self := e.ledger, e.vouchers, e.self
	e.policyMu.Unlock()

	rest := n - freeLeft
	if rest <= 0 {
		return true
	}
	tag := PayTag(peer, self)
	if v != nil {
		if rest -= v.Spend(tag, rest); rest <= 0 {
			return true
		}
	}
	return l != nil && l.Consume(tag, rest)
}

func (e *ExitService) quotaLocked(peer types.PeerID) *peerQuota {
	q := e.quotas[peer]
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if q == nil || q.day.Before(today) {
		if q == nil && len(e.quotas) >= quotaPeers {
			e.pruneQuotasLocked(today)
		}
		q = &peerQuota{day: today}
		e.quotas[peer] = q
	}
	return q
}

// pruneQuotasLocked makes room in the quota table: yesterday's entries go first, then the ones
// that used nothing, then the half that used least. Wiping the table instead, as it once did,
// gave every peer its free quota back as soon as enough fresh identities had asked.
func (e *ExitService) pruneQuotasLocked(today time.Time) {
	for id, q := range e.quotas {
		if q.day.Before(today) || q.used <= 0 {
			delete(e.quotas, id)
		}
	}
	if len(e.quotas) < quotaPeers {
		return
	}
	used := make([]int64, 0, len(e.quotas))
	for _, q := range e.quotas {
		used = append(used, q.used)
	}
	slices.Sort(used)
	cut := used[len(used)/2]
	for id, q := range e.quotas {
		if q.used <= cut && len(e.quotas) >= quotaPeers/2 {
			delete(e.quotas, id)
		}
	}
}

// meterSlice is how much allowance a stream sets aside at a time. Taking it per read meant the
// exit-wide policy lock, a clock read and, for a paying peer, a sha256 for every chunk.
const meterSlice = 256 << 10

// meter spends one stream's (or UDP relay's) share of the peer's allowance. It reserves in
// slices from the shared pools, so parallel streams still cannot multiply a quota: what one
// holds, the others cannot take. What is left at the end goes back.
type meter struct {
	svc  *ExitService
	peer types.PeerID

	mu     sync.Mutex
	tag    string    // pay tag, computed once
	free   int64     // bytes before checking again that the peer still rides free
	quota  int64     // reserved from today's free quota
	day    time.Time // the day quota was taken from
	vouch  int64     // reserved from voucher credit
	paid   int64     // reserved from prepaid credit
	cut    bool
	closed bool
}

func newMeter(svc *ExitService, peer types.PeerID) *meter { return &meter{svc: svc, peer: peer} }

// spend accounts n bytes; false means the allowance is used up and the traffic must stop.
func (m *meter) spend(n int) bool {
	if n <= 0 {
		return true
	}
	need := int64(n)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cut {
		return false
	}
	if m.left() < need && !m.closed {
		m.svc.reserve(m, max(meterSlice, need-m.left()))
	}
	if m.left() < need {
		m.free, m.quota, m.vouch, m.paid = 0, 0, 0, 0 // what was allowed is spent, then refused
		m.cut = true
		return false
	}
	take := func(pool *int64) {
		t := min(*pool, need)
		*pool -= t
		need -= t
	}
	take(&m.free)
	take(&m.quota)
	take(&m.vouch)
	take(&m.paid)
	return true
}

func (m *meter) left() int64 { return m.free + m.quota + m.vouch + m.paid }

// close gives back what the stream reserved and did not use.
func (m *meter) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	m.svc.refund(m)
	m.free, m.quota, m.vouch, m.paid = 0, 0, 0, 0
}

// reserve tops m up by up to want bytes: free riding first, then today's free quota, then
// voucher credit, then prepaid credit. Called with m.mu held.
func (e *ExitService) reserve(m *meter, want int64) {
	e.policyMu.Lock()
	if e.freeLocked(m.peer) {
		e.policyMu.Unlock()
		m.free += want
		return
	}
	q := e.quotaLocked(m.peer)
	got := min(want, max(0, e.policy.FreeBytes-q.used))
	q.used += got
	if !m.day.Equal(q.day) {
		m.day = q.day // yesterday's leftover stays spent; refunds go to today
	}
	l, v, self := e.ledger, e.vouchers, e.self
	e.policyMu.Unlock()
	m.quota += got
	want -= got
	if want <= 0 || (l == nil && v == nil) {
		return
	}
	if m.tag == "" {
		m.tag = PayTag(m.peer, self)
	}
	if v != nil {
		got := v.Spend(m.tag, want)
		m.vouch += got
		want -= got
	}
	if l != nil && want > 0 {
		m.paid += l.Reserve(m.tag, want)
	}
}

// refund returns m's unused reservation to the pools it came from. Called with m.mu held.
func (e *ExitService) refund(m *meter) {
	if m.quota > 0 {
		e.policyMu.Lock()
		if q := e.quotas[m.peer]; q != nil && q.day.Equal(m.day) {
			q.used = max(0, q.used-m.quota)
		}
		e.policyMu.Unlock()
	}
	if m.vouch <= 0 && m.paid <= 0 {
		return
	}
	e.policyMu.Lock()
	l, v := e.ledger, e.vouchers
	e.policyMu.Unlock()
	if v != nil {
		v.Refund(m.tag, m.vouch)
	}
	if l != nil {
		l.Refund(m.tag, m.paid)
	}
}

// meteredStream spends the peer's allowance as traffic passes and cuts the tunnel when it runs out
type meteredStream struct {
	FramedStream
	m    *meter
	pace *pace // nil: not slowed down
}

func (m *meteredStream) account(n int) error {
	if !m.m.spend(n) {
		return ErrPaymentRequired
	}
	return nil
}

// CloseWrite passes the half-close on to the stream underneath.
func (m *meteredStream) CloseWrite() error {
	if cw, ok := m.FramedStream.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (m *meteredStream) Read(p []byte) (int, error) {
	n, err := m.FramedStream.Read(p)
	if aerr := m.account(n); aerr != nil {
		// what comes from the client is not passed on unless it is paid for: the chunk that
		// crosses the limit is dropped, so a client cannot get free traffic by sending a big one
		return 0, aerr
	}
	m.pace.throttle(n)
	return n, err
}

func (m *meteredStream) Write(p []byte) (int, error) {
	n, err := m.FramedStream.Write(p)
	if aerr := m.account(n); aerr != nil && err == nil {
		err = aerr
	}
	m.pace.throttle(n)
	return n, err
}
