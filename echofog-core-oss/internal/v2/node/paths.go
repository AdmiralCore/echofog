package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// pathKey identifies one path of one session for the local carrier policy.
type pathKey struct {
	sid [16]byte
	id  uint64
}

// pathHooks builds the session callbacks: the binder learns the session before it is returned
// (so a peer can bind a path immediately), and every path failure updates the carrier policy.
func (r *Runtime) pathHooks() (func(*mux.Session), func(mux.PathFailure)) {
	onEstablished := func(s *mux.Session) {
		r.binder.Add(s)
		go func() {
			<-s.Done()
			r.releaseSessionPathPairs(s.SID())
		}()
	}
	onPathFailed := func(f mux.PathFailure) { r.pathFailed(f) }
	return onEstablished, onPathFailed
}

func (r *Runtime) pathFailed(f mux.PathFailure) {
	key := pathKey{sid: f.SID, id: f.PathID}
	r.pathsMu.Lock()
	pair, ok := r.pathPairs[key]
	delete(r.pathPairs, key)
	r.deadPaths[key] = struct{}{}
	r.pathsMu.Unlock()
	if !ok {
		return
	}
	if errors.Is(f.Cause, mux.ErrPathAbandoned) {
		r.policy.Release(pair)
	} else {
		// A reset, a handshake timeout or a dead path is the penalty signal of H15.
		r.policy.Penalize(pair)
	}
}

// recordPair stores the carrier pair of a newly bound path. It refuses when the path or the
// session already died: the failure hook then ran before this record and the slot is not ours to
// keep. The caller releases the slot when it returns false.
func (r *Runtime) recordPair(sid [16]byte, id uint64, pair session.Pair, done <-chan struct{}) bool {
	key := pathKey{sid: sid, id: id}
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()
	if _, dead := r.deadPaths[key]; dead {
		return false
	}
	select {
	case <-done:
		return false
	default:
	}
	r.pathPairs[key] = pair
	return true
}

func (r *Runtime) releaseSessionPathPairs(sid [16]byte) {
	r.pathsMu.Lock()
	var pairs []session.Pair
	for k, pair := range r.pathPairs {
		if k.sid == sid {
			delete(r.pathPairs, k)
			pairs = append(pairs, pair)
		}
	}
	for k := range r.deadPaths {
		if k.sid == sid {
			delete(r.deadPaths, k)
		}
	}
	r.pathsMu.Unlock()
	for _, pair := range pairs {
		r.policy.Release(pair)
	}
}

func (r *Runtime) sessionHosts(sid [16]byte) map[string]bool {
	hosts := map[string]bool{}
	r.pathsMu.Lock()
	for k, pair := range r.pathPairs {
		if k.sid == sid && pair.Host != "" {
			hosts[pair.Host] = true
		}
	}
	r.pathsMu.Unlock()
	return hosts
}

// AddPath opens one more carrier to the peer of a live session and binds it with PATH_BIND
// (wire.md §8.2). It never starts a new session. Under low_power a session with a live path is
// not extended (platforms.md §2); otherwise at most two standby paths are kept, and the oldest
// beyond them is abandoned by the mux (wire.md §8.3).
//
// opt.Carrier, opt.CarrierFunc and opt.Attempts work as in DialOpt. For an injected carrier the
// carrier pair policy is not applied: it is a test or relay-circuit input, not a TLS dial.
func (r *Runtime) AddPath(ctx context.Context, peer DeviceID, opt DialOpt) (uint64, error) {
	closeInjected := func() {
		if opt.Carrier != nil {
			_ = opt.Carrier.Close()
		}
	}
	if !r.running() {
		closeInjected()
		return 0, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	est, err := r.existingSession(ctx, peer, opt)
	if err != nil {
		closeInjected()
		return 0, err
	}
	return r.addPathTo(ctx, est, opt, false)
}

// NetworkChanged reacts to a local network change (wire.md §8.2–§8.3): every live session that
// negotiated path-migration opens a new path on the new network and abandons its old paths. The
// session, its keys and its peer authentication survive.
//
// opt supplies the new carriers when the caller has them (tests, a platform callback that
// rebuilds the sockets); with an empty opt the carrier plan is dialed again over the new
// routing. An injected single Carrier serves one session only.
func (r *Runtime) NetworkChanged(ctx context.Context, opt DialOpt) error {
	if !r.running() {
		return ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// What peers saw before says nothing about the new uplink: the public IP and the NAT class
	// are decided again from new reports.
	r.observer.Reset()
	r.pm.Confirm(netip.Addr{})
	r.pm.Rediscover() // the old router holds the mappings, not the new one
	sessions := r.table.Sessions()
	eligible := make([]*session.Established, 0, len(sessions))
	for _, est := range sessions {
		// The table keeps a draining session until its successor replaces it (T3.7b); a network
		// change only migrates live ones.
		if !liveSession(est) {
			continue
		}
		if est.HasFeature(frame.FeaturePathMigration) {
			eligible = append(eligible, est)
		}
	}
	if opt.Carrier != nil && len(eligible) > 1 {
		return errors.New("node: one injected carrier serves one session")
	}
	var errs []error
	moved := 0
	for _, est := range eligible {
		old, _, _ := est.Paths()
		id, err := r.addPathTo(ctx, est, opt, true)
		if err != nil {
			errs = append(errs, fmt.Errorf("node: path for %x: %w", est.Peer.DeviceID, err))
			continue
		}
		if err := r.abandonReplaced(est, old, id); err != nil {
			// The switch did not complete: roll the new path back and report a real failure.
			// moved and the operator event stay untouched.
			if rerr := est.AbandonPath(id, frame.CodeNoError); rerr != nil && !errors.Is(rerr, mux.ErrNoPath) {
				err = errors.Join(err, fmt.Errorf("rollback of path %d: %w", id, rerr))
			}
			errs = append(errs, fmt.Errorf("node: network change for %x: %w", est.Peer.DeviceID, err))
			continue
		}
		moved++
	}
	if moved > 0 {
		r.events.Emit(Event{
			Kind:    EventNetworkChanged,
			Message: fmt.Sprintf("network changed: %d session(s) moved to new paths", moved),
			At:      time.Now(),
		})
	}
	return errors.Join(errs...)
}

// abandonReplaced abandons the old paths of a network change after the new path is active. A path
// that the standby trim already removed (ErrNoPath / no longer live) counts as abandoned.
func (r *Runtime) abandonReplaced(est *session.Established, old []uint64, keep uint64) error {
	ids, _, _ := est.Paths()
	live := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	for _, p := range old {
		if p == keep || !live[p] {
			continue
		}
		if err := est.AbandonPath(p, frame.CodeNoError); err != nil && !errors.Is(err, mux.ErrNoPath) {
			return fmt.Errorf("abandon old path %d: %w", p, err)
		}
	}
	return nil
}

// AbandonPath tells the peer that a path is no longer used and closes it (wire.md §8.3).
func (r *Runtime) AbandonPath(_ context.Context, peer DeviceID, id uint64) error {
	if !r.running() {
		return ErrClosed
	}
	est := r.table.ByPeer([32]byte(peer))
	if est == nil {
		return fmt.Errorf("%w: %s", ErrNoSession, peer)
	}
	return est.AbandonPath(id, frame.CodeNoError)
}

// Paths returns the live path IDs and the active path of the session to peer.
func (r *Runtime) Paths(peer DeviceID) (ids []uint64, active uint64, ok bool) {
	if !r.running() || r.table == nil {
		return nil, 0, false
	}
	est := r.table.ByPeer([32]byte(peer))
	if est == nil {
		return nil, 0, false
	}
	return est.Paths()
}

func (r *Runtime) existingSession(ctx context.Context, peer DeviceID, opt DialOpt) (*session.Established, error) {
	local, err := r.dialIdentity(ctx, opt)
	if err != nil {
		return nil, err
	}
	if local.Device == nil {
		return nil, fmt.Errorf("%w: no local device", ErrConfig)
	}
	est := r.table.Lookup(local.Device.ID(), [32]byte(peer))
	if est == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSession, peer)
	}
	return est, nil
}

// addPathTo binds one more path. replace is the network-change mode: the new path becomes active
// before the standby trim, so on a platform with no standby slots it is the old path that is
// abandoned (wire.md §8.3, platforms.md §2). The caller abandons the remaining old paths after
// this returns and rolls the new one back if that fails.
func (r *Runtime) addPathTo(ctx context.Context, est *session.Established, opt DialOpt, replace bool) (uint64, error) {
	closeInjected := func() {
		if opt.Carrier != nil {
			_ = opt.Carrier.Close()
		}
	}
	if est == nil || est.Session == nil {
		closeInjected()
		return 0, fmt.Errorf("%w: no session", ErrNoSession)
	}
	if !est.HasFeature(frame.FeaturePathMigration) {
		closeInjected()
		return 0, ErrPathFeature
	}
	if _, _, hasActive := est.Paths(); r.cfg.Profile.LowPower && hasActive && !replace {
		closeInjected()
		return 0, ErrTooManyPaths
	}

	kind := opt.PathKind
	var c mux.Carrier
	var pair session.Pair
	counted := false
	switch {
	case opt.Carrier != nil:
		c = opt.Carrier
	case opt.CarrierFunc != nil:
		var err error
		c, err = opt.CarrierFunc(ctx)
		if err != nil {
			return 0, err
		}
	default:
		card := opt.Card
		if card == nil {
			card = r.cardBook.Latest([32]byte(est.Peer.DeviceID))
		}
		if card == nil {
			return 0, fmt.Errorf("%w: %s", ErrNoPeerCard, est.Peer.DeviceID)
		}
		attempts := opt.Attempts
		if len(attempts) == 0 {
			attempts = carrier.Plan(card, r.localInfo)
		}
		attempts = r.pathAttempts(est, attempts)
		// A new path is a new direct dial: the country policy applies to it as to the first one.
		var perr error
		if attempts, _, perr = r.applyRoutePolicy(card, attempts, false); perr != nil {
			return 0, perr
		}
		if len(attempts) == 0 {
			return 0, fmt.Errorf("%w: %s", ErrNoRoute, est.Peer.DeviceID)
		}
		req := carrier.DialRequest{Card: card, DialContext: r.watchedDialContext(r.cfg.DialContext)}
		dialCfg := carrier.DialConfig{Registry: r.registry, Clock: r.carrierClock}
		var last error
		for _, a := range attempts {
			if a.Kind != carrier.AttemptCarrier || a.Addr == nil {
				continue
			}
			p := pairOfAttempt(a)
			if err := r.policy.Acquire(p); err != nil {
				last = err
				continue
			}
			// One attempt at a time: a third carrier to the same pair must not be opened, so
			// happy eyeballs over several attempts is deliberately not used here (H11).
			dc, _, err := carrier.Dial(ctx, req, []carrier.Attempt{a}, dialCfg)
			if err != nil {
				r.policy.Penalize(p)
				last = err
				continue
			}
			c, pair, counted = dc, p, true
			break
		}
		if c == nil {
			if last == nil {
				last = ErrNoRoute
			}
			return 0, last
		}
	}

	var (
		id  uint64
		err error
	)
	if replace {
		id, err = est.BindCarrierActive(ctx, c, kind)
	} else {
		id, err = est.BindCarrier(ctx, c, kind)
	}
	if err != nil {
		if counted {
			if errors.Is(err, mux.ErrSessionClosed) || errors.Is(err, mux.ErrFeatureNotSelected) {
				r.policy.Release(pair)
			} else {
				r.policy.Penalize(pair)
			}
		}
		return 0, err
	}
	if counted {
		if r.recordPair(est.SID, id, pair, est.Session.Done()) {
			r.policy.Succeeded(pair)
		} else {
			// The path died (or the session closed) before its carrier slot was recorded; the
			// failure hook could not see it. Release the slot here.
			r.policy.Release(pair)
		}
	}
	return id, nil
}

// reserveAttempts applies the H11 carrier limit to a happy-eyeballs plan before any handshake
// starts: at most MaxPerPair attempts per (address, SNI), and no attempt to a pair inside a
// backoff window. It returns one reserved policy slot per allowed attempt; the caller settles
// them with settleReservations or failReservations.
func (r *Runtime) reserveAttempts(attempts []carrier.Attempt) ([]carrier.Attempt, []session.Pair, error) {
	out := make([]carrier.Attempt, 0, len(attempts))
	reserved := make([]session.Pair, 0, len(attempts))
	counts := map[session.Pair]int{}
	direct := 0
	var last error
	for _, a := range attempts {
		if a.Kind != carrier.AttemptCarrier || a.Addr == nil {
			out = append(out, a)
			continue
		}
		direct++
		p := pairOfAttempt(a)
		if counts[p] >= r.policy.MaxPerPair() {
			continue
		}
		if err := r.policy.Acquire(p); err != nil {
			last = err
			continue
		}
		counts[p]++
		reserved = append(reserved, p)
		out = append(out, a)
	}
	if direct == 0 {
		// No direct suite: let carrier.Dial report ErrNeedRelay (the relay is T6.1).
		return attempts, nil, nil
	}
	if len(reserved) == 0 {
		if last == nil {
			last = ErrNoRoute
		}
		return nil, nil, fmt.Errorf("node: no carrier attempt allowed: %w", last)
	}
	return out, reserved, nil
}

// settleReservations keeps one slot for the winning pair and releases the others.
func (r *Runtime) settleReservations(reserved []session.Pair, chosen session.Pair) {
	kept := false
	for _, p := range reserved {
		if p == chosen && !kept {
			kept = true
			continue
		}
		r.policy.Release(p)
	}
}

// failReservations penalties the first attempt's pair and releases the rest. A carrier.Dial
// failure is a reset/timeout (H15); the first attempt is the one the plan preferred.
func (r *Runtime) failReservations(reserved []session.Pair) {
	if len(reserved) == 0 {
		return
	}
	r.policy.Penalize(reserved[0])
	for _, p := range reserved[1:] {
		r.policy.Release(p)
	}
}

// releaseReservations returns every reserved slot without a penalty (a caller cancellation).
func (r *Runtime) releaseReservations(reserved []session.Pair) {
	for _, p := range reserved {
		r.policy.Release(p)
	}
}

// pathAttempts puts an address on another family and an unused address first: on a network
// change the new carrier must not repeat the old (address, SNI) pair (H15).
func (r *Runtime) pathAttempts(est *session.Established, attempts []carrier.Attempt) []carrier.Attempt {
	used := r.sessionHosts(est.SID)
	cur := est.Remote.Addr()
	if cur.IsValid() {
		used[cur.String()] = true
	}
	var otherFamily, unused, rest []carrier.Attempt
	for _, a := range attempts {
		if a.Kind != carrier.AttemptCarrier || a.Addr == nil {
			continue
		}
		ip := attemptIP(a.Addr)
		if used[attemptHost(a.Addr)] {
			rest = append(rest, a)
			continue
		}
		if ip.IsValid() && cur.IsValid() && ip.Is6() != cur.Is6() {
			otherFamily = append(otherFamily, a)
			continue
		}
		unused = append(unused, a)
	}
	out := append(otherFamily, unused...)
	out = append(out, rest...)
	if len(out) == 0 {
		return attempts
	}
	return out
}

func pairOfAttempt(a carrier.Attempt) session.Pair {
	if a.Addr == nil {
		return session.Pair{}
	}
	return session.Pair{Host: attemptHost(a.Addr), SNI: objects.SuiteParams(a.Addr).GetSni()}
}

// attemptHost returns the dialed host: the URL host for ws2 and the literal host otherwise.
func attemptHost(a *pb.Addr) string {
	if a == nil {
		return ""
	}
	host := a.GetHost()
	if a.GetSuite() == objects.SuiteWS2 {
		if u, err := url.Parse(a.GetUrl()); err == nil {
			host = u.Hostname()
		}
	}
	return strings.Trim(host, "[]")
}

func attemptIP(a *pb.Addr) netip.Addr {
	ip, err := netip.ParseAddr(attemptHost(a))
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

// penalizeDial reports whether a failed dial is the H15 penalty signal (a reset or a handshake
// timeout) rather than a local or peer policy decision.
func penalizeDial(err error) bool {
	if err == nil {
		return false
	}
	var rejected *session.ErrRejected
	if errors.As(err, &rejected) {
		// The peer answered with REJECT: the carrier worked, the policy did not.
		return false
	}
	return !errors.Is(err, session.ErrConfig)
}
