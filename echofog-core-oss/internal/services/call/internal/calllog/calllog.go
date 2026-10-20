// Package calllog holds the small logging helpers shared by the call packages: a per-key rate
// limiter for events that can repeat per packet, and short forms of peer and call IDs.
//
// Group calls used to fail without a trace: dropped frames, rejected signals and dead links were
// only counted, if at all. Every such event is now logged, and the ones that can happen per packet
// go through a Limiter so a broken link costs one line every few seconds, not thousands.
package calllog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Every is the default interval of a limiter for per-packet events.
const Every = 10 * time.Second

// maxKeys bounds the limiter; keys are built from peers, streams and reasons of one call, so a
// real call stays far below it. Past it the table starts over instead of growing.
const maxKeys = 1024

// Limiter lets one log line per key through every interval and counts the ones it holds back.
type Limiter struct {
	every time.Duration
	now   func() time.Time

	mu      sync.Mutex
	last    map[string]time.Time
	skipped map[string]int
}

// New returns a limiter that lets one line per key through every interval.
func New(every time.Duration) *Limiter {
	return &Limiter{every: every, now: time.Now, last: map[string]time.Time{}, skipped: map[string]int{}}
}

// Allow reports whether a line for key may be logged now, and how many were held back since the
// last one that was. A nil limiter lets everything through.
func (l *Limiter) Allow(key string) (bool, int) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.every {
		l.skipped[key]++
		return false, 0
	}
	if len(l.last) >= maxKeys {
		l.last, l.skipped = map[string]time.Time{}, map[string]int{}
	}
	n := l.skipped[key]
	l.last[key], l.skipped[key] = now, 0
	return true, n
}

// Log writes one line at level if the key is due, with the number of lines folded into it as
// "suppressed". It checks the level first, so a Debug line that is filtered out costs no lock.
func (l *Limiter) Log(logger *slog.Logger, level slog.Level, key, msg string, args ...any) {
	if logger == nil || !logger.Enabled(context.Background(), level) {
		return
	}
	ok, skipped := l.Allow(key)
	if !ok {
		return
	}
	if skipped > 0 {
		args = append(args, "suppressed", skipped)
	}
	logger.Log(context.Background(), level, msg, args...)
}

// Peer is the short form of a peer ID used in logs: the first 12 hex characters.
func Peer(id types.PeerID) string {
	if id == (types.PeerID{}) {
		return "none"
	}
	return id.String()[:12]
}

// Call shortens a call or room ID for logs. Room IDs are public (they appear in signaling), but
// the full 29 characters only make lines harder to read.
func Call(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}
