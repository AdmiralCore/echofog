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

// maxStandby is the number of standby paths of the platform profile (wire.md §8.3,
// platforms.md §2): none under low_power, two otherwise.
func (r *Runtime) maxStandby() int {
	if r.cfg.Profile.LowPower {
		return 0
	}
	return 2
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

func (r *Runtime) recordPair(sid [16]byte, id uint64, pair session.Pair) {
	r.pathsMu.Lock()
	r.pathPairs[pathKey{sid: sid, id: id}] = pair
	r.pathsMu.Unlock()
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
	if !r.running() {
		return 0, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	est, err := r.existingSession(ctx, peer, opt)
	if err != nil {
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
	sessions := r.table.Sessions()
	eligible := make([]*session.Established, 0, len(sessions))
	for _, est := range sessions {
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
		for _, p := range old {
			if p == id {
				continue
			}
			if aerr := est.AbandonPath(p, frame.CodeNoError); aerr != nil && !errors.Is(aerr, mux.ErrNoPath) {
				errs = append(errs, fmt.Errorf("node: abandon path %d: %w", p, aerr))
			}
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

// addPathTo binds one more path. replace is the network-change mode: old paths may be dropped
// before binding when the platform keeps no standby paths.
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
	old, active, hasActive := est.Paths()
	if r.cfg.Profile.LowPower && hasActive && !replace {
		closeInjected()
		return 0, ErrTooManyPaths
	}
	if replace && r.cfg.Profile.LowPower {
		// No standby paths exist on this platform: drop the old path before the new one is
		// bound (the session keeps its 60 s no-path grace if the bind then fails).
		for _, p := range old {
			_ = est.AbandonPath(p, frame.CodeNoError)
		}
	} else if replace && len(old) >= 1+r.maxStandby() {
		// Make room inside the standby limit before binding so the mux does not have to pick
		// a victim on its own.
		for _, p := range old {
			if hasActive && p == active {
				continue
			}
			_ = est.AbandonPath(p, frame.CodeNoError)
		}
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
		if len(attempts) == 0 {
			return 0, fmt.Errorf("%w: %s", ErrNoRoute, est.Peer.DeviceID)
		}
		req := carrier.DialRequest{Card: card, DialContext: r.cfg.DialContext}
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

	id, err := est.BindCarrier(ctx, c, kind)
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
		r.policy.Succeeded(pair)
		r.recordPair(est.SID, id, pair)
	}
	return id, nil
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
