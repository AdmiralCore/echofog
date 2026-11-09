package behaviour

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
)

var (
	ruMobile = Product{Country: "RU", Net: routepolicy.NetMobile}
	ruWired  = Product{Country: "RU", Net: routepolicy.NetWired}
	cnWired  = Product{Country: "CN", Net: routepolicy.NetWired}
)

// D16: the freeze is reproduced, and the product's answer to it is read off its own policy table.
func TestFreezeIsReproducedAndTheProductsAnswerIsMeasured(t *testing.T) {
	const gb = 1 << 30
	freeze := CensorConfig{FreezeBytes: 16384}

	control := MeasureFreeze(ruWired, CensorConfig{}, gb)
	assert.True(t, control.Completed, "without a freeze rule a gigabyte goes")

	m := MeasureFreeze(ruMobile, freeze, gb)
	assert.True(t, m.Completed)
	assert.Equal(t, "entry-in-country", m.Via, "a Russian mobile network goes through a home entry")
	assert.Equal(t, "ru-mobile-foreign", m.Rule)

	w := MeasureFreeze(ruWired, freeze, gb)
	assert.True(t, w.Completed)
	assert.Equal(t, "udp", w.Via, "TCP freezes, so a wired client with UDP uses it")

	// T6.4d: UDP gone and TCP frozen. The first two flows stop at the budget, which marks the network
	// frozen; the transfer then goes through the home entry.
	both := CensorConfig{FreezeBytes: 16384, UDPDrop: true}
	for _, p := range []Product{ruWired, cnWired} {
		r := MeasureFreeze(p, both, gb)
		assert.True(t, r.Completed, "%s: %+v", r.Rule, r)
		assert.Equal(t, "entry-after-freeze", r.Via)
		assert.Equal(t, 2, r.Attempts, "the flow and its repeat are all it costs")
		assert.Equal(t, "frozen-foreign", r.Rule)
	}
}

// Without an entry the answer is a plain error, quickly, and not a storm of retries.
func TestFreezeWithoutAnEntryIsAPlainErrorWithoutAStorm(t *testing.T) {
	const gb = 1 << 30
	both := CensorConfig{FreezeBytes: 16384, UDPDrop: true}
	for _, p := range []Product{{Country: "RU", Net: ruWired.Net, NoEntry: true}, {Country: "CN", Net: cnWired.Net, NoEntry: true}} {
		r := MeasureFreeze(p, both, gb)
		assert.False(t, r.Completed)
		assert.Equal(t, "no-entry", r.Via)
		assert.NotEmpty(t, r.Error, "a connection report has something to say")
		assert.LessOrEqual(t, r.Elapsed, time.Minute, "the error comes within a minute")
		assert.LessOrEqual(t, r.Attempts, 3, "the two stalls and nothing after them")
	}
}

func TestFreezeThroughAHomeEntryCompletesEvenWithoutUDP(t *testing.T) {
	// the mobile row has a TCP limit of a probe and a home entry: the bulk goes through the entry
	r := MeasureFreeze(ruMobile, CensorConfig{FreezeBytes: 16384, UDPDrop: true}, 1<<20)
	assert.True(t, r.Completed)
	assert.Equal(t, "entry-in-country", r.Via)
}

func quicTCPCard(t *testing.T, sni string, ip netip.Addr) *objects.Card {
	t.Helper()
	params, err := objects.Marshal(&pb.SuiteParams{Sni: sni})
	require.NoError(t, err)
	return &objects.Card{Scope: objects.ScopePeer, Body: &pb.NodeCard{Addrs: []*pb.Addr{
		{Suite: objects.SuiteReality2QUIC, Host: ip.String(), Port: 443, Mask: 1, Params: params},
		{Suite: objects.SuiteReality2TCP, Host: ip.String(), Port: 443, Mask: 1, Params: params},
	}}}
}

// D17 on the product's real dial race: carrier.Plan orders the attempts and carrier.Dial runs the
// happy-eyeballs race on a virtual clock.
func TestUDPBanCostsOneHappyEyeballsDelay(t *testing.T) {
	card := quicTCPCard(t, "cover.lab", netip.MustParseAddr("198.51.100.20"))
	rtt := 100 * time.Millisecond

	free := MeasureDial(card, ruWired, CensorConfig{}, rtt)
	require.True(t, free.OK, free.Err)
	assert.Equal(t, uint32(objects.SuiteReality2QUIC), free.Suite)
	assert.Less(t, free.Elapsed, 300*time.Millisecond, "QUIC wins before the second attempt even starts")

	banned := MeasureDial(card, ruWired, CensorConfig{UDPDrop: true}, rtt)
	require.True(t, banned.OK, banned.Err)
	assert.Equal(t, uint32(objects.SuiteReality2TCP), banned.Suite)
	assert.GreaterOrEqual(t, banned.Elapsed, 300*time.Millisecond, "TCP starts after the specification's delay")
	assert.Less(t, banned.Elapsed, time.Second, "and nothing waits for the 4 s QUIC timeout")
	assert.Equal(t, 2, banned.Attempts)
}

// D18, D23.
func TestTheProductsOwnCardPassesAnAllowlistAndAStrangersDoesNot(t *testing.T) {
	allow := CensorConfig{
		AllowSNI:  map[string]bool{"cover.lab": true},
		AllowCIDR: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		Only443:   true,
	}
	ok := MeasureDial(quicTCPCard(t, "cover.lab", netip.MustParseAddr("203.0.113.15")), ruWired, allow, 50*time.Millisecond)
	assert.True(t, ok.OK, ok.Err)
	assert.Equal(t, 0, ok.Blocked, "an allowed name and range is never blocked")

	bad := MeasureDial(quicTCPCard(t, "evil.example", netip.MustParseAddr("198.51.100.99")), ruWired, allow, 50*time.Millisecond)
	assert.False(t, bad.OK)
	assert.Equal(t, 2, bad.Blocked, "both attempts hit the list")
}

// D25.
func TestPort443DownIsAnsweredByTheBackupPortWithinTenSeconds(t *testing.T) {
	params, err := objects.Marshal(&pb.SuiteParams{Sni: "cover.lab"})
	require.NoError(t, err)
	card := &objects.Card{Scope: objects.ScopePeer, Body: &pb.NodeCard{Addrs: []*pb.Addr{
		{Suite: objects.SuiteReality2TCP, Host: "198.51.100.20", Port: 443, Mask: 1, Params: params},
		{Suite: objects.SuiteReality2TCP, Host: "198.51.100.20", Port: 8443, Mask: 1, Params: params},
	}}}
	r := MeasureDial(card, ruWired, CensorConfig{Port443Down: true}, 100*time.Millisecond)
	require.True(t, r.OK, r.Err)
	assert.Less(t, r.Elapsed, 10*time.Second)
	assert.Equal(t, 1, r.Blocked, "the reset on 443 is the only block")

	only443 := &objects.Card{Scope: objects.ScopePeer, Body: &pb.NodeCard{Addrs: card.Body.Addrs[:1]}}
	none := MeasureDial(only443, ruWired, CensorConfig{Port443Down: true}, 100*time.Millisecond)
	assert.False(t, none.OK, "a card with no other port has no way round")
}

// D21 on the real PathPolicy.
func TestAndRuleNeverFiresOnTheProductBecauseItKeepsTwoCarriersPerPair(t *testing.T) {
	rule := &AndRule{ASNs: map[string]bool{"AS64500": true}, JA4s: map[string]bool{"j": true}, Parallel: 3, Penalty: 120 * time.Second}
	cfg := CensorConfig{And: rule}
	prod := MeasureParallel(cfg, 50, false)
	assert.LessOrEqual(t, prod.MaxOpen, 2, "H11: not more than two carriers to one (address, SNI)")
	assert.False(t, prod.Fired)

	naive := MeasureParallel(cfg, 8, true)
	assert.True(t, naive.Fired, "the control: a client without the cap trips the rule")
	assert.Greater(t, naive.MaxOpen, 3)
}

// D24 on the real PathPolicy.
func TestPenaltyBackoffSpacesTheRetriesAndSaysHowItGrows(t *testing.T) {
	r := MeasurePenalty(300*time.Second, 2*time.Hour)
	assert.Equal(t, 120*time.Second, r.FirstBackoff)
	assert.LessOrEqual(t, r.InWindow120, 1, "one handshake per 120 s window")
	assert.LessOrEqual(t, r.InWindow600, 3)
	require.GreaterOrEqual(t, len(r.Attempts), 3)
	assert.Equal(t, time.Duration(0), r.Attempts[0])
	assert.Equal(t, 120*time.Second, r.Attempts[1])
	assert.Equal(t, 360*time.Second, r.Attempts[2], "the second penalty is twice the first")
	assert.True(t, r.Grows, "the backoff grows with every failure: the launchpad's 'no growth' is not what H15 says")
	assert.Equal(t, 60*time.Second, r.Recovery, "the censor lifts the block at 300 s; the next allowed attempt is at 360 s")

	never := MeasurePenalty(0, 2*time.Hour)
	for i := 1; i < len(never.Attempts); i++ {
		assert.LessOrEqual(t, never.Attempts[i]-never.Attempts[i-1], 30*time.Minute, "capped at 30 minutes")
	}
	assert.Less(t, len(never.Attempts), 20, "two hours of a permanent block cost a dozen handshakes, not thousands")
}

// D22: the product's ClientHello, measured, not guessed.
func TestCrowdShareOfTheProductsFingerprint(t *testing.T) {
	r, err := MeasureCrowd(20)
	require.NoError(t, err)
	assert.Equal(t, 20, r.Clients)
	assert.NotEmpty(t, r.Top)
	assert.GreaterOrEqual(t, r.Distinct, 1)
	assert.InDelta(t, 1.0, r.TopShare, 0.5, "with one profile, the top share is most of the crowd")
	assert.LessOrEqual(t, r.TopShare, 1.0)
}

// H16.
func TestTheProductsHelloCarriesGreaseECH(t *testing.T) {
	r, err := MeasureECH()
	require.NoError(t, err)
	assert.True(t, r.Present, "GREASE-ECH is kept in the hello (H16)")
	assert.Greater(t, r.Bytes, 200)
	assert.NotEmpty(t, r.JA4)
}
