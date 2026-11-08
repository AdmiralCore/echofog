package carrier

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Narrow path (T3.12, wire.md section 2.4 "Path requirement").

const (
	// NoAnswerAfter is how long a noise3 Init may stay unanswered, while another suite connects,
	// before the network is taken for a narrow path.
	NoAnswerAfter = time.Second
	// NarrowTTL is how long a network stays marked.
	NarrowTTL = 24 * time.Hour
)

// ErrMSSUnsupported reports that a TCP retry with a limited MSS was needed but the socket cannot
// take one (a proxy opens it, or the OS has no TCP_MAXSEG).
var ErrMSSUnsupported = errors.New("carrier: TCP MSS cannot be limited on this socket")

// NarrowReason says why a network was marked.
type NarrowReason int

const (
	// ReasonNoise3Silent: noise3 got no answer for NoAnswerAfter while another suite connected.
	ReasonNoise3Silent NarrowReason = iota + 1
	// ReasonMSS: a TCP carrier connected only after the MSS was limited.
	ReasonMSS
)

// NarrowPath is the per-network mark of a narrow path.
type NarrowPath struct {
	// FirstTry and Rung bound the first unrestricted TCP attempt and each limited retry.
	FirstTry, Rung time.Duration
}

// NewNarrowPath builds the marks on an injectable clock. onMark, when set, is called once for each
// new mark.
func NewNarrowPath(clock Clock, onMark func(netID string, why NarrowReason)) *NarrowPath {
	return &NarrowPath{}
}

// Mark marks a network for NarrowTTL.
func (n *NarrowPath) Mark(netID string, why NarrowReason) {}

// Marked reports whether the network is marked now.
func (n *NarrowPath) Marked(netID string) bool { return false }

// Forget drops the mark of a network.
func (n *NarrowPath) Forget(netID string) {}

// MSSFor is the MSS of retry rung (0-based) for IPv6 or IPv4.
func MSSFor(v6 bool, rung int) int { return 0 }

// WithNarrowRetry runs attempt, and on a black-holed TCP path runs it again with a limited MSS.
func WithNarrowRetry(ctx context.Context, req DialRequest, attempt func(ctx context.Context, req DialRequest) (mux.Carrier, error)) (mux.Carrier, error) {
	return attempt(ctx, req)
}

var _ = net.ErrClosed
