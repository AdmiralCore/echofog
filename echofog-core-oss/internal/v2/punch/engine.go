package punch

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	mrand "math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Timings of services.md §6.4.
const (
	// AttemptTime is how long one attempt (and the wait for the peer's offer) lasts.
	AttemptTime = 5 * time.Second
	// MaxAttempts is how many attempts one round makes.
	MaxAttempts = 2
	// OfferInterval is the least time between two PUNCH_OFFERs of a session, in both directions.
	OfferInterval = 30 * time.Second
	// RetryMin and RetryMax bound the pause before the next round of a session that stays relayed.
	RetryMin = 90 * time.Second
	RetryMax = 150 * time.Second
	// MaxInitDatagrams is the most Init datagrams the initiator sends in one attempt.
	MaxInitDatagrams = 24
	// JunkMin..JunkMax datagrams of JunkSizeMin..JunkSizeMax bytes, JunkGapMin..JunkGapMax apart,
	// go to each target of the responder.
	JunkMin, JunkMax         = 3, 6
	JunkSizeMin, JunkSizeMax = 32, 256
	JunkGapMin, JunkGapMax   = 10 * time.Millisecond, 40 * time.Millisecond
)

// Role is the end of the session the engine runs on. The initiator of the end-to-end session is
// also the noise3 initiator (services.md §6.4 step 4).
type Role uint8

const (
	Initiator Role = iota + 1
	Responder
)

// Link is the relayed session the punch is coordinated on.
type Link interface {
	SendControl(typ uint64, flags byte, payload []byte) error
	Ping(ctx context.Context) (time.Duration, error)
}

// Net is the datagram side of the node.
type Net interface {
	// OwnAddrs are the addresses to offer (at most 4 are used): the external address of the socket
	// that Dial and the responder's listener use.
	OwnAddrs() []netip.AddrPort
	// SendJunk sends one datagram from the noise3 listener's socket to to.
	SendJunk(to netip.AddrPort, payload []byte) error
	// Dial sends fresh noise3 Init datagrams (at most budget in all) to the targets and returns the
	// first flow whose inner QUIC handshake completes, as a carrier.
	Dial(ctx context.Context, targets []netip.AddrPort, budget int) (mux.Carrier, error)
}

// Config wires an Engine to one session.
type Config struct {
	Role  Role
	Link  Link
	Net   Net
	Clock carrier.Clock
	// Rand returns a uniform value in [0, n); nil uses math/rand/v2.
	Rand func(n int64) int64
	// Rules yields the target rules (PEER_ADDR IPs and the peer card's IPs) at the time of use.
	Rules func() Rules
	// Allowed reports whether punching is allowed now: same country (D21), feature path-migration
	// negotiated, profile and policy. A false answer makes the engine silent.
	Allowed func() bool
	// HasDirect reports that the session already has a direct path.
	HasDirect func() bool
	// Bind attaches the punched carrier to the session (PATH_BIND).
	Bind func(ctx context.Context, c mux.Carrier) error
}

// Engine coordinates hole punching on one relayed session. The initiator runs Run; both ends feed
// the control frames they receive to HandleControl.
type Engine struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	waitNonce uint64
	waitCh    chan frame.PunchOffer
	lastOffer time.Time // initiator: when our last PUNCH_OFFER went out
	lastPeer  time.Time // responder: when we last answered a PUNCH_OFFER
	answered  bool
	pend      pending
	junking   bool
}

// pending is what the responder holds between PUNCH_OFFER and PUNCH_SYNC.
type pending struct {
	nonce   uint64
	targets []netip.AddrPort
	expires time.Time
	syncs   int
}

// New returns an engine.
func New(cfg Config) *Engine {
	if cfg.Rand == nil {
		cfg.Rand = func(n int64) int64 { return mrand.Int64N(n) }
	}
	if cfg.Rules == nil {
		cfg.Rules = func() Rules { return Rules{} }
	}
	if cfg.Allowed == nil {
		cfg.Allowed = func() bool { return true }
	}
	if cfg.HasDirect == nil {
		cfg.HasDirect = func() bool { return false }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{cfg: cfg, ctx: ctx, cancel: cancel}
}

// Close stops the junk the engine still sends. Run ends with its own context.
func (e *Engine) Close() { e.cancel() }

func (e *Engine) now() time.Time { return e.cfg.Clock.Now() }

// uniform returns a duration in [lo, hi], in whole milliseconds.
func (e *Engine) uniform(lo, hi time.Duration) time.Duration {
	span := int64((hi - lo) / time.Millisecond)
	return lo + time.Duration(e.cfg.Rand(span+1))*time.Millisecond
}

func (e *Engine) uniformInt(lo, hi int) int { return lo + int(e.cfg.Rand(int64(hi-lo+1))) }

// sleep waits d on the injected clock.
func (e *Engine) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	ch := make(chan struct{})
	t := e.cfg.Clock.AfterFunc(d, func() { close(ch) })
	defer t.Stop()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-e.ctx.Done():
		return context.Canceled
	}
}

// Run is the initiator's loop: rounds of offer, sync and Init attempts until a direct path is bound,
// the session ends or ctx is cancelled. It returns nil after a successful bind or when the session
// already has a direct path.
func (e *Engine) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.cfg.HasDirect() {
			return nil
		}
		if e.cfg.Allowed() {
			bound, err := e.round(ctx)
			if err != nil {
				return err
			}
			if bound {
				return nil
			}
		}
		if err := e.sleep(ctx, e.uniform(RetryMin, RetryMax)); err != nil {
			return err
		}
	}
}

// round is one offer plus up to MaxAttempts Init attempts. It reports whether a carrier was bound.
func (e *Engine) round(ctx context.Context) (bool, error) {
	// PUNCH_OFFER at most once per OfferInterval per session.
	e.mu.Lock()
	wait := OfferInterval - e.now().Sub(e.lastOffer)
	first := e.lastOffer.IsZero()
	e.mu.Unlock()
	if !first && wait > 0 {
		if err := e.sleep(ctx, wait); err != nil {
			return false, err
		}
	}
	nonce := randNonce()
	ch := make(chan frame.PunchOffer, 1)
	e.mu.Lock()
	e.waitNonce, e.waitCh = nonce, ch
	e.lastOffer = e.now()
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.waitNonce, e.waitCh = 0, nil
		e.mu.Unlock()
	}()

	own := e.cfg.Net.OwnAddrs()
	if len(own) > frame.MaxPunchAddrs {
		own = own[:frame.MaxPunchAddrs]
	}
	if err := e.cfg.Link.SendControl(frame.TypePunchOffer, 0, frame.PunchOffer{Addrs: own, Nonce: nonce}.Append(nil)); err != nil {
		return false, nil // the session cannot carry it now; the next round retries
	}
	// The peer's answer: the same nonce, within one attempt time.
	waitCtx, cancelWait := context.WithCancel(ctx)
	stopWait := e.cfg.Clock.AfterFunc(AttemptTime, cancelWait)
	var peer frame.PunchOffer
	select {
	case peer = <-ch:
	case <-waitCtx.Done():
	}
	stopWait.Stop()
	cancelWait()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if peer.Nonce != nonce {
		return false, nil
	}
	targets := FilterTargets(peer.Addrs, e.cfg.Rules())
	if len(targets) == 0 {
		return false, nil
	}
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		pctx, cancelPing := context.WithTimeout(ctx, AttemptTime)
		rtt, err := e.cfg.Link.Ping(pctx)
		cancelPing()
		if err != nil {
			return false, ctx.Err()
		}
		if err := e.cfg.Link.SendControl(frame.TypePunchSync, 0, binary.BigEndian.AppendUint64(nil, nonce)); err != nil {
			return false, nil
		}
		// A starts after RTT/2 so that its first Init meets the junk B starts on receipt.
		if err := e.sleep(ctx, rtt/2); err != nil {
			return false, err
		}
		actx, cancel := context.WithCancel(ctx)
		stop := e.cfg.Clock.AfterFunc(AttemptTime, cancel)
		c, err := e.cfg.Net.Dial(actx, targets, MaxInitDatagrams)
		stop.Stop()
		cancel()
		if err == nil && c != nil {
			if berr := e.cfg.Bind(ctx, c); berr != nil {
				_ = c.Close()
				continue
			}
			return true, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return false, cerr
		}
	}
	return false, nil
}

func randNonce() uint64 {
	for {
		var b [8]byte
		_, _ = crand.Read(b[:])
		if v := binary.BigEndian.Uint64(b[:]); v != 0 {
			return v
		}
	}
}

// HandleControl feeds a PUNCH_OFFER or PUNCH_SYNC received on the session. Malformed frames are
// ignored.
func (e *Engine) HandleControl(f frame.Frame) {
	switch f.Type {
	case frame.TypePunchOffer:
		o, err := frame.ParsePunchOffer(f.Payload)
		if err != nil || o.Nonce == 0 {
			return
		}
		e.onOffer(o)
	case frame.TypePunchSync:
		v, err := frame.ParsePing(f.Payload)
		if err != nil {
			return
		}
		e.onSync(binary.BigEndian.Uint64(v[:]))
	}
}

func (e *Engine) onOffer(o frame.PunchOffer) {
	if e.cfg.Role == Initiator {
		// The initiator takes an offer only as the answer to its own.
		e.mu.Lock()
		ch := e.waitCh
		match := ch != nil && o.Nonce == e.waitNonce
		e.mu.Unlock()
		if match {
			select {
			case ch <- o:
			default:
			}
		}
		return
	}
	if e.cfg.Role != Responder {
		return
	}
	// F2: the cheap rate check comes before Allowed, which verifies a card and looks up countries.
	now := e.now()
	e.mu.Lock()
	limited := e.answered && now.Sub(e.lastPeer) < OfferInterval
	e.mu.Unlock()
	if limited || !e.cfg.Allowed() {
		return
	}
	e.mu.Lock()
	if e.answered && now.Sub(e.lastPeer) < OfferInterval {
		e.mu.Unlock()
		return
	}
	e.answered, e.lastPeer = true, now
	e.pend = pending{nonce: o.Nonce, targets: FilterTargets(o.Addrs, e.cfg.Rules()), expires: now.Add(OfferInterval)}
	e.mu.Unlock()
	own := e.cfg.Net.OwnAddrs()
	if len(own) > frame.MaxPunchAddrs {
		own = own[:frame.MaxPunchAddrs]
	}
	_ = e.cfg.Link.SendControl(frame.TypePunchOffer, 0, frame.PunchOffer{Addrs: own, Nonce: o.Nonce}.Append(nil))
}

func (e *Engine) onSync(nonce uint64) {
	if e.cfg.Role != Responder || nonce == 0 {
		return
	}
	now := e.now()
	e.mu.Lock()
	p := &e.pend
	if p.nonce != nonce || now.After(p.expires) || p.syncs >= MaxAttempts || e.junking || len(p.targets) == 0 {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	if !e.cfg.Allowed() {
		return
	}
	e.mu.Lock()
	if p.nonce != nonce || p.syncs >= MaxAttempts || e.junking {
		e.mu.Unlock()
		return
	}
	p.syncs++
	e.junking = true
	targets := append([]netip.AddrPort(nil), p.targets...)
	e.mu.Unlock()
	go e.junk(targets)
}

// junk opens this end's NAT towards every target: JunkMin..JunkMax random datagrams, JunkGapMin..
// JunkGapMax apart, one sender per target.
func (e *Engine) junk(targets []netip.AddrPort) {
	var wg sync.WaitGroup
	for _, to := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := e.uniformInt(JunkMin, JunkMax)
			for i := 0; i < n; i++ {
				if i > 0 && e.sleep(e.ctx, e.uniform(JunkGapMin, JunkGapMax)) != nil {
					return
				}
				buf := make([]byte, e.uniformInt(JunkSizeMin, JunkSizeMax))
				_, _ = crand.Read(buf)
				_ = e.cfg.Net.SendJunk(to, buf)
			}
		}()
	}
	wg.Wait()
	e.mu.Lock()
	e.junking = false
	e.mu.Unlock()
}
