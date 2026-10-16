package exit_gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Refused streams used to vanish without a trace: the client answered its application with a bare
// SOCKS "connection refused" and the exit only bumped a counter. Both sides now log them, at most
// one line per reason every refusalLogEvery, with how many were folded into it.

const refusalLogEvery = 30 * time.Second

// Why an exit refused a stream.
const (
	RefuseDisabled = "exit disabled"      // the node does not share its connection
	RefusePayment  = "payment required"   // the free quota is used up and nothing is paid
	RefuseTarget   = "target not allowed" // private address, blocked port or a malformed target
	RefuseDial     = "dial failed"        // the exit could not reach the target
)

var refuseReasons = [...]string{RefuseDisabled, RefusePayment, RefuseTarget, RefuseDial}

// logLimiter lets one log line per key through every interval and counts the ones it holds back.
type logLimiter struct {
	every time.Duration
	now   func() time.Time

	mu      sync.Mutex
	last    map[string]time.Time
	skipped map[string]int
}

func newLogLimiter(every time.Duration) *logLimiter {
	return &logLimiter{every: every, now: time.Now, last: map[string]time.Time{}, skipped: map[string]int{}}
}

// allow reports whether a line for key may be logged now, and how many were held back since the
// last one that was.
func (l *logLimiter) allow(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.every {
		l.skipped[key]++
		return false, 0
	}
	if len(l.last) > 256 { // keys are reasons, a handful; this only guards against a bug
		l.last, l.skipped = map[string]time.Time{}, map[string]int{}
	}
	n := l.skipped[key]
	l.last[key], l.skipped[key] = now, 0
	return true, n
}

// refusalCounts counts an exit's refusals by reason.
type refusalCounts struct {
	by [len(refuseReasons)]atomic.Uint64
}

func (c *refusalCounts) add(reason string) {
	for i, r := range refuseReasons {
		if r == reason {
			c.by[i].Add(1)
			return
		}
	}
}

func (c *refusalCounts) snapshot() map[string]uint64 {
	m := make(map[string]uint64, len(refuseReasons))
	for i, r := range refuseReasons {
		if n := c.by[i].Load(); n > 0 {
			m[r] = n
		}
	}
	return m
}

// refuseCode is the reason behind a result code the exit sends back.
func refuseCode(code byte) string {
	switch code {
	case resultPayment:
		return RefusePayment
	case resultDialFailed:
		return RefuseDial
	default:
		return RefuseTarget
	}
}

// refuse counts and logs one refused stream. The target is not logged: an exit keeps no record
// of where its clients go.
func (e *ExitService) refuse(peer types.PeerID, reason string) {
	e.refused.Add(1)
	e.refusedBy.add(reason)
	e.logRefusalsOnce.Do(func() { e.refusalLog = newLogLimiter(refusalLogEvery) })
	if ok, skipped := e.refusalLog.allow(reason); ok {
		slog.Warn("refused stream", "component", "exit", "reason", reason, "peer", shortPeer(peer), "more_since_last", skipped)
	}
}

// RefusedByReason returns how many streams the exit refused, by reason (reasons never seen are left out).
func (e *ExitService) RefusedByReason() map[string]uint64 { return e.refusedBy.snapshot() }

// ExitRefusal names why a stream through the exit failed, for the client's log and UI.
func ExitRefusal(err error) string {
	switch {
	case errors.Is(err, ErrPaymentRequired):
		return "payment"
	case errors.Is(err, ErrExitDenied), errors.Is(err, ErrExitDisabled):
		return "denied"
	case errors.Is(err, ErrExitDialFailed):
		return "dial failed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	default:
		return "exit unreachable"
	}
}

// Refusal is the last stream the exit would not carry.
type Refusal struct {
	Reason string
	Host   string
	At     time.Time
}

// noteRefusal records and logs a stream the exit would not carry.
func (s *SOCKS5Server) noteRefusal(target string, err error) {
	if s.ctx != nil && s.ctx.Err() != nil {
		return // the proxy is stopping: nothing was refused
	}
	reason := ExitRefusal(err)
	s.refused.Add(1)
	s.lastRefusal.Store(&Refusal{Reason: reason, Host: target, At: time.Now()})
	s.refusalsOnce.Do(func() { s.refusalLog = newLogLimiter(refusalLogEvery) })
	if ok, skipped := s.refusalLog.allow(reason); ok {
		s.logger.Warn("exit refused: "+reason+" for "+target, "err", err, "more_since_last", skipped)
	}
}

// Refusals returns how many streams the exit refused and the last one (nil: none yet).
func (s *SOCKS5Server) Refusals() (uint64, *Refusal) {
	return s.refused.Load(), s.lastRefusal.Load()
}

func shortPeer(id types.PeerID) string { return id.String()[:12] }
