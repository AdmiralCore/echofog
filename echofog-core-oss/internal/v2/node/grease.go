package node

import (
	"errors"
	mrand "math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// D18 requires a GREASE frame of type 0x3e at random intervals and sets no value. The range is
// wide enough to exercise the "unknown is ignored" path on every session without adding
// meaningful traffic; it is local policy, so it never changes the wire format.
const (
	greaseMin = 30 * time.Second
	greaseMax = 90 * time.Second
)

// defaultGreaseInterval draws the next GREASE delay uniformly from [greaseMin, greaseMax].
func defaultGreaseInterval() time.Duration {
	return greaseMin + time.Duration(mrand.Int64N(int64(greaseMax-greaseMin)+1))
}

// greaseLoop sends one GREASE frame per random interval on the session's control stream (D18).
// A receiver skips it; the loop stops with the session or with the runtime.
func (r *Runtime) greaseLoop(s *mux.Session) {
	defer r.wg.Done()
	interval := r.cfg.GreaseInterval
	if interval == nil {
		interval = defaultGreaseInterval
	}
	fire := make(chan struct{}, 1)
	for {
		if s.GoingAway() {
			return
		}
		timer := r.carrierClock.AfterFunc(interval(), func() {
			select {
			case fire <- struct{}{}:
			default:
			}
		})
		select {
		case <-fire:
		case <-s.Done():
			timer.Stop()
			return
		case <-r.ctx.Done():
			timer.Stop()
			return
		}
		timer.Stop()
		if err := s.SendGrease(); err != nil {
			if errors.Is(err, mux.ErrSessionClosed) || errors.Is(err, mux.ErrGoingAway) {
				return
			}
			// A session without an active path retries on the next tick; any other write
			// failure is logged and does not stop the session.
			r.log.Debug("node: GREASE frame not sent", "err", err)
		}
	}
}
