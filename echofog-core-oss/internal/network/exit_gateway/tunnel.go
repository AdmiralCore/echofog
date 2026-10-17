package exit_gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	resultOK         byte = 0
	resultDenied     byte = 1
	resultDialFailed byte = 2
	resultPayment    byte = 3

	maxTargetLen = 300
)

var (
	ErrPaymentRequired = errors.New("exit node requires payment after the free test traffic")
	ErrExitDisabled    = errors.New("exit node is not accepting traffic")
	ErrExitDenied      = errors.New("exit node refused the target")
	ErrExitDialFailed  = errors.New("exit node could not reach the target")
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

// DialExitStream asks the exit peer to connect to targetAddr and returns the raw byte pipe once it confirms
func (d *P2PDialer) DialExitStream(ctx context.Context, targetAddr string) (io.ReadWriteCloser, error) {
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

	type reply struct {
		t   net_types.MessageType
		b   []byte
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		t, b, err := st.ReadFrame()
		ch <- reply{t, b, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			st.Close()
			return nil, r.err
		}
		if r.t != net_types.TypeExitConnectResult || len(r.b) < 1 {
			st.Close()
			return nil, ErrExitProtocol
		}
		switch r.b[0] {
		case resultOK:
			return st, nil
		case resultDenied:
			st.Close()
			return nil, ErrExitDenied
		case resultPayment:
			st.Close()
			return nil, ErrPaymentRequired
		default:
			st.Close()
			return nil, ErrExitDialFailed
		}
	case <-ctx.Done():
		st.Close()
		return nil, ctx.Err()
	}
}

// ExitService is the egress side: it accepts tunnel requests from peers and splices them to the Internet
type ExitService struct {
	splicer      *ExitSplicer
	enabled      atomic.Bool
	allowPrivate bool
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

	// payments (nil: this exit takes none; a priced exit then only serves its free quota)
	ledger     *Ledger
	self       types.PeerID
	payAddress string
	onPayment  atomic.Pointer[func(micro, bytes int64)]

	// vouchers from clients' TON vaults (nil: this exit takes none)
	vouchers *VoucherBook
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
func (e *ExitService) AllowPrivateTargets(on bool) { e.allowPrivate = on }

func NewExitService() *ExitService {
	return &ExitService{
		splicer:     NewExitSplicer(8 * time.Second),
		dialTimeout: 8 * time.Second,
		quotas:      make(map[types.PeerID]*peerQuota),
		resolver: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
	}
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

	if !e.enabled.Load() {
		e.refuse(peer, RefuseDisabled)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultDenied})
		return
	}

	if e.allowance(peer) == 0 {
		e.refuse(peer, RefusePayment)
		_ = st.WriteFrame(net_types.TypeExitConnectResult, []byte{resultPayment})
		return
	}

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
	_ = e.splicer.Splice(ctx, &meteredStream{FramedStream: st, svc: e, peer: peer}, conn)
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
		if len(e.quotas) > 10000 {
			e.quotas = make(map[types.PeerID]*peerQuota)
		}
		q = &peerQuota{day: today}
		e.quotas[peer] = q
	}
	return q
}

// meteredStream spends the peer's allowance as traffic passes and cuts the tunnel when it runs out
type meteredStream struct {
	FramedStream
	svc  *ExitService
	peer types.PeerID
}

func (m *meteredStream) account(n int) error {
	if !m.svc.consume(m.peer, int64(n)) {
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
	return n, err
}

func (m *meteredStream) Write(p []byte) (int, error) {
	n, err := m.FramedStream.Write(p)
	if aerr := m.account(n); aerr != nil && err == nil {
		err = aerr
	}
	return n, err
}
