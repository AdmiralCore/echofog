package behaviour

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	dstA = netip.MustParseAddrPort("203.0.113.15:443")
	dstB = netip.MustParseAddrPort("198.51.100.7:443")
)

func tcpAt(dst netip.AddrPort, flow int) Attempt {
	return Attempt{Flow: flow, Proto: "tcp", Dst: dst, SNI: "cover.lab"}
}

func TestVClockFiresTimersInOrderOnAdvance(t *testing.T) {
	c := NewVClock()
	start := c.Now()
	var order []int
	c.AfterFunc(300*time.Millisecond, func() { order = append(order, 2) })
	c.AfterFunc(100*time.Millisecond, func() { order = append(order, 1) })
	stopped := c.AfterFunc(200*time.Millisecond, func() { order = append(order, 99) })
	assert.True(t, stopped.Stop())
	c.Advance(250 * time.Millisecond)
	assert.Equal(t, []int{1}, order)
	c.Advance(100 * time.Millisecond)
	assert.Equal(t, []int{1, 2}, order)
	assert.Equal(t, 350*time.Millisecond, c.Now().Sub(start))
}

// D16: a TCP flow freezes once the server has sent the budget; UDP is not touched.
func TestFreezeRuleStopsATCPFlowAtItsBudget(t *testing.T) {
	c := NewCensor(CensorConfig{FreezeBytes: 16384}, NewVClock())
	a := tcpAt(dstA, 1)
	require.Equal(t, Pass, c.Begin(a).Kind)
	a.ServerBytes = 16383
	assert.Equal(t, Pass, c.Judge(a).Kind)
	a.ServerBytes = 16384
	assert.Equal(t, Freeze, c.Judge(a).Kind)
	u := Attempt{Flow: 2, Proto: "udp", Dst: dstA, ServerBytes: 1 << 30}
	assert.Equal(t, Pass, c.Begin(u).Kind)
	assert.Equal(t, Pass, c.Judge(u).Kind, "the freeze rule is a TCP rule")
}

// D17.
func TestUDPBlockDropsUDPAndLeavesTCP(t *testing.T) {
	c := NewCensor(CensorConfig{UDPDrop: true}, NewVClock())
	assert.Equal(t, Drop, c.Begin(Attempt{Flow: 1, Proto: "udp", Dst: dstA}).Kind)
	assert.Equal(t, Pass, c.Begin(tcpAt(dstA, 2)).Kind)
}

// D18, D23: an allow-list of names and of ranges, and the only-443 mode.
func TestAllowlistByNameRangeAndPort(t *testing.T) {
	c := NewCensor(CensorConfig{
		AllowSNI:  map[string]bool{"cover.lab": true},
		AllowCIDR: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		Only443:   true,
	}, NewVClock())
	assert.Equal(t, Pass, c.Begin(tcpAt(dstA, 1)).Kind)
	assert.Equal(t, Drop, c.Begin(Attempt{Flow: 2, Proto: "tcp", Dst: dstA, SNI: "other.example"}).Kind, "a name outside the list")
	assert.Equal(t, Drop, c.Begin(tcpAt(dstB, 3)).Kind, "an address outside the allocated range")
	assert.Equal(t, Drop, c.Begin(tcpAt(netip.MustParseAddrPort("203.0.113.15:8443"), 4)).Kind, "only 443")
	open := NewCensor(CensorConfig{}, NewVClock())
	assert.Equal(t, Pass, open.Begin(tcpAt(dstB, 1)).Kind, "no list, no block")
}

// D19.
func TestThrottleGivesARate(t *testing.T) {
	c := NewCensor(CensorConfig{ThrottleBps: 8000}, NewVClock())
	v := c.Begin(tcpAt(dstA, 1))
	assert.Equal(t, Throttle, v.Kind)
	assert.Equal(t, 8000, v.RateBps)
}

// D20.
func TestShutdownDropsEverythingInsideTheWindow(t *testing.T) {
	clk := NewVClock()
	from := clk.Now().Add(10 * time.Second)
	c := NewCensor(CensorConfig{ShutdownFrom: from, ShutdownTo: from.Add(time.Minute)}, clk)
	assert.Equal(t, Pass, c.Begin(tcpAt(dstA, 1)).Kind)
	clk.Advance(30 * time.Second)
	assert.Equal(t, Drop, c.Begin(tcpAt(dstA, 2)).Kind)
	assert.Equal(t, Drop, c.Begin(Attempt{Flow: 3, Proto: "udp", Dst: dstB}).Kind)
	clk.Advance(2 * time.Minute)
	assert.Equal(t, Pass, c.Begin(tcpAt(dstA, 4)).Kind)
}

// D25.
func TestPort443DownResets443Only(t *testing.T) {
	c := NewCensor(CensorConfig{Port443Down: true}, NewVClock())
	assert.Equal(t, Reset, c.Begin(tcpAt(dstA, 1)).Kind)
	assert.Equal(t, Pass, c.Begin(tcpAt(netip.MustParseAddrPort("203.0.113.15:8443"), 2)).Kind)
}

// D21: (ASN in the list) AND (JA4 in the profile) AND (more than three parallel TLS to one address).
func TestAndRuleFiresOnlyWhenAllThreeHold(t *testing.T) {
	rule := &AndRule{ASNs: map[string]bool{"AS64500": true}, JA4s: map[string]bool{"t13d1516h2_chrome": true}, Parallel: 3, Penalty: 120 * time.Second}
	mk := func(ja4, asn string, flow int) Attempt {
		return Attempt{Flow: flow, Proto: "tcp", Dst: dstA, SNI: "cover.lab", JA4: ja4, ASN: asn}
	}
	run := func(ja4, asn string, n int) (fired bool) {
		c := NewCensor(CensorConfig{And: rule}, NewVClock())
		for i := 1; i <= n; i++ {
			if c.Begin(mk(ja4, asn, i)).Kind == Drop {
				return true
			}
		}
		return false
	}
	assert.True(t, run("t13d1516h2_chrome", "AS64500", 4), "the fourth parallel flow")
	assert.False(t, run("t13d1516h2_chrome", "AS64500", 3), "three are allowed")
	assert.False(t, run("t13d1516h2_other", "AS64500", 8), "a fingerprint outside the profile")
	assert.False(t, run("t13d1516h2_chrome", "AS64999", 8), "an ASN outside the list")

	clk := NewVClock()
	c := NewCensor(CensorConfig{And: rule}, clk)
	for i := 1; i <= 4; i++ {
		c.Begin(mk("t13d1516h2_chrome", "AS64500", i))
	}
	v := c.Begin(mk("t13d1516h2_other", "AS64999", 9))
	assert.Equal(t, Drop, v.Kind, "once it fired, everything to the address is dropped")
	assert.WithinDuration(t, clk.Now().Add(120*time.Second), v.Until, time.Second)
	clk.Advance(121 * time.Second)
	for i := 1; i <= 4; i++ {
		c.End(mk("t13d1516h2_chrome", "AS64500", i))
	}
	assert.Equal(t, Pass, c.Begin(mk("t13d1516h2_chrome", "AS64500", 20)).Kind, "the penalty ends")
}

func TestEndedFlowsDoNotCountAsParallel(t *testing.T) {
	rule := &AndRule{ASNs: map[string]bool{"AS64500": true}, JA4s: map[string]bool{"j": true}, Parallel: 3, Penalty: time.Minute}
	c := NewCensor(CensorConfig{And: rule}, NewVClock())
	for i := 1; i <= 50; i++ {
		a := Attempt{Flow: i, Proto: "tcp", Dst: dstA, JA4: "j", ASN: "AS64500"}
		require.Equal(t, Pass, c.Begin(a).Kind, "flow %d", i)
		c.End(a)
	}
}

func TestLoadCensorConfigReadsARuleSet(t *testing.T) {
	spec := `{"freeze_bytes":16384,"udp_drop":true,"allow_sni":["cover.lab"],"allow_cidr":["203.0.113.0/24"],
		"only_443":true,"throttle_bps":8000,"port_443_down":false,
		"and":{"asns":["AS64500"],"ja4s":["j"],"parallel":3,"penalty_seconds":120}}`
	c, err := LoadCensorConfig(strings.NewReader(spec))
	require.NoError(t, err)
	assert.Equal(t, 16384, c.FreezeBytes)
	assert.True(t, c.UDPDrop)
	assert.True(t, c.AllowSNI["cover.lab"])
	require.Len(t, c.AllowCIDR, 1)
	assert.True(t, c.Only443)
	assert.Equal(t, 8000, c.ThrottleBps)
	require.NotNil(t, c.And)
	assert.Equal(t, 120*time.Second, c.And.Penalty)
	assert.True(t, c.And.ASNs["AS64500"])

	for name, bad := range map[string]string{
		"not json":    `{`,
		"bad cidr":    `{"allow_cidr":["nope"]}`,
		"negative":    `{"freeze_bytes":-1}`,
		"and no ja4s": `{"and":{"asns":["A"],"parallel":3,"penalty_seconds":1}}`,
		"and zero n":  `{"and":{"asns":["A"],"ja4s":["j"],"parallel":0,"penalty_seconds":1}}`,
	} {
		_, err := LoadCensorConfig(strings.NewReader(bad))
		assert.Error(t, err, name)
	}
}

// T15.0c acceptance, remark 1: a description with a typo must not load as "no rules".
func TestLoadCensorConfigRefusesWhatItDoesNotKnow(t *testing.T) {
	for name, bad := range map[string]string{
		"typo in a key":     `{"freez_bytes":16384,"udp_dorp":true}`,
		"unknown key":       `{"freeze_bytes":1,"colour":"red"}`,
		"unknown in and":    `{"and":{"asns":["A"],"ja4s":["j"],"parallel":3,"penalty_seconds":1,"extra":1}}`,
		"trailing value":    `{"freeze_bytes":1} {"udp_drop":true}`,
		"trailing garbage":  `{"freeze_bytes":1} x`,
		"window backwards":  `{"shutdown_from":"2026-11-01T01:00:00Z","shutdown_to":"2026-11-01T00:00:00Z"}`,
		"window of nothing": `{"shutdown_from":"2026-11-01T01:00:00Z","shutdown_to":"2026-11-01T01:00:00Z"}`,
		"only one end":      `{"shutdown_from":"2026-11-01T01:00:00Z"}`,
	} {
		_, err := LoadCensorConfig(strings.NewReader(bad))
		assert.Error(t, err, name)
	}
	_, err := LoadCensorConfig(strings.NewReader(`{"freeze_bytes":1}` + "\n  \n"))
	assert.NoError(t, err, "whitespace after the value is not trailing data")
}

// T15.0c acceptance, remark 2: the start of a shutdown window cuts the flows that are open.
func TestShutdownCutsTheFlowsThatAreOpen(t *testing.T) {
	clk := NewVClock()
	from := clk.Now().Add(10 * time.Second)
	c := NewCensor(CensorConfig{ShutdownFrom: from, ShutdownTo: from.Add(time.Minute)}, clk)
	a := tcpAt(dstA, 1)
	require.Equal(t, Pass, c.Begin(a).Kind)
	assert.Equal(t, Pass, c.Judge(a).Kind)
	clk.Advance(30 * time.Second)
	v := c.Judge(a)
	assert.Equal(t, Drop, v.Kind, "the link is down: an open flow gets nothing more")
	assert.WithinDuration(t, from.Add(time.Minute), v.Until, time.Second)
	clk.Advance(2 * time.Minute)
	assert.Equal(t, Pass, c.Judge(a).Kind, "and after the window it goes on")
}

// H15: the attempts the product may make to a pair that stays blocked.
func TestTheReportCarriesTheH15Sequence(t *testing.T) {
	r := MeasurePenalty(0, 2*time.Hour)
	want := []time.Duration{0, 2 * time.Minute, 6 * time.Minute, 14 * time.Minute, 30 * time.Minute}
	require.GreaterOrEqual(t, len(r.Attempts), 5)
	assert.Equal(t, want, r.Attempts[:5], "0, 2, 6, 14 and 30 minutes")
	for i := 5; i < len(r.Attempts); i++ {
		assert.LessOrEqual(t, r.Attempts[i]-r.Attempts[i-1], 30*time.Minute+time.Second)
	}
	var rep Report
	rep.PenaltyNever = r // the field exists and is filled by RunAll; see TestReportSaysWhatItDidNotCheck
	assert.Equal(t, r, rep.PenaltyNever)
}

// L1 of the T15.0e acceptance: a file that says nothing is not a censor, and a key twice is a typo.
func TestLoadCensorConfigRefusesEmptyAndDoubledKeys(t *testing.T) {
	for name, bad := range map[string]string{
		"empty object":     `{}`,
		"null":             `null`,
		"all rules off":    `{"freeze_bytes":0,"udp_drop":false}`,
		"key twice":        `{"freeze_bytes":16384,"freeze_bytes":1}`,
		"key twice, udp":   `{"udp_drop":true,"udp_drop":false}`,
		"key twice in and": `{"and":{"asns":["A"],"asns":["B"],"ja4s":["j"],"parallel":3,"penalty_seconds":1}}`,
		"and twice":        `{"and":{"asns":["A"],"ja4s":["j"],"parallel":3,"penalty_seconds":1},"and":null}`,
	} {
		_, err := LoadCensorConfig(strings.NewReader(bad))
		assert.Error(t, err, name)
	}
	_, err := LoadCensorConfig(strings.NewReader(`{"udp_drop":true}`))
	assert.NoError(t, err, "one rule is a censor")
}
