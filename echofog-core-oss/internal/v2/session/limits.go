package session

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Pre-authentication limits (handshake.md §11). They count handshakes in flight, not bytes, and
// they never ban: a source IP may be a CGNAT shared by thousands of users.
const (
	MaxPreAuthPerIP   = 64
	MaxPreAuthPerUnit = 256 // per IPv4 /24 or IPv6 /48
	MaxPreAuthTotal   = 1024
)

// MaxTrackedSources bounds every table of Limits (rates, holds, quotas, abuse scores).
const MaxTrackedSources = 4096

const (
	// unitRateCap is how much one network can add to the node-wide handshake rate that turns the
	// puzzle on (handshake §6 "under load"). It is below the first threshold of the policy, so a
	// flood from one network cannot make everybody else pay; it takes several networks.
	unitRateCap = 16
	// unitRateFree is the arrival rate per second of one /24 (IPv6 /48) below which the network
	// pays nothing for its own rate. From there on the policy table applies to that network alone.
	unitRateFree = 100
	// maxDifficulty is the largest difficulty an initiator accepts (handshake §6).
	maxDifficulty = handshake.MaxDifficulty
)

// Default abuse-ban policy: 20 findings of a known frame type within a minute ban an
// authenticated device ID for an hour. Findings are rare for honest software (versioning §2
// rule 2: unknown is never abuse).
const (
	defaultBanAfter  = 20
	defaultBanWindow = time.Minute
	defaultBanFor    = time.Hour
	secretLifetime   = 24 * time.Hour
)

// LimitsConfig configures Limits. The zero value is usable.
type LimitsConfig struct {
	// Now is the injectable clock (AD13); nil means time.Now.
	Now func() time.Time
	// Rand draws the puzzle secret R; nil means crypto/rand.
	Rand io.Reader
	// Abuse persists the finding counts of authenticated devices (optional).
	Abuse *AbuseLog
	// BanAfter findings within BanWindow ban a device ID for BanFor.
	BanAfter  int
	BanWindow time.Duration
	BanFor    time.Duration
}

// LimitsStats are counters for the operator view.
type LimitsStats struct {
	Admitted uint64
	Refused  uint64
	Retries  uint64
}

// Limits holds the resource limits of one node's session layer: pre-authentication admission and
// puzzle policy (handshake.md §6, §11), the daily puzzle secret, per-session/per-network/per-node
// quotas (services.md §1) and bans for authenticated abuse. It is safe for concurrent use.
type Limits struct {
	cfg LimitsConfig

	mu       sync.Mutex
	ipHeld   map[netip.Addr]int
	unitHeld map[netip.Prefix]int
	total    int
	winStart time.Time
	unitRate map[netip.Prefix]int
	global   int
	stats    LimitsStats
	secret   []byte
	prev     []byte
	secretAt time.Time
	rotated  time.Time

	qmu   sync.Mutex
	quota map[quotaKey]*quotaEnt

	amu    sync.Mutex
	scores map[[32]byte][]time.Time
	bans   map[[32]byte]time.Time
}

// NewLimits returns Limits with a fresh puzzle secret.
func NewLimits(cfg LimitsConfig) (*Limits, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.BanAfter <= 0 {
		cfg.BanAfter = defaultBanAfter
	}
	if cfg.BanWindow <= 0 {
		cfg.BanWindow = defaultBanWindow
	}
	if cfg.BanFor <= 0 {
		cfg.BanFor = defaultBanFor
	}
	l := &Limits{
		cfg:      cfg,
		ipHeld:   map[netip.Addr]int{},
		unitHeld: map[netip.Prefix]int{},
		unitRate: map[netip.Prefix]int{},
		quota:    map[quotaKey]*quotaEnt{},
		scores:   map[[32]byte][]time.Time{},
		bans:     map[[32]byte]time.Time{},
	}
	var err error
	if l.secret, err = l.newSecret(); err != nil {
		return nil, err
	}
	l.secretAt = cfg.Now()
	return l, nil
}

func (l *Limits) newSecret() ([]byte, error) {
	s := make([]byte, 32)
	if _, err := io.ReadFull(l.cfg.Rand, s); err != nil {
		return nil, fmt.Errorf("%w: puzzle secret: %v", ErrConfig, err)
	}
	return s, nil
}

// Stats returns the counters.
func (l *Limits) Stats() LimitsStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

// InFlight is the number of pre-authentication handshakes currently admitted.
func (l *Limits) InFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Tracked is the number of entries in all bounded tables (tests and the operator view).
func (l *Limits) Tracked() int {
	l.mu.Lock()
	n := len(l.ipHeld) + len(l.unitHeld) + len(l.unitRate)
	l.mu.Unlock()
	l.qmu.Lock()
	n += len(l.quota)
	l.qmu.Unlock()
	l.amu.Lock()
	n += len(l.scores) + len(l.bans)
	l.amu.Unlock()
	return n
}

func (l *Limits) noteRetry() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.stats.Retries++
	l.mu.Unlock()
}

// Puzzle returns the current HS_RETRY configuration (handshake §6): R, rotated every 24 hours,
// and the previous R. Difficulty is left nil; Accept sets it from the admission of each carrier.
// The slices are copies.
func (l *Limits) Puzzle() *handshake.PuzzleConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	if now.Sub(l.secretAt) >= secretLifetime {
		if s, err := l.newSecret(); err == nil {
			l.prev, l.secret = l.secret, s
			l.secretAt, l.rotated = now, now
		}
		// On a failed draw the old R stays: a longer life is better than no puzzle.
	}
	p := &handshake.PuzzleConfig{Secret: append([]byte(nil), l.secret...), RotatedAt: l.rotated}
	if l.prev != nil {
		p.PrevSecret = append([]byte(nil), l.prev...)
	}
	return p
}

// ---- pre-authentication admission

// Admission is the verdict on one carrier before any handshake byte is processed. Release it when
// the handshake ends, however it ends.
type Admission struct {
	l        *Limits
	ip       netip.Addr
	unit     netip.Prefix
	refused  bool
	exceeded int
	diff     uint8
	mu       sync.Mutex
	done     bool
}

// Refused reports that a hidden node must not serve this carrier: it waits and closes silently
// (handshake §11). A public node is never refused; it raises the puzzle instead.
func (a *Admission) Refused() bool { return a.refused }

// Exceeded is the number of per-source limits (IP, network, node) this carrier is over.
func (a *Admission) Exceeded() int { return a.exceeded }

// Difficulty is the puzzle difficulty to demand of this carrier, 0 for none, at most 24.
func (a *Admission) Difficulty() uint8 { return a.diff }

// Release returns the slot. It is idempotent.
func (a *Admission) Release() {
	a.mu.Lock()
	if a.done || a.refused {
		a.mu.Unlock()
		return
	}
	a.done = true
	a.mu.Unlock()
	l := a.l
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if n := l.ipHeld[a.ip] - 1; n > 0 {
		l.ipHeld[a.ip] = n
	} else {
		delete(l.ipHeld, a.ip)
	}
	if n := l.unitHeld[a.unit] - 1; n > 0 {
		l.unitHeld[a.unit] = n
	} else {
		delete(l.unitHeld, a.unit)
	}
}

// preAuthUnit is the network a carrier belongs to for the limits of handshake §11: IPv4 /24,
// IPv6 /48.
func preAuthUnit(a netip.Addr) netip.Prefix {
	if !a.IsValid() {
		return netip.Prefix{}
	}
	bits := 24
	if a.Is6() {
		bits = 48
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// Admit counts a new carrier from src. An invalid src (a carrier without an address) is one
// shared network.
func (l *Limits) Admit(src netip.Addr, hidden bool) *Admission {
	src = src.Unmap()
	a := &Admission{l: l, ip: src, unit: preAuthUnit(src)}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()

	if l.winStart.IsZero() || now.Sub(l.winStart) >= time.Second || now.Before(l.winStart) {
		l.winStart = now
		clear(l.unitRate)
		l.global = 0
	}
	n, tracked := l.unitRate[a.unit]
	switch {
	case !tracked && len(l.unitRate) >= MaxTrackedSources:
		n = 1 // an untracked network cannot be capped: it counts in full
		l.global++
	default:
		n++
		l.unitRate[a.unit] = n
		if n <= unitRateCap {
			l.global++
		}
	}

	if l.ipHeld[a.ip] >= MaxPreAuthPerIP {
		a.exceeded++
	}
	if l.unitHeld[a.unit] >= MaxPreAuthPerUnit {
		a.exceeded++
	}
	if l.total >= MaxPreAuthTotal {
		a.exceeded++
	}

	d := rateDifficulty(l.global)
	if n >= unitRateFree {
		d = max(d, rateDifficulty(n))
	}
	d += uint8(2 * a.exceeded)
	a.diff = min(d, maxDifficulty)

	if hidden && a.exceeded > 0 {
		a.refused = true
		l.stats.Refused++
		return a
	}
	l.stats.Admitted++
	l.total++
	l.ipHeld[a.ip]++
	l.unitHeld[a.unit]++
	return a
}

// rateDifficulty is the puzzle policy of v1 (handshake §6) as a function of handshakes per
// second: 0 below 25, 16 below 100, 20 below 400, else 22.
func rateDifficulty(perSecond int) uint8 {
	switch {
	case perSecond < 25:
		return 0
	case perSecond < 100:
		return 16
	case perSecond < 400:
		return 20
	}
	return 22
}

// ---- authenticated abuse

// AbuseHandler returns the mux.Config.OnAbuse hook of a session whose peer authenticated as
// device peer. Only a malformed frame of a known type reaches it (versioning §2 rule 2), and only
// an authenticated device ID is ever banned: the zero ID stands for "not authenticated".
func (l *Limits) AbuseHandler(peer [32]byte) func(mux.Abuse) {
	if peer == ([32]byte{}) {
		return func(mux.Abuse) {}
	}
	return func(mux.Abuse) { l.reportAbuse(peer) }
}

func (l *Limits) reportAbuse(peer [32]byte) {
	_ = l.cfg.Abuse.Report(peer)
	now := l.cfg.Now()
	l.amu.Lock()
	defer l.amu.Unlock()
	if _, ok := l.scores[peer]; !ok && len(l.scores)+len(l.bans) >= MaxTrackedSources {
		l.evictAbuseLocked(now)
	}
	kept := l.scores[peer][:0]
	for _, t := range l.scores[peer] {
		if now.Sub(t) < l.cfg.BanWindow {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	if len(kept) >= l.cfg.BanAfter {
		l.bans[peer] = now.Add(l.cfg.BanFor)
		delete(l.scores, peer)
		return
	}
	l.scores[peer] = kept
}

// evictAbuseLocked frees room: expired bans and stale scores first, then an arbitrary score.
func (l *Limits) evictAbuseLocked(now time.Time) {
	for p, until := range l.bans {
		if !now.Before(until) {
			delete(l.bans, p)
		}
	}
	for p, ts := range l.scores {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= l.cfg.BanWindow {
			delete(l.scores, p)
		}
	}
	for p := range l.scores {
		if len(l.scores)+len(l.bans) < MaxTrackedSources {
			break
		}
		delete(l.scores, p)
	}
}

// Banned reports whether the authenticated device is banned now.
func (l *Limits) Banned(peer [32]byte) bool {
	if l == nil || peer == ([32]byte{}) {
		return false
	}
	now := l.cfg.Now()
	l.amu.Lock()
	defer l.amu.Unlock()
	until, ok := l.bans[peer]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(l.bans, peer)
		return false
	}
	return true
}

// ---- quotas (services.md §1)

// Rule is one quota: PerSession events per Window for each session, three times that per network
// diversity unit (IPv4 /24, IPv6 /32), and, when PerNodeDay is set, PerNodeDay events per
// authenticated node ID per 24 hours, whatever sessions carry them.
type Rule struct {
	Name       string
	PerSession int
	Window     time.Duration
	PerNodeDay int
}

type quotaKey struct {
	name string
	kind byte // 's' session, 'u' unit, 'n' node
	sid  [16]byte
	unit netip.Prefix
	node [32]byte
}

type quotaEnt struct {
	n     int
	until time.Time
}

// diversityUnit is the unit of dht.md §1 for quotas: IPv4 /24, IPv6 /32.
func diversityUnit(a netip.Addr) netip.Prefix {
	a = a.Unmap()
	if !a.IsValid() {
		return netip.Prefix{}
	}
	bits := 24
	if a.Is6() {
		bits = 32
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// Allow counts one event under rule for the session sid, whose peer is at src and authenticated
// as node. It reports whether the event is within every limit; a refused event counts nowhere.
func (l *Limits) Allow(rule Rule, sid [16]byte, src netip.Addr, node [32]byte) bool {
	if rule.PerSession <= 0 || rule.Window <= 0 {
		return false
	}
	now := l.cfg.Now()
	l.qmu.Lock()
	defer l.qmu.Unlock()

	type need struct {
		key   quotaKey
		limit int
		span  time.Duration
	}
	needs := []need{{quotaKey{name: rule.Name, kind: 's', sid: sid}, rule.PerSession, rule.Window}}
	if u := diversityUnit(src); u.IsValid() {
		needs = append(needs, need{quotaKey{name: rule.Name, kind: 'u', unit: u}, 3 * rule.PerSession, rule.Window})
	}
	if rule.PerNodeDay > 0 && node != ([32]byte{}) {
		needs = append(needs, need{quotaKey{name: rule.Name, kind: 'n', node: node}, rule.PerNodeDay, 24 * time.Hour})
	}

	fresh := 0
	ents := make([]*quotaEnt, len(needs))
	for i, nd := range needs {
		e := l.quota[nd.key]
		if e != nil && !now.Before(e.until) {
			delete(l.quota, nd.key)
			e = nil
		}
		if e == nil {
			fresh++
		} else if e.n >= nd.limit {
			return false
		}
		ents[i] = e
	}
	if fresh > 0 && len(l.quota)+fresh > MaxTrackedSources {
		l.purgeQuotaLocked(now)
		if len(l.quota)+fresh > MaxTrackedSources {
			return false // full of live entries: fail closed
		}
	}
	for i, nd := range needs {
		if ents[i] == nil {
			ents[i] = &quotaEnt{until: now.Add(nd.span)}
			l.quota[nd.key] = ents[i]
		}
		ents[i].n++
	}
	return true
}

func (l *Limits) purgeQuotaLocked(now time.Time) {
	for k, e := range l.quota {
		if !now.Before(e.until) {
			delete(l.quota, k)
		}
	}
}

// PeerPolicy is the part of objects.ReleaseState that decides whether a peer version is refused.
type PeerPolicy interface {
	PeerRefused(component string, own, peer uint32, now time.Time) bool
}

// ReleaseAdmit adapts a release policy to AcceptConfig.AdmitVersion for component (normally
// "proto") and the node's own version. The "never lock out" rule lives in PeerRefused: a manifest
// refuses only peers strictly below the node's own version.
func ReleaseAdmit(p PeerPolicy, component string, own uint32, now func() time.Time) func(uint32) bool {
	if now == nil {
		now = time.Now
	}
	return func(peer uint32) bool { return !p.PeerRefused(component, own, peer, now()) }
}
