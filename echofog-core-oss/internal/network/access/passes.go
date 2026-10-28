package access

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Passes are personal invitations of a hidden or isolated node: each has its own secret (so its
// own link), a term, a number of devices and, if the owner wants, a traffic allowance. An owner
// who takes payment their own way (a Telegram bot, a website, cash) makes a pass when somebody
// pays, extends it when they pay again, and revokes it to take somebody off. A device that proves a
// pass's secret is bound to that pass; it may use the node while the pass is active.
//
// The link of a pass is an ordinary invitation link, so every app that can join a hidden node can
// use one.

// MaxPasses bounds the passes of one node: a stranger's handshake is checked against every active
// pass's secret, so the list must stay small enough for that to be cheap.
const MaxPasses = 10000

// maxPassDevices bounds the devices one pass may have.
const maxPassDevices = 100

// Pass errors.
var (
	ErrNoPass      = errors.New("no such pass")
	ErrTooManyPass = errors.New("too many passes: delete old ones first")
	ErrBadPass     = errors.New("devices must be 1 to 100, days and GB not negative, the label at most 100 characters")
)

// Pass is one personal invitation.
type Pass struct {
	ID         string           `json:"id"`
	Label      string           `json:"label,omitempty"` // the owner's note: a name, an order number
	Secret     []byte           `json:"secret"`
	Created    int64            `json:"created"`           // unix seconds
	Expires    int64            `json:"expires,omitempty"` // unix seconds; 0: never
	MaxDevices int              `json:"maxDevices"`
	QuotaBytes int64            `json:"quotaBytes,omitempty"` // traffic allowance through the exit; 0: no limit
	UsedBytes  int64            `json:"usedBytes,omitempty"`
	Revoked    bool             `json:"revoked,omitempty"`
	Devices    map[string]int64 `json:"devices,omitempty"` // peer ID -> unix seconds it joined
}

// Pass states, as State reports them.
const (
	PassActive  = "active"
	PassExpired = "expired"
	PassUsedUp  = "used_up"
	PassRevoked = "revoked"
)

// State is whether the pass lets its devices in now, and if not, why.
func (p *Pass) State(now time.Time) string {
	switch {
	case p.Revoked:
		return PassRevoked
	case p.Expires > 0 && now.Unix() >= p.Expires:
		return PassExpired
	case p.QuotaBytes > 0 && p.UsedBytes >= p.QuotaBytes:
		return PassUsedUp
	}
	return PassActive
}

func (p *Pass) clone() Pass {
	c := *p
	c.Secret = append([]byte(nil), p.Secret...)
	c.Devices = make(map[string]int64, len(p.Devices))
	for k, v := range p.Devices {
		c.Devices[k] = v
	}
	return c
}

// PassSpec is what the owner asks for when making or extending a pass.
type PassSpec struct {
	Label      string
	Days       float64 // term from now (or, when extending, from the current end); 0: never ends
	MaxDevices int     // 0: the default, 2
	QuotaGB    float64 // 0: no traffic limit
}

// Passbook keeps a node's passes.
type Passbook struct {
	mu       sync.RWMutex
	passes   map[string]*Pass
	byPeer   map[types.PeerID]string
	now      func() time.Time
	onChange func(structural bool) // structural: anything but the traffic count changed
}

// NewPassbook returns an empty passbook.
func NewPassbook() *Passbook {
	return &Passbook{passes: map[string]*Pass{}, byPeer: map[types.PeerID]string{}, now: time.Now}
}

// OnChange is called (outside the lock) after every change, so the owner can save the passbook.
func (b *Passbook) OnChange(f func(structural bool)) {
	b.mu.Lock()
	b.onChange = f
	b.mu.Unlock()
}

func (b *Passbook) changed(structural bool) {
	b.mu.RLock()
	f := b.onChange
	b.mu.RUnlock()
	if f != nil {
		f(structural)
	}
}

// Load replaces the passes (read from storage).
func (b *Passbook) Load(list []Pass) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.passes = map[string]*Pass{}
	b.byPeer = map[types.PeerID]string{}
	for i := range list {
		p := list[i].clone()
		if p.ID == "" || len(p.Secret) != SecretLen {
			continue
		}
		b.passes[p.ID] = &p
		for s := range p.Devices {
			if id, err := types.PeerIDFromString(s); err == nil {
				b.byPeer[id] = p.ID
			}
		}
	}
}

// List returns every pass, newest first.
func (b *Passbook) List() []Pass {
	b.mu.RLock()
	out := make([]Pass, 0, len(b.passes))
	for _, p := range b.passes {
		out = append(out, p.clone())
	}
	b.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns one pass.
func (b *Passbook) Get(id string) (Pass, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	p, ok := b.passes[id]
	if !ok {
		return Pass{}, ErrNoPass
	}
	return p.clone(), nil
}

func validSpec(s PassSpec) bool {
	return s.MaxDevices >= 0 && s.MaxDevices <= maxPassDevices && s.Days >= 0 && s.QuotaGB >= 0 &&
		len([]rune(s.Label)) <= 100 && !strings.ContainsAny(s.Label, "\r\n")
}

const gb = int64(1) << 30

// Create makes a pass with a fresh secret.
func (b *Passbook) Create(s PassSpec) (Pass, error) {
	if !validSpec(s) {
		return Pass{}, ErrBadPass
	}
	idb := make([]byte, 6)
	_, _ = rand.Read(idb)
	now := b.now()
	p := &Pass{
		ID: hex.EncodeToString(idb), Label: strings.TrimSpace(s.Label), Secret: NewSecret(),
		Created: now.Unix(), MaxDevices: s.MaxDevices, QuotaBytes: int64(s.QuotaGB * float64(gb)),
		Devices: map[string]int64{},
	}
	if p.MaxDevices == 0 {
		p.MaxDevices = 2
	}
	if s.Days > 0 {
		p.Expires = now.Add(time.Duration(s.Days * float64(24*time.Hour))).Unix()
	}
	b.mu.Lock()
	if len(b.passes) >= MaxPasses {
		b.mu.Unlock()
		return Pass{}, ErrTooManyPass
	}
	b.passes[p.ID] = p
	out := p.clone()
	b.mu.Unlock()
	b.changed(true)
	return out, nil
}

// Extend renews a pass, as when somebody pays again: Days are added to its end (to now when it has
// ended already), QuotaGB to its allowance, and a revoked pass is let in again. With resetUsage the
// traffic count starts from zero (a monthly allowance). MaxDevices and Label change when set.
func (b *Passbook) Extend(id string, s PassSpec, resetUsage bool) (Pass, error) {
	if !validSpec(s) {
		return Pass{}, ErrBadPass
	}
	now := b.now()
	b.mu.Lock()
	p, ok := b.passes[id]
	if !ok {
		b.mu.Unlock()
		return Pass{}, ErrNoPass
	}
	if s.Days > 0 {
		from := now
		if p.Expires > now.Unix() {
			from = time.Unix(p.Expires, 0)
		}
		p.Expires = from.Add(time.Duration(s.Days * float64(24*time.Hour))).Unix()
	}
	if s.QuotaGB > 0 {
		if p.QuotaBytes > 0 && p.UsedBytes > p.QuotaBytes {
			p.UsedBytes = p.QuotaBytes // what was used past the end of the old allowance is not owed
		}
		p.QuotaBytes += int64(s.QuotaGB * float64(gb))
	}
	if resetUsage {
		p.UsedBytes = 0
	}
	if s.MaxDevices > 0 {
		p.MaxDevices = s.MaxDevices
	}
	if s.Label != "" {
		p.Label = strings.TrimSpace(s.Label)
	}
	p.Revoked = false
	out := p.clone()
	b.mu.Unlock()
	b.changed(true)
	return out, nil
}

// Revoke stops a pass letting its devices in; Extend lets them in again.
func (b *Passbook) Revoke(id string) (Pass, error) {
	b.mu.Lock()
	p, ok := b.passes[id]
	if !ok {
		b.mu.Unlock()
		return Pass{}, ErrNoPass
	}
	p.Revoked = true
	out := p.clone()
	b.mu.Unlock()
	b.changed(true)
	return out, nil
}

// Delete forgets a pass and its devices.
func (b *Passbook) Delete(id string) error {
	b.mu.Lock()
	p, ok := b.passes[id]
	if !ok {
		b.mu.Unlock()
		return ErrNoPass
	}
	for s := range p.Devices {
		if pid, err := types.PeerIDFromString(s); err == nil && b.byPeer[pid] == id {
			delete(b.byPeer, pid)
		}
	}
	delete(b.passes, id)
	b.mu.Unlock()
	b.changed(true)
	return nil
}

// RemoveDevice takes one device off a pass; the pass's link lets it (or another) in again while
// there is room.
func (b *Passbook) RemoveDevice(id string, peer types.PeerID) error {
	b.mu.Lock()
	p, ok := b.passes[id]
	if !ok || b.byPeer[peer] != id {
		b.mu.Unlock()
		return ErrNoPass
	}
	delete(p.Devices, peer.String())
	delete(b.byPeer, peer)
	b.mu.Unlock()
	b.changed(true)
	return nil
}

// PassOf is the pass a device is bound to.
func (b *Passbook) PassOf(peer types.PeerID) (Pass, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	p, ok := b.passes[b.byPeer[peer]]
	if !ok {
		return Pass{}, false
	}
	return p.clone(), true
}

// Allows reports whether peer is a device of an active pass.
func (b *Passbook) Allows(peer types.PeerID) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	p, ok := b.passes[b.byPeer[peer]]
	return ok && p.State(b.now()) == PassActive
}

// Bound reports whether peer is a device of any pass, active or not.
func (b *Passbook) Bound(peer types.PeerID) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.byPeer[peer]
	return ok
}

// admit binds peer to the active pass whose secret proves accepts, while the pass has room for
// another device. A device bound to a pass that is not active is not let in through another one.
func (b *Passbook) admit(peer types.PeerID, proves func(secret []byte) bool) bool {
	now := b.now()
	b.mu.Lock()
	if id, ok := b.byPeer[peer]; ok {
		p := b.passes[id]
		ok := p != nil && p.State(now) == PassActive && proves(p.Secret)
		b.mu.Unlock()
		return ok
	}
	var hit *Pass
	for _, p := range b.passes {
		if p.State(now) == PassActive && proves(p.Secret) {
			hit = p
			break
		}
	}
	if hit == nil || len(hit.Devices) >= hit.MaxDevices {
		b.mu.Unlock()
		return false
	}
	hit.Devices[peer.String()] = now.Unix()
	b.byPeer[peer] = hit.ID
	b.mu.Unlock()
	b.changed(true)
	return true
}

// Charge counts n bytes a device used through the exit. It reports false when the device's pass
// has no allowance left (the stream is to be cut); a peer bound to no pass is not counted.
func (b *Passbook) Charge(peer types.PeerID, n int64) bool {
	b.mu.Lock()
	p, ok := b.passes[b.byPeer[peer]]
	if !ok {
		b.mu.Unlock()
		return true
	}
	if p.QuotaBytes > 0 && p.UsedBytes >= p.QuotaBytes {
		b.mu.Unlock()
		return false
	}
	p.UsedBytes += n
	b.mu.Unlock()
	b.changed(false)
	return true
}

// Shut are the devices whose pass does not let them in now (expired, used up or revoked): the
// owner drops their connections.
func (b *Passbook) Shut() []types.PeerID {
	now := b.now()
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []types.PeerID
	for peer, id := range b.byPeer {
		if p := b.passes[id]; p == nil || p.State(now) != PassActive {
			out = append(out, peer)
		}
	}
	return out
}
