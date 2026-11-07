package routepolicy

import (
	"net/netip"
	"sync"
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
	// FreezeTTL is how long a network stays marked frozen, and how long a lone stall waits for its
	// repeat.
	FreezeTTL = 24 * time.Hour
	// FreezeTTLNoEntry is how long a network stays marked when the client knows no verified entry:
	// the mark then costs the direct foreign paths and gives nothing back, so it is short.
	FreezeTTLNoEntry = time.Hour
	// FreezeAnswerWindow is how old an answer on another flow may be to excuse a stall.
	FreezeAnswerWindow = 60 * time.Second

	freezeRepeats   = 2  // stalls at one address that make the mark: the flow and its repeat
	maxFreezeNets   = 64 // networks kept
	maxFreezeAddrs  = 64 // addresses kept per network
	frozenRowRuleID = "frozen-foreign"
)

// DecideFrozen is Decide for a network that may be frozen. A frozen network towards a foreign or
// neighbouring destination gets the row of the own country's mobile network: at most ProbeBytes on
// TCP, no UDP, the first hop an entry node at home or in a neighbouring country. A domestic
// destination is not affected, and a Russian mobile network already has that row.
func (r *Rules) DecideFrozen(own string, net NetClass, dest DestClass, frozen bool) Decision {
	if !frozen || dest == DestDomestic || (own == "RU" && net.Effective() == NetMobile) {
		return r.Decide(own, net, dest)
	}
	d := Decision{RuleID: frozenRowRuleID, Direct: false, TCPLimit: ProbeBytes, Probe: ProbeBytes,
		EntryInCountry: true, EntryNeighbor: true}
	r.overlay(&d, own, DestForeign)
	return d
}

type stallRecord struct {
	n     int
	first time.Time
}

type dataRecord struct {
	flow uint64
	at   time.Time
}

type netFreeze struct {
	stalls      map[netip.Addr]stallRecord
	data        map[netip.Addr]dataRecord // the last data heard from an address, and on which flow
	frozenUntil time.Time
}

// FreezeTracker keeps, per network identifier, the stalls it has seen and the networks it has
// marked frozen. The clock is injected. It is safe for concurrent use and bounded: at most
// 64 networks of 64 addresses.
type FreezeTracker struct {
	now   func() time.Time
	mu    sync.Mutex
	nets  map[string]*netFreeze
	order []string // networks, oldest first
}

// NewFreezeTracker returns a tracker on the given clock; nil means time.Now.
func NewFreezeTracker(now func() time.Time) *FreezeTracker {
	if now == nil {
		now = time.Now
	}
	return &FreezeTracker{now: now, nets: map[string]*netFreeze{}}
}

// net returns the state of a network, creating it and dropping the oldest one past the bound.
// The caller holds the lock.
func (t *FreezeTracker) net(netID string) *netFreeze {
	n := t.nets[netID]
	if n == nil {
		if len(t.order) >= maxFreezeNets {
			delete(t.nets, t.order[0])
			t.order = t.order[1:]
		}
		n = &netFreeze{stalls: map[netip.Addr]stallRecord{}, data: map[netip.Addr]dataRecord{}}
		t.nets[netID] = n
		t.order = append(t.order, netID)
	}
	return n
}

// NoteStall records that a flow to dst (a foreign address) on network netID stopped after bytes
// inbound bytes, with no flow identity: any data heard from dst excuses it. A stall outside
// 12-20 KiB is a slow server and does not count. It returns true when this stall is the one that
// marks the network frozen, for FreezeTTL.
func (t *FreezeTracker) NoteStall(netID string, dst netip.Addr, bytes int) bool {
	return t.NoteStallFlow(netID, dst, bytes, 0, time.Time{}, FreezeTTL)
}

// NoteData records that flow of the network heard data from dst now.
func (t *FreezeTracker) NoteData(netID string, dst netip.Addr, flow uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.net(netID)
	if _, ok := n.data[dst]; !ok && len(n.data) >= maxFreezeAddrs {
		for a := range n.data { // a bounded map: drop an arbitrary old record
			delete(n.data, a)
			break
		}
	}
	n.data[dst] = dataRecord{flow: flow, at: t.now()}
}

// NoteStallFlow is NoteStall for a flow that waited for an answer since waitingSince. A stall is
// not the freeze when another flow of the same network heard data from dst after the wait began
// (and within FreezeAnswerWindow): the server is alive, it is that one flow it will not answer.
// The mark lasts ttl (FreezeTTL when ttl is not positive).
func (t *FreezeTracker) NoteStallFlow(netID string, dst netip.Addr, bytes int, flow uint64, waitingSince time.Time, ttl time.Duration) bool {
	if bytes < FreezeMin || bytes > FreezeMax {
		return false
	}
	if ttl <= 0 {
		ttl = FreezeTTL
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	n := t.net(netID)
	if d, ok := n.data[dst]; ok && d.flow != flow && !d.at.Before(waitingSince) && now.Sub(d.at) <= FreezeAnswerWindow {
		return false
	}
	if n.frozenUntil.After(now) {
		return false // already marked
	}
	if !n.frozenUntil.IsZero() {
		n.frozenUntil = time.Time{} // the mark expired: the count starts again
		clear(n.stalls)
	}
	rec := n.stalls[dst]
	if rec.n > 0 && now.Sub(rec.first) > FreezeTTL {
		rec = stallRecord{}
	}
	if rec.n == 0 {
		rec.first = now
		if len(n.stalls) >= maxFreezeAddrs {
			for a := range n.stalls { // a bounded map: drop an arbitrary old record
				delete(n.stalls, a)
				break
			}
		}
	}
	rec.n++
	if rec.n >= freezeRepeats {
		n.frozenUntil = now.Add(ttl)
		clear(n.stalls)
		return true
	}
	n.stalls[dst] = rec
	return false
}

// Until is when the mark of the network ends; zero when it is not marked.
func (t *FreezeTracker) Until(netID string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := t.nets[netID]; n != nil && n.frozenUntil.After(t.now()) {
		return n.frozenUntil
	}
	return time.Time{}
}

// Frozen reports whether the network is marked frozen now.
func (t *FreezeTracker) Frozen(netID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.nets[netID]
	return n != nil && n.frozenUntil.After(t.now())
}

// Forget drops what is known about a network (it was left).
func (t *FreezeTracker) Forget(netID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.nets[netID]; !ok {
		return
	}
	delete(t.nets, netID)
	for i, id := range t.order {
		if id == netID {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

// size is how many (network, address) records the tracker holds.
func (t *FreezeTracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, nf := range t.nets {
		n += len(nf.stalls) + len(nf.data) + 1
	}
	return n
}
