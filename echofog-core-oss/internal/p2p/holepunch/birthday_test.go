package holepunch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func symNAT(lo, hi uint16, ports int) func() NATInfo {
	return func() NATInfo { return NATInfo{Sym: true, Lo: lo, Hi: hi, Ports: ports} }
}

func coneNAT() func() NATInfo {
	return func() NATInfo { return NATInfo{} }
}

func unknownNAT() func() NATInfo {
	return func() NATInfo { return NATInfo{Unknown: true} }
}

func syncMsg(log []message) (message, bool) {
	for _, m := range log {
		if m.T == "sync" {
			return m, true
		}
	}
	return message{}, false
}

// A symmetric NAT with a narrow port block reaches a cone peer with a birthday punch, where the
// ordinary punch (which this pair never runs) would have sent them both to a relay.
func TestSymmetricToConeUsesBirthday(t *testing.T) {
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = symNAT(40000, 44000, 4)
		b.OwnNAT = coneNAT()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, p.a.Connect(ctx, idB))

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.True(t, p.net.connected[idA] && p.net.connected[idB], "a direct link came up")
	assert.Equal(t, 1, p.net.bdDialN, "the symmetric side (initiator) opened the sockets")
	assert.Equal(t, 1, p.net.bdSprayN, "the cone side sprayed")
	assert.Zero(t, p.net.punchHits, "no ordinary punch was tried")
	assert.Empty(t, p.net.dials, "and no ordinary dial")
	assert.Equal(t, maxBirthdaySockets, p.net.bdSockets, "the symmetric side opens the capped number of sockets")
	assert.LessOrEqual(t, p.net.bdSpray, maxBirthdaySpray)
	assert.Equal(t, uint16(40000), p.net.bdLo, "the cone sprays the symmetric side's range")
	assert.Equal(t, uint16(44000), p.net.bdHi)

	sync, ok := syncMsg(p.log)
	require.True(t, ok)
	assert.True(t, sync.BD, "SYNC says birthday")
	assert.Equal(t, roleSpray, sync.BR, "SYNC names the role the cone side must run")
	assert.Equal(t, uint16(maxBirthdaySockets), sync.NS, "and how many sockets")
}

// The mirror: a cone initiator reaches a symmetric peer with a narrow range by spraying it.
func TestConeToSymmetricUsesBirthday(t *testing.T) {
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = coneNAT()
		b.OwnNAT = symNAT(50000, 52000, 3)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, p.a.Connect(ctx, idB))

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.True(t, p.net.connected[idA] && p.net.connected[idB])
	assert.Equal(t, 1, p.net.bdSprayN, "the cone side (initiator) sprayed")
	assert.Equal(t, 1, p.net.bdDialN, "the symmetric side (responder) opened the sockets")
	assert.Zero(t, p.net.punchHits)
	assert.Equal(t, uint16(50000), p.net.bdLo, "the symmetric side's range, learned from its ANSWER")
	assert.Equal(t, uint16(52000), p.net.bdHi)

	sync, ok := syncMsg(p.log)
	require.True(t, ok)
	assert.Equal(t, roleDial, sync.BR, "the symmetric side is told to open the sockets")
}

// Two cone NATs take the ordinary DCUtR punch, never a birthday one.
func TestConeToConeStaysOrdinary(t *testing.T) {
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = coneNAT()
		b.OwnNAT = coneNAT()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, p.a.Connect(ctx, idB))

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.True(t, p.net.connected[idA])
	assert.Zero(t, p.net.bdDialN, "no birthday dial")
	assert.Zero(t, p.net.bdSprayN, "no birthday spray")
	assert.Equal(t, 1, p.net.punchHits, "the ordinary punch ran")
}

// A symmetric NAT with a wide port range still tries a birthday punch toward a cone peer; the cone
// then sprays the whole dynamic range (signalled by a zeroed range to the transport).
func TestWideSymmetricToConeStillTriesBirthday(t *testing.T) {
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = coneNAT()
		b.OwnNAT = symNAT(1100, 60000, 12) // span far wider than a carrier block
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, p.a.Connect(ctx, idB))

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.True(t, p.net.connected[idA])
	assert.Equal(t, 1, p.net.bdSprayN, "the cone side (initiator) sprayed")
	assert.Equal(t, uint16(0), p.net.bdLo, "a wide range is zeroed: spray the whole dynamic range")
	assert.Equal(t, uint16(0), p.net.bdHi)
}

// Two symmetric NATs both with a wide range have no birthday path: a relay carries them, and not a
// single spray packet goes out.
func TestBothSymmetricWideFallsBackToRelay(t *testing.T) {
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = symNAT(1100, 60000, 12)
		b.OwnNAT = symNAT(1200, 61000, 10)
		a.OwnID, b.OwnID = idA, idB
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.a.Connect(ctx, idB)
	assert.ErrorIs(t, err, ErrNoPunchPath)

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.False(t, p.net.connected[idA], "no direct link")
	assert.Zero(t, p.net.bdMutualN, "no mutual birthday toward wide ranges")
	assert.Zero(t, p.net.bdDialN)
	assert.Zero(t, p.net.bdSprayN)
	_, hasSync := syncMsg(p.log)
	assert.False(t, hasSync, "no SYNC: the peer is never told to start")
}

// Two symmetric NATs both with a narrow range run a mutual birthday: the lower peer ID dials, the
// higher listens, decided the same way on both sides.
func TestBothSymmetricNarrowUsesMutual(t *testing.T) {
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = symNAT(40000, 44000, 4)
		b.OwnNAT = symNAT(50000, 53000, 5)
		a.OwnID, b.OwnID = idA, idB // idA (0x0A) < idB (0x0B): A dials, B listens
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, p.a.Connect(ctx, idB))

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.True(t, p.net.connected[idA] && p.net.connected[idB])
	assert.Equal(t, 2, p.net.bdMutualN, "both sides ran the mutual birthday")
	assert.True(t, p.net.bdMutualDialer, "one side dialed")
	assert.True(t, p.net.bdMutualListen, "the other listened")
	assert.Zero(t, p.net.punchHits)
	assert.LessOrEqual(t, p.net.bdMutualSockets, maxBirthdayMutualSockets)

	sync, ok := syncMsg(p.log)
	require.True(t, ok)
	assert.Equal(t, roleMutual, sync.BR, "both sides are told to run the mutual birthday")
}

// The initiator's role is authoritative. The responder must follow the role in SYNC even if its own
// NAT picture changed after it sent ANSWER; recomputing there made the two ends run the wrong halves
// of the punch (for example both spraying, with nobody opening sockets).
func TestSyncBirthdayRoleIsAuthoritative(t *testing.T) {
	var bBecameSymmetric atomic.Bool
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = symNAT(40000, 44000, 4)
		b.OwnNAT = func() NATInfo {
			if bBecameSymmetric.Load() {
				return NATInfo{Sym: true, Lo: 50000, Hi: 53000, Ports: 4}
			}
			return NATInfo{}
		}
		// flip the responder's classification after it answered, just before it sees SYNC
		a.Signal.(*link).drop = func(m message) bool {
			if m.T == "sync" {
				bBecameSymmetric.Store(true)
			}
			return false
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, p.a.Connect(ctx, idB))

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.True(t, p.net.connected[idB])
	assert.Equal(t, 1, p.net.bdSprayN, "the responder followed the role (spray) instead of recomputing")
	assert.Zero(t, p.net.bdMutualN, "a recomputed mutual birthday would have left both sides opening sockets")
}

// Birthday punching is IPv4-only. A symmetric peer with a global IPv6 address has a direct path the
// ordinary punch can open (IPv6 has no NAT), so the pair must fall back to it instead of failing.
func TestBirthdayFallsBackToOrdinaryOnIPv6Only(t *testing.T) {
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = symNAT(40000, 44000, 4)
		b.OwnNAT = coneNAT()
		a.OwnAddrs = func() []string { return []string{"[2001:db8::1]:4001"} }
		b.OwnAddrs = func() []string { return []string{"[2001:db8::2]:4002"} }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, p.a.Connect(ctx, idB))

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.True(t, p.net.connected[idA] && p.net.connected[idB])
	assert.Zero(t, p.net.bdDialN, "no birthday sockets over IPv6")
	assert.Zero(t, p.net.bdSprayN, "no birthday spray over IPv6")
	assert.Equal(t, 1, p.net.punchHits, "the ordinary punch ran over IPv6")
	assert.Equal(t, []string{"[2001:db8::1]:4001"}, p.net.dials)
}

// chooseMode is the whole decision matrix in one place, including an older peer that sends no NAT
// field at all (peerNT == "").
func TestChooseModeMatrix(t *testing.T) {
	s := New(Config{})
	cone := NATInfo{}
	unknown := NATInfo{Unknown: true}
	symNarrow := NATInfo{Sym: true, Lo: 40000, Hi: 44000, Ports: 4}
	symWide := NATInfo{Sym: true, Lo: 1100, Hi: 60000, Ports: 9}

	assert.Equal(t, ntCone, cone.nt())
	assert.Equal(t, ntSym, symNarrow.nt())
	assert.Equal(t, ntUnknown, unknown.nt())

	cases := []struct {
		name           string
		own            NATInfo
		peerNT         string
		peerLo, peerHi uint16
		peerPorts      int
		want           punchMode
	}{
		{"cone/cone", cone, ntCone, 0, 0, 0, modeOrdinary},
		{"cone/old-peer", cone, "", 0, 0, 0, modeOrdinary},
		{"cone/unknown", cone, ntUnknown, 0, 0, 0, modeOrdinary},
		{"unknown/cone", unknown, ntCone, 0, 0, 0, modeOrdinary},
		{"unknown/old-peer", unknown, "", 0, 0, 0, modeOrdinary},
		{"unknown/sym-narrow", unknown, ntSym, 40000, 44000, 4, modeBirthdaySpray},
		{"sym-narrow/cone", symNarrow, ntCone, 0, 0, 0, modeBirthdayDial},
		{"cone/sym-narrow", cone, ntSym, 40000, 44000, 4, modeBirthdaySpray},
		{"sym-wide/cone", symWide, ntCone, 0, 0, 0, modeBirthdayDial},               // wide range now tried
		{"cone/sym-wide", cone, ntSym, 1100, 60000, 9, modeBirthdaySpray},           // sprayed over full range
		{"cone/sym-too-few-ports", cone, ntSym, 40000, 44000, 1, modeBirthdaySpray}, // range zeroed later
		{"sym-narrow/sym-narrow", symNarrow, ntSym, 50000, 52000, 3, modeBirthdayMutual},
		{"sym-narrow/sym-wide", symNarrow, ntSym, 1100, 60000, 9, modeNone}, // mutual needs both narrow
		{"sym-wide/sym-wide", symWide, ntSym, 1100, 60000, 9, modeNone},
		{"sym-narrow/old-peer", symNarrow, "", 0, 0, 0, modeNone}, // old peer cannot spray back
		{"sym-narrow/unknown", symNarrow, ntUnknown, 0, 0, 0, modeNone},
		{"sym-narrow/unknown-range", symNarrow, ntUnknown, 40000, 44000, 4, modeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.chooseMode(c.own, c.peerNT, c.peerLo, c.peerHi, c.peerPorts)
			assert.Equal(t, c.want, got)
		})
	}
}

// The noise-dial rule (a peer across a censoring border) holds for a birthday punch as it does for
// the ordinary one: with every address refused, not a single socket or spray packet goes out.
func TestBirthdayRespectsNoiseDialRule(t *testing.T) {
	refuseAll := func(_ types.PeerID, _ string) bool { return true }
	p := newPair(t, func(a, b *Config) {
		a.OwnNAT = symNAT(40000, 44000, 4)
		b.OwnNAT = coneNAT()
		a.Refuse = refuseAll
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := p.a.Connect(ctx, idB)
	assert.Error(t, err)

	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	assert.Zero(t, p.net.bdDialN, "a refused peer is not punched")
	assert.False(t, p.net.connected[idA])
}

// Two birthday punches may run on this node at once, a third is turned away.
func TestBirthdayNodeConcurrencyCap(t *testing.T) {
	s := New(Config{})
	assert.True(t, s.beginBirthday())
	assert.True(t, s.beginBirthday())
	assert.False(t, s.beginBirthday(), "the third is over the cap")
	s.endBirthday()
	assert.True(t, s.beginBirthday(), "a freed slot is reusable")
}
