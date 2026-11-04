package exit_gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/ratelimit"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// freeBudget caps how much a free exit gives away: per peer and per address in a day (UTC), and
// for the whole exit in a calendar month (UTC). Speed shaping alone let one client with a torrent
// drain an operator's prepaid traffic in days. Every cap is off until the operator sets it, and
// the peers on the free list never count against it.
//
// The month total is saved to disk, so a restart does not hand the month's traffic out again; the
// daily counters live in memory only.
type freeBudget struct {
	mu        sync.Mutex
	day       time.Time
	peers     map[types.PeerID]int64
	ips       map[string]int64
	month     time.Time
	monthUsed int64
	path      string
	dirty     bool
	saved     time.Time
}

// freeBudgetFile is what is kept on disk.
type freeBudgetFile struct {
	Month int64 `json:"month"` // unix seconds of the month's first day
	Used  int64 `json:"used"`
}

// budgetSaveEvery spaces out the writes of the month total; a crash loses at most this much of it.
const budgetSaveEvery = time.Minute

// SetFreeBudgetFile keeps the month total of the free budget in path and loads what is there.
// Without it (as in tests) the total starts at zero on every start.
func (e *ExitService) SetFreeBudgetFile(path string) {
	b := &e.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	b.path = path
	if raw, err := os.ReadFile(path); err == nil {
		var f freeBudgetFile
		if json.Unmarshal(raw, &f) == nil && f.Month > 0 && f.Used > 0 {
			b.month, b.monthUsed = time.Unix(f.Month, 0).UTC(), f.Used
		}
	}
}

// budgetedLocked reports whether the free budget applies to peer: a free exit with a cap set, and a
// peer that is not on the free list. Called with policyMu held.
func (e *ExitService) budgetedLocked(peer types.PeerID, l Limits) bool {
	return e.policy.PricePerMB <= 0 && !e.freePeers[peer] &&
		(l.FreePeerDaily > 0 || l.FreeIPDaily > 0 || l.FreeMonthly > 0)
}

// budgetKey is the address a peer's free traffic counts against ("" when there is none to count:
// no address known, or a loopback or LAN one).
func (e *ExitService) budgetKey(peer types.PeerID) string {
	e.slots.mu.Lock()
	addrOf := e.addrOf
	e.slots.mu.Unlock()
	if addrOf == nil {
		return ""
	}
	ip, _ := ratelimit.AddrKeys(addrOf(peer))
	return ip
}

// rollLocked starts a new day or month when the clock has passed into one. Called with b.mu held.
func (b *freeBudget) rollLocked(now time.Time) {
	now = now.UTC()
	day := now.Truncate(24 * time.Hour)
	if !b.day.Equal(day) {
		b.day = day
		b.peers, b.ips = map[types.PeerID]int64{}, map[string]int64{}
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if !b.month.Equal(month) {
		b.month, b.monthUsed, b.dirty = month, 0, true
	}
}

// leftLocked is how much free traffic peer (from address ip) may still take. Called with b.mu held.
func (b *freeBudget) leftLocked(peer types.PeerID, ip string, l Limits) int64 {
	left := int64(-1)
	cap := func(limit, used int64) {
		if limit <= 0 {
			return
		}
		if v := max(0, limit-used); left < 0 || v < left {
			left = v
		}
	}
	cap(l.FreePeerDaily, b.peers[peer])
	if ip != "" {
		cap(l.FreeIPDaily, b.ips[ip])
	}
	cap(l.FreeMonthly, b.monthUsed)
	return left
}

// takeFree sets aside up to want bytes of free traffic for peer and returns how many it got, and
// the day they count against.
func (e *ExitService) takeFree(peer types.PeerID, ip string, want int64, l Limits) (int64, time.Time) {
	b := &e.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.rollLocked(now)
	got := want
	if left := b.leftLocked(peer, ip, l); left >= 0 {
		got = min(want, left)
	}
	if got <= 0 {
		return 0, b.day
	}
	b.addLocked(peer, ip, got)
	b.saveLocked(now, false)
	return got, b.day
}

// freeLeft is how much free traffic peer may still take today (-1: no cap).
func (e *ExitService) freeLeft(peer types.PeerID, ip string, l Limits) int64 {
	b := &e.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(time.Now())
	return b.leftLocked(peer, ip, l)
}

// giveBackFree returns free traffic a stream set aside on day and did not use.
func (e *ExitService) giveBackFree(peer types.PeerID, ip string, day time.Time, n int64) {
	if n <= 0 {
		return
	}
	b := &e.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.day.Equal(day) {
		return // taken yesterday: today's counters never held it
	}
	b.addLocked(peer, ip, -n)
}

// addLocked moves the counters by n bytes, dropping a counter that reaches zero. Called with b.mu held.
func (b *freeBudget) addLocked(peer types.PeerID, ip string, n int64) {
	if b.peers[peer]+n <= 0 {
		delete(b.peers, peer)
	} else {
		if _, ok := b.peers[peer]; !ok && len(b.peers) >= quotaPeers {
			pruneSmallest(b.peers)
		}
		b.peers[peer] += n
	}
	if ip != "" {
		if b.ips[ip]+n <= 0 {
			delete(b.ips, ip)
		} else {
			if _, ok := b.ips[ip]; !ok && len(b.ips) >= quotaPeers {
				pruneSmallest(b.ips)
			}
			b.ips[ip] += n
		}
	}
	b.monthUsed = max(0, b.monthUsed+n)
	b.dirty = true
}

// pruneSmallest makes room in a counter table by forgetting the half that used least: those get
// their share back, which costs little, while the heavy users stay counted.
func pruneSmallest[K comparable](m map[K]int64) {
	used := make([]int64, 0, len(m))
	for _, v := range m {
		used = append(used, v)
	}
	slices.Sort(used)
	cut := used[len(used)/2]
	for k, v := range m {
		if v <= cut && len(m) > len(used)/2 {
			delete(m, k)
		}
	}
}

// saveLocked writes the month total when it changed, at most every budgetSaveEvery unless forced.
// Called with b.mu held.
func (b *freeBudget) saveLocked(now time.Time, force bool) {
	if b.path == "" || !b.dirty || (!force && now.Sub(b.saved) < budgetSaveEvery) {
		return
	}
	raw, err := json.Marshal(freeBudgetFile{Month: b.month.Unix(), Used: b.monthUsed})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil || os.Rename(tmp, b.path) != nil {
		return
	}
	b.dirty, b.saved = false, now
}

// FlushFreeBudget writes the month total now (on shutdown).
func (e *ExitService) FlushFreeBudget() {
	b := &e.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	b.saveLocked(time.Now(), true)
}

// FreeMonth returns the free traffic given away this month and the monthly cap (0: none).
func (e *ExitService) FreeMonth() (used, limit int64) {
	l := e.CurrentLimits()
	b := &e.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(time.Now())
	return b.monthUsed, l.FreeMonthly
}

// paymentRefusal names why a peer with no allowance left is refused: a free exit has no payment to
// ask for, so it was its free budget.
func (e *ExitService) paymentRefusal() string {
	if e.CurrentPolicy().PricePerMB <= 0 {
		return RefuseFreeLimit
	}
	return RefusePayment
}
