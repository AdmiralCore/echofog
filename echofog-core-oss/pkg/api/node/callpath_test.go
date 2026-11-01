package node

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// Each case gets the advice that fits it: router settings are never offered behind the
// provider's NAT, where they cannot help, and a relay chosen on purpose gets no advice at all.
func TestPathReasonFitsTheCase(t *testing.T) {
	cone := pathFacts{NAT: "cone"}
	cases := []struct {
		name string
		f    pathFacts
		want string
	}{
		{"anonymous mode wins over everything", pathFacts{Anonymous: true, Forced: true, Guard: true, Carrier: true, NAT: "symmetric"}, pathAnonymous},
		{"a proxy hides the address", pathFacts{Proxy: true, NAT: "unknown"}, pathProxy},
		{"calls set to go through a relay", pathFacts{Forced: true, NAT: "public"}, pathSettings},
		{"guard mode keeps a user across the border on a relay", pathFacts{Guard: true, NAT: "symmetric", Carrier: true}, pathGuard},
		{"a new link is still looking for a direct one", pathFacts{New: true, NAT: "symmetric"}, pathTrying},
		{"a new link behind the provider's NAT may still punch through", pathFacts{New: true, NAT: "cone", Carrier: true}, pathTrying},
		{"behind the provider's NAT the router cannot help", pathFacts{NAT: "symmetric", Carrier: true}, pathCarrier},
		{"carrier NAT even with a cone mapping", pathFacts{NAT: "cone", Carrier: true}, pathCarrier},
		{"a router that lets nobody in", cone, pathRouter},
		{"a symmetric router with a public WAN address", pathFacts{NAT: "symmetric"}, pathRouter},
		{"this side has a public address", pathFacts{NAT: "public"}, pathRemote},
		{"this side's router forwards a port", pathFacts{NAT: "symmetric", Mapped: true}, pathRemote},
		{"the NAT is not known yet", pathFacts{NAT: "unknown"}, pathChecking},
		{"no NAT check at all", pathFacts{}, pathChecking},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, pathReason(c.f), c.name)
	}
}

// A call's summary names the reason that matters most: what the user chose, then what they can
// act on, then what passes by itself.
func TestPathSummaryPicksWhatMattersMost(t *testing.T) {
	assert.Equal(t, "", pathSummary(nil), "nothing relayed")
	assert.Equal(t, pathRouter, pathSummary([]string{pathGuard, pathTrying, pathRouter, pathRemote}))
	assert.Equal(t, pathCarrier, pathSummary([]string{pathRouter, pathCarrier}))
	assert.Equal(t, pathGuard, pathSummary([]string{pathGuard, pathGuard}))
	assert.Equal(t, pathTrying, pathSummary([]string{pathGuard, pathTrying}), "a link still being made comes before guard mode")
	assert.Equal(t, pathSettings, pathSummary([]string{pathCarrier, pathSettings}))
	assert.Equal(t, "", pathSummary([]string{"something new"}), "an unknown code is not made up into one")
}

// The badge turns from calm to a warning only when the relay costs something people notice.
func TestPathHurtsOnlyWhenNoticeable(t *testing.T) {
	assert.Equal(t, "", pathHurts(48, 0, 0), "a quick relay in a voice call")
	assert.Equal(t, "", pathHurts(0, 0, 0), "not measured yet")
	assert.Equal(t, "ping", pathHurts(pathSlowPing, 8000, 8000))
	assert.Equal(t, "ping", pathHurts(400, 1000, 8000), "a slow round trip is said first")
	assert.Equal(t, "capped", pathHurts(60, 3000, 8000), "a stream held far below its preset")
	assert.Equal(t, "", pathHurts(60, 6000, 8000), "a small cut is the rate control at work")
}

// The other side's NAT is read from its call profile only where the profile is its own word.
func TestMemberReachReadsTheProfile(t *testing.T) {
	open := tree.PeerNodeProfile{MaxRelayChildren: 4}
	cone := tree.PeerNodeProfile{MaxRelayChildren: 2}
	leaf := tree.PeerNodeProfile{MaxRelayChildren: 0}
	sym := tree.PeerNodeProfile{MaxRelayChildren: 0, RelayOnly: true}
	raised := tree.PeerNodeProfile{MaxRelayChildren: tree.DefaultCoordinatorFanout}

	assert.Equal(t, "open", memberReach(open, true, false))
	assert.Equal(t, "cone", memberReach(cone, false, false))
	assert.Equal(t, "symmetric", memberReach(sym, true, false), "what a member sent the coordinator")
	assert.Equal(t, "", memberReach(sym, false, false), "on a member RelayOnly may be the coordinator's mark")
	assert.Equal(t, "symmetric", memberReach(sym, false, true), "the coordinator's own mark is its own word")
	assert.Equal(t, "", memberReach(leaf, true, false), "a leaf whose NAT is not known yet")
	assert.Equal(t, "", memberReach(raised, false, true), "the coordinator's fan-out was raised: it declared less")
	assert.Equal(t, "cone", memberReach(cone, false, true))
}

// A member counts as newly relayed from the first time it is seen through a relay in this call;
// a new call starts the clock again.
func TestCallPathSeenStartsAClockPerCall(t *testing.T) {
	var s callPathSeen
	var bob types.PeerID
	bob[0] = 0x0b
	t0 := time.Now()
	assert.Zero(t, s.relayedFor("c1", bob, t0))
	assert.Equal(t, 30*time.Second, s.relayedFor("c1", bob, t0.Add(30*time.Second)))
	assert.Zero(t, s.relayedFor("c2", bob, t0.Add(time.Hour)), "another call")
}
