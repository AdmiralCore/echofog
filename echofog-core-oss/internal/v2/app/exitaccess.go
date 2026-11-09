package app

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// Limits of the queue of clients waiting for the owner's confirmation, and of the confirmed list.
const (
	ExitPendingMax       = 16
	ExitPendingTTL       = 3600 // seconds, from the last attempt
	ExitPendingPerSource = 2
	exitApprovedMax      = 256
	exitLabelMax         = 64
	exitApprovedPrefix   = "exit/ok/"
)

// ErrExitNoEntry reports an id that is not in the queue or in the confirmed list.
var ErrExitNoEntry = errors.New("app: no such client")

// ErrExitBadEntry reports an id or a label that cannot be one.
var ErrExitBadEntry = errors.New("app: bad client id or label")

// ErrExitFull reports a confirmed list that cannot take more.
var ErrExitFull = errors.New("app: too many confirmed clients")

// ExitPendingView is a client that tried and was turned away; the owner may confirm it.
type ExitPendingView struct {
	ID    string
	Since int64
	Last  int64
	Tries int
}

// ExitApprovedView is a confirmed client.
type ExitApprovedView struct {
	ID    string
	Label string
	Since int64
}

type pendingRec struct {
	id          [32]byte
	source      string
	since, last int64
	tries       int
}

type approvedRec struct {
	Label string `json:"l"`
	Since int64  `json:"s"`
}

// ExitAccess decides who the exit serves. The unit of admission is the identity a client uses for
// this exit; the messenger identity is never seen. Confirmed clients are remembered in the store;
// the queue and the open access live in memory only, so a restart empties both.
type ExitAccess struct {
	clk Clock
	kv  store.KV

	mu       sync.Mutex
	approved map[[32]byte]approvedRec
	pending  []pendingRec
	anyUntil int64
}

// NewExitAccess returns the access rules over a store (nil: in memory).
func NewExitAccess(clk Clock, kv store.KV) *ExitAccess {
	x := &ExitAccess{clk: clk, kv: kv, approved: map[[32]byte]approvedRec{}}
	if kv != nil {
		if es, err := kv.Scan([]byte(exitApprovedPrefix), exitApprovedMax); err == nil {
			for _, e := range es {
				raw, err := hex.DecodeString(string(e.Key[len(exitApprovedPrefix):]))
				var rec approvedRec
				if err != nil || len(raw) != 32 || json.Unmarshal(e.Value, &rec) != nil {
					continue
				}
				x.approved[[32]byte(raw)] = rec
			}
		}
	}
	return x
}

func (x *ExitAccess) now() int64 { return x.clk.Now().Unix() }

// Admit says whether the client is served now. It has no side effects.
func (x *ExitAccess) Admit(node [32]byte) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.approved[node]; ok {
		return true
	}
	return x.anyUntil != 0 && x.now() < x.anyUntil
}

// dropExpiredLocked removes the waiting clients that waited out the lifetime.
func (x *ExitAccess) dropExpiredLocked() {
	cut := x.now() - ExitPendingTTL
	kept := x.pending[:0]
	for _, p := range x.pending {
		if p.last > cut {
			kept = append(kept, p)
		}
	}
	x.pending = kept
}

// Refused records a client that was turned away: it waits for the owner, within the limits. A flood
// does not push earlier clients out; what does not fit is dropped without a trace.
func (x *ExitAccess) Refused(node [32]byte, source string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dropExpiredLocked()
	now := x.now()
	for i := range x.pending {
		if x.pending[i].id == node {
			x.pending[i].last = now
			x.pending[i].tries++
			return
		}
	}
	if _, ok := x.approved[node]; ok {
		return
	}
	if len(x.pending) >= ExitPendingMax {
		return
	}
	same := 0
	for _, p := range x.pending {
		if p.source == source {
			same++
		}
	}
	if same >= ExitPendingPerSource {
		return
	}
	x.pending = append(x.pending, pendingRec{id: node, source: source, since: now, last: now, tries: 1})
}

// Pending lists the waiting clients, oldest first, without those that waited out the lifetime.
func (x *ExitAccess) Pending() []ExitPendingView {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dropExpiredLocked()
	out := make([]ExitPendingView, 0, len(x.pending))
	for _, p := range x.pending {
		out = append(out, ExitPendingView{ID: hex.EncodeToString(p.id[:]), Since: p.since, Last: p.last, Tries: p.tries})
	}
	return out
}

// Approved lists the confirmed clients, oldest first.
func (x *ExitAccess) Approved() []ExitApprovedView {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]ExitApprovedView, 0, len(x.approved))
	for id, r := range x.approved {
		out = append(out, ExitApprovedView{ID: hex.EncodeToString(id[:]), Label: r.Label, Since: r.Since})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func parseExitID(id string) ([32]byte, error) {
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 32 {
		return [32]byte{}, ErrExitBadEntry
	}
	return [32]byte(raw), nil
}

// Approve confirms a waiting client under label.
func (x *ExitAccess) Approve(id, label string) error {
	node, err := parseExitID(id)
	if err != nil {
		return err
	}
	if !utf8.ValidString(label) || utf8.RuneCountInString(label) > exitLabelMax {
		return ErrExitBadEntry
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dropExpiredLocked()
	i := -1
	for j, p := range x.pending {
		if p.id == node {
			i = j
		}
	}
	if i < 0 {
		return ErrExitNoEntry
	}
	if len(x.approved) >= exitApprovedMax {
		return ErrExitFull
	}
	rec := approvedRec{Label: label, Since: x.now()}
	if x.kv != nil {
		raw, _ := json.Marshal(rec)
		if err := x.kv.Put([]byte(exitApprovedPrefix+id), raw); err != nil {
			return fmt.Errorf("app: remember the client: %w", err)
		}
	}
	x.approved[node] = rec
	x.pending = append(x.pending[:i], x.pending[i+1:]...)
	return nil
}

// Reject removes a waiting client.
func (x *ExitAccess) Reject(id string) error {
	node, err := parseExitID(id)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dropExpiredLocked()
	for i, p := range x.pending {
		if p.id == node {
			x.pending = append(x.pending[:i], x.pending[i+1:]...)
			return nil
		}
	}
	return ErrExitNoEntry
}

// Revoke forgets a confirmed client.
func (x *ExitAccess) Revoke(id string) error {
	node, err := parseExitID(id)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.approved[node]; !ok {
		return ErrExitNoEntry
	}
	if x.kv != nil {
		if err := x.kv.Delete([]byte(exitApprovedPrefix + id)); err != nil {
			return fmt.Errorf("app: forget the client: %w", err)
		}
	}
	delete(x.approved, node)
	return nil
}

// AllowAny opens the exit to anyone for ttl seconds and returns the end (unix seconds); a ttl that is
// not positive opens nothing.
func (x *ExitAccess) AllowAny(ttl int64) int64 {
	if ttl <= 0 {
		return 0
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.anyUntil = x.now() + ttl
	return x.anyUntil
}

// RevokeAny closes the open access.
func (x *ExitAccess) RevokeAny() {
	x.mu.Lock()
	x.anyUntil = 0
	x.mu.Unlock()
}

// AnyUntil is the end of the open access, 0 when it is closed or over.
func (x *ExitAccess) AnyUntil() int64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.anyUntil != 0 && x.now() >= x.anyUntil {
		x.anyUntil = 0
	}
	return x.anyUntil
}
