package senderkeys

import (
	"sort"
	"time"
)

// chain is where one sender's chain stands: the key for message N, and the keys of earlier
// messages that have not arrived yet.
type chain struct {
	Key     []byte            `json:"k"`
	N       uint32            `json:"n"`
	Skipped map[uint32][]byte `json:"sk,omitempty"`
}

func (c *chain) clone() *chain {
	out := &chain{Key: append([]byte(nil), c.Key...), N: c.N}
	if len(c.Skipped) > 0 {
		out.Skipped = make(map[uint32][]byte, len(c.Skipped))
		for n, k := range c.Skipped {
			out.Skipped[n] = append([]byte(nil), k...) // a copy: a failed open wipes what it used
		}
	}
	return out
}

// Keyring is what one member holds for one group: the epochs it knows, its own sending chain in
// each, and where every sender's chain stands. It is plain data, saved as JSON by the caller
// after every change (before an envelope it sealed leaves the node).
type Keyring struct {
	Group  string            `json:"g"`
	Self   string            `json:"self"`
	Epochs map[string]*Epoch `json:"e"`
	Send   map[string]*chain `json:"tx"` // epoch -> our chain
	Recv   map[string]*chain `json:"rx"` // epoch + "|" + sender -> their chain
}

// NewKeyring starts an empty keyring for self in group.
func NewKeyring(group, self string) *Keyring {
	return &Keyring{Group: group, Self: self, Epochs: map[string]*Epoch{}, Send: map[string]*chain{}, Recv: map[string]*chain{}}
}

func (k *Keyring) init() {
	if k.Epochs == nil {
		k.Epochs = map[string]*Epoch{}
	}
	if k.Send == nil {
		k.Send = map[string]*chain{}
	}
	if k.Recv == nil {
		k.Recv = map[string]*chain{}
	}
}

// Add keeps an epoch (already verified by the caller) and reports whether it was new.
func (k *Keyring) Add(e *Epoch) bool {
	k.init()
	if e == nil || e.Group != k.Group {
		return false
	}
	id := e.ID().String()
	if _, ok := k.Epochs[id]; ok {
		return false
	}
	k.Epochs[id] = e
	return true
}

// Epoch returns a known epoch.
func (k *Keyring) Epoch(id EpochID) *Epoch { return k.Epochs[id.String()] }

// Current is the epoch to send in: the newest known (see EpochID.After), nil when none.
func (k *Keyring) Current() *Epoch {
	var best *Epoch
	for _, e := range k.Epochs {
		if best == nil || e.ID().After(best.ID()) {
			best = e
		}
	}
	return best
}

// List returns the known epochs, newest first.
func (k *Keyring) List() []*Epoch {
	out := make([]*Epoch, 0, len(k.Epochs))
	for _, e := range k.Epochs {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID().After(out[j].ID()) })
	return out
}

// Seal encrypts plain in the current epoch with the next key of our chain.
func (k *Keyring) Seal(plain []byte) (*Envelope, error) {
	k.init()
	e := k.Current()
	if e == nil {
		return nil, ErrUnknownEpoch
	}
	id := e.ID()
	c := k.Send[id.String()]
	if c == nil {
		c = &chain{Key: chainStart(e, k.Self)}
	}
	mk, next := step(c.Key)
	defer wipe(mk)
	v := &Envelope{Group: k.Group, Epoch: id.N, Tag: id.Tag, Sender: k.Self, N: c.N}
	if err := seal(mk, v, plain); err != nil {
		return nil, err
	}
	wipe(c.Key)
	c.Key, c.N = next, c.N+1
	k.Send[id.String()] = c
	return v, nil
}

// Open decrypts an envelope. The sender's chain moves on only when the envelope opens, so junk
// cannot push a chain past the real messages; keys of skipped messages are kept for when they
// come (up to MaxSkip per chain) and forgotten once used.
func (k *Keyring) Open(v *Envelope) ([]byte, error) {
	k.init()
	if v == nil || v.Group != k.Group || v.Sender == "" {
		return nil, ErrBadEnvelope
	}
	e := k.Epoch(v.EpochID())
	if e == nil {
		return nil, ErrUnknownEpoch
	}
	key := v.EpochID().String() + "|" + v.Sender
	cur := k.Recv[key]
	if cur == nil {
		cur = &chain{Key: chainStart(e, v.Sender)}
	}
	c := cur.clone()
	var mk []byte
	switch {
	case v.N < c.N:
		mk = c.Skipped[v.N]
		if mk == nil {
			return nil, ErrOldMessage
		}
		delete(c.Skipped, v.N)
	case v.N-c.N > MaxSkip:
		return nil, ErrTooFarAhead
	default:
		for c.N < v.N {
			skipped, next := step(c.Key)
			if c.Skipped == nil {
				c.Skipped = map[uint32][]byte{}
			}
			c.Skipped[c.N] = skipped
			c.Key, c.N = next, c.N+1
		}
		var next []byte
		mk, next = step(c.Key)
		c.Key, c.N = next, c.N+1
	}
	plain, err := open(mk, v)
	wipe(mk)
	if err != nil {
		return nil, err
	}
	trimSkipped(c)
	k.Recv[key] = c
	return plain, nil
}

// trimSkipped forgets the oldest skipped keys past MaxSkip.
func trimSkipped(c *chain) {
	if len(c.Skipped) <= MaxSkip {
		return
	}
	ns := make([]uint32, 0, len(c.Skipped))
	for n := range c.Skipped {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i] < ns[j] })
	for _, n := range ns[:len(ns)-MaxSkip] {
		delete(c.Skipped, n)
	}
}

// ChainState is where we stand with one sender in one epoch: the next message we expect and the
// earlier ones we are still waiting for. A member asking others for what it missed sends these.
type ChainState struct {
	Epoch   EpochID  `json:"e"`
	Sender  string   `json:"s"`
	Next    uint32   `json:"n"`
	Missing []uint32 `json:"m,omitempty"`
}

// Have lists the chains we hold, for a request of what we missed.
func (k *Keyring) Have() []ChainState {
	out := []ChainState{}
	for key, c := range k.Recv {
		i := len(key) - 1
		for i >= 0 && key[i] != '|' {
			i--
		}
		if i < 0 {
			continue
		}
		var id EpochID
		for _, e := range k.Epochs {
			if e.ID().String() == key[:i] {
				id = e.ID()
			}
		}
		if id.N == 0 {
			continue
		}
		st := ChainState{Epoch: id, Sender: key[i+1:], Next: c.N}
		for n := range c.Skipped {
			st.Missing = append(st.Missing, n)
		}
		sort.Slice(st.Missing, func(a, b int) bool { return st.Missing[a] < st.Missing[b] })
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Epoch != out[j].Epoch {
			return out[i].Epoch.After(out[j].Epoch)
		}
		return out[i].Sender < out[j].Sender
	})
	return out
}

// Wants tells, from a peer's Have list, whether it lacks message n of sender in epoch: it holds
// that epoch and either has not reached n yet or is still waiting for it.
func Wants(have []ChainState, epochs []EpochID, epoch EpochID, sender string, n uint32) bool {
	held := false
	for _, e := range epochs {
		if e == epoch {
			held = true
		}
	}
	if !held {
		return false
	}
	for _, st := range have {
		if st.Epoch != epoch || st.Sender != sender {
			continue
		}
		if n >= st.Next {
			return true
		}
		for _, m := range st.Missing {
			if m == n {
				return true
			}
		}
		return false
	}
	return true // a sender it has heard nothing from in that epoch
}

// Prune forgets epochs replaced more than KeepReplacedFor before now (and any past MaxEpochs),
// with their chains. An epoch counts as replaced when the oldest of the newer epochs was made.
// Returns how many epochs went.
func (k *Keyring) Prune(now time.Time) int {
	list := k.List() // newest first
	var drop []*Epoch
	replacedAt := int64(0) // when the next newer epoch was made; the current one is never replaced
	for i, e := range list {
		if i > 0 && (i >= MaxEpochs || now.Sub(time.UnixMilli(replacedAt)) > KeepReplacedFor) {
			drop = append(drop, e)
		}
		if replacedAt == 0 || e.CreatedAt < replacedAt {
			replacedAt = e.CreatedAt
		}
	}
	gone := 0
	for _, e := range drop {
		id := e.ID().String()
		wipe(e.Secret)
		delete(k.Epochs, id)
		if c := k.Send[id]; c != nil {
			wipe(c.Key)
			delete(k.Send, id)
		}
		for key, c := range k.Recv {
			if len(key) > len(id) && key[:len(id)+1] == id+"|" {
				wipe(c.Key)
				delete(k.Recv, key)
			}
		}
		gone++
	}
	return gone
}

// Stale tells whether the current epoch is older than MaxEpochAge at now.
func (k *Keyring) Stale(now time.Time) bool {
	e := k.Current()
	return e != nil && now.Sub(time.UnixMilli(e.CreatedAt)) > MaxEpochAge
}
