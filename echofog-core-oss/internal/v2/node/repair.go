package node

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// Repair of a session left without a working path (T3.13). When the active path of a session is
// declared failing and no healthy path can take over, the dialer does not wait for the path to
// die (a minute) and the session to be closed (another minute): it opens a replacement at once,
// as a new path when path-migration was negotiated and as a new session otherwise. The attempts
// are spaced (one in flight, exponentially, six at most) and their failures are not the H15 penalty
// signal: the pair worked a moment ago, the network is what is away, and a backoff that doubles
// with every failure inside the outage would turn seconds into minutes.
const (
	repairAttempts = 6
	repairBase     = time.Second
	repairCap      = 8 * time.Second
)

// pathFailing is the reaction to mux.Session.SetOnPathFailing.
func (r *Runtime) pathFailing(s *mux.Session, _ mux.PathFailure) {
	est := r.establishedOf(s)
	if est == nil || est.Role != session.RoleInitiator {
		// The accepting side has nothing to dial with in general; the dialer repairs.
		return
	}
	r.startRepair(est)
}

func (r *Runtime) establishedOf(s *mux.Session) *session.Established {
	table := r.table
	if table == nil {
		return nil
	}
	for _, est := range table.Sessions() {
		if est != nil && est.Session == s {
			return est
		}
	}
	return nil
}

// startRepair runs one repair per session, tracked by the runtime.
func (r *Runtime) startRepair(est *session.Established) {
	r.pathsMu.Lock()
	if r.repairing[est.SID] {
		r.pathsMu.Unlock()
		return
	}
	r.repairing[est.SID] = true
	r.pathsMu.Unlock()
	done := func() {
		r.pathsMu.Lock()
		delete(r.repairing, est.SID)
		r.pathsMu.Unlock()
	}
	if !r.track() {
		done()
		return
	}
	go func() {
		defer r.wg.Done()
		defer done()
		r.repair(est)
	}()
}

func (r *Runtime) repair(est *session.Established) {
	ctx := r.ctx
	if ctx == nil {
		return
	}
	delay := repairBase
	for try := 0; try < repairAttempts; try++ {
		if try > 0 {
			if !r.repairWait(ctx, jittered(delay)) {
				return
			}
			delay = min(2*delay, repairCap)
		}
		select {
		case <-est.Session.Done():
			return
		default:
		}
		if est.Session.GoingAway() || (try > 0 && est.Session.Healthy()) {
			return // the path came back, or the session is draining on purpose
		}
		opt := r.repairOpt
		opt.repair = true
		if est.HasFeature(frame.FeaturePathMigration) {
			// The new path becomes the active one; the failing path stays a standby until it
			// recovers or dies.
			if _, err := r.addPathTo(ctx, est, opt, true); err == nil {
				return
			}
			continue
		}
		// No path-migration: the session cannot take another carrier. End it and dial a new one;
		// streams on it were lost with the path anyway.
		_ = est.Session.Close()
		if _, _, err := r.established(ctx, DeviceID(est.Peer.DeviceID), opt); err == nil {
			return
		}
	}
}

// repairWait waits d on the runtime's clock; false when the runtime stops first.
func (r *Runtime) repairWait(ctx context.Context, d time.Duration) bool {
	fired := make(chan struct{})
	t := r.carrierClock.AfterFunc(d, func() { close(fired) })
	select {
	case <-fired:
		return true
	case <-ctx.Done():
		if t != nil {
			t.Stop()
		}
		return false
	}
}

// jittered spreads d by +-20 % (AD13).
func jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// repairCard is the card the session was dialed with; nil when unknown.
func (r *Runtime) repairCard(sid [16]byte) *objects.Card { return nil }
