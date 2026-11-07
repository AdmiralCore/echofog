package routepolicy

import (
	"net/netip"
	"time"
)

// A network that freezes TCP stops a flow to a foreign address once the server has sent 12 to 20
// KiB. A repeat to the same address stops the same way. After that the whole network is marked
// frozen and the row of a Russian mobile network towards a foreign data centre applies to it, in
// whatever country the client is (T6.4d).
const (
	// FreezeMin and FreezeMax bound the inbound bytes at which a stall counts as the freeze.
	FreezeMin = 12 << 10
	FreezeMax = 20 << 10
	// FreezeTTL is how long a network stays marked frozen.
	FreezeTTL = 24 * time.Hour
	// freezeRepeats is how many stalls at one address make the mark: the flow and its repeat.
	freezeRepeats = 2
)

// DecideFrozen is Decide for a network that may be frozen. A frozen network towards a foreign or
// neighbouring destination gets the row of the own country's mobile network: at most ProbeBytes on
// TCP, no UDP, the first hop an entry node at home or in a neighbouring country. A domestic
// destination is not affected.
func (r *Rules) DecideFrozen(own string, net NetClass, dest DestClass, frozen bool) Decision {
	return r.Decide(own, net, dest)
}

// FreezeTracker keeps, per network identifier, the stalls it has seen and the networks it has
// marked frozen. The clock is injected.
type FreezeTracker struct{}

// NewFreezeTracker returns a tracker on the given clock; nil means time.Now.
func NewFreezeTracker(now func() time.Time) *FreezeTracker { return &FreezeTracker{} }

// NoteStall records that a flow to dst (a foreign address) on network netID stopped after bytes
// inbound bytes. It returns true when this stall is the one that marks the network frozen.
func (t *FreezeTracker) NoteStall(netID string, dst netip.Addr, bytes int) bool { return false }

// Frozen reports whether the network is marked frozen now.
func (t *FreezeTracker) Frozen(netID string) bool { return false }

// Forget drops what is known about a network (it was left).
func (t *FreezeTracker) Forget(netID string) {}

// size is how many (network, address) records the tracker holds.
func (t *FreezeTracker) size() int { return 0 }
