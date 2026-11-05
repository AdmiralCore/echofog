package relay

import (
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Candidate is a node A could use as R. Everything in it is A's own knowledge: its table of live
// sessions and the cards in its PEX store. Nothing here is taken from a frame of R or B.
type Candidate struct {
	Node   [32]byte // R's node ID (keys the circuit binding)
	Device [32]byte // R's device ID (how B's hints and cards name it)
	Flags  uint32   // card flags (registries.md §11)
	// Connected: A holds a live session with this node. A circuit needs one: CONNECT travels on it.
	Connected bool
}

// Choose returns the relays A should try, in order (T6.1 step 4).
//
// A candidate qualifies only with the relay card flag and a live session with A. A and B must
// share R, so relays B is known to use (common: device IDs from B's card side: its mailbox hints
// and the relay cards B handed out, plus relays seen in PEX answers that name B) go first; the
// other connected relay nodes follow, since B is often connected to a relay A knows of (PEX
// spreads relay cards). Within a tier the order is shuffled with rnd (rnd(n) in [0, n)) so load is
// spread; a nil rnd keeps the input order. exclude drops nodes (a relay that just failed).
//
// The result is a list, not one pick: a refusal is uniform (services.md §6.3) and tells A nothing
// about which relay B uses, so A tries the next one.
func Choose(cands []Candidate, common, exclude map[[32]byte]struct{}, rnd func(n int) int) []Candidate {
	var first, rest []Candidate
	for _, c := range cands {
		if c.Flags&objects.FlagRelay == 0 || !c.Connected {
			continue
		}
		if _, no := exclude[c.Node]; no {
			continue
		}
		if _, no := exclude[c.Device]; no {
			continue
		}
		_, byNode := common[c.Node]
		_, byDev := common[c.Device]
		if byNode || byDev {
			first = append(first, c)
		} else {
			rest = append(rest, c)
		}
	}
	shuffle(first, rnd)
	shuffle(rest, rnd)
	return append(first, rest...)
}

func shuffle(s []Candidate, rnd func(n int) int) {
	if rnd == nil {
		return
	}
	for i := len(s) - 1; i > 0; i-- {
		j := rnd(i + 1)
		if j < 0 || j > i {
			j = i
		}
		s[i], s[j] = s[j], s[i]
	}
}
