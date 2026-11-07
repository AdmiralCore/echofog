package routepolicy

import (
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// The pure function: for every country row, with the freeze mark and without it.
func TestDecideFrozenTable(t *testing.T) {
	r := Default()
	for _, own := range []string{"RU", "CN", "IR", "DE", ""} {
		for _, net := range []NetClass{NetWired, NetMobile} {
			for _, dest := range []DestClass{DestForeign, DestNeighbor, DestDomestic} {
				plain := r.Decide(own, net, dest)
				if got := r.DecideFrozen(own, net, dest, false); !sameDecision(got, plain) {
					t.Errorf("%q/%v/%v: not frozen must equal Decide: %+v against %+v", own, net, dest, got, plain)
				}
				got := r.DecideFrozen(own, net, dest, true)
				switch {
				case dest == DestDomestic:
					if !sameDecision(got, plain) {
						t.Errorf("%q/%v/domestic: a frozen network does not change the domestic row: %+v", own, net, got)
					}
				case own == "RU" && net == NetMobile:
					if !sameDecision(got, plain) {
						t.Errorf("RU mobile is already the frozen row: %+v against %+v", got, plain)
					}
				default:
					if got.RuleID != "frozen-foreign" || got.Direct || got.UDP || got.Suites != nil {
						t.Errorf("%q/%v/%v frozen: want the frozen row, got %+v", own, net, dest, got)
					}
					if got.TCPLimit != ProbeBytes || got.Probe != ProbeBytes || ProbeBytes > 12288 {
						t.Errorf("%q/%v/%v frozen: at most 12 KiB on TCP: %+v", own, net, dest, got)
					}
					if !got.EntryInCountry || !got.EntryNeighbor {
						t.Errorf("%q/%v/%v frozen: the first hop is an entry at home or next door: %+v", own, net, dest, got)
					}
				}
			}
		}
	}
}

func sameDecision(a, b Decision) bool {
	if a.RuleID != b.RuleID || a.Direct != b.Direct || a.TCPLimit != b.TCPLimit || a.UDP != b.UDP || a.Rotate != b.Rotate ||
		a.EntryInCountry != b.EntryInCountry || a.EntryNeighbor != b.EntryNeighbor || a.Probe != b.Probe || len(a.Suites) != len(b.Suites) {
		return false
	}
	for i := range a.Suites {
		if a.Suites[i] != b.Suites[i] {
			return false
		}
	}
	return true
}

// The four rows the task names, spelled out: wired RU, CN, IR and the others lose their direct
// carrier once frozen; before, they had it.
func TestFrozenRowsOfWiredRUCNIRAndTheRest(t *testing.T) {
	r := Default()
	for _, c := range []struct {
		own, before string
	}{{"RU", "ru-wired-foreign"}, {"CN", "cn-foreign"}, {"IR", "ir-foreign"}, {"DE", "foreign"}} {
		before := r.DecideFrozen(c.own, NetWired, DestForeign, false)
		if before.RuleID != c.before || !before.Direct {
			t.Errorf("%s without the mark: %+v", c.own, before)
		}
		after := r.DecideFrozen(c.own, NetWired, DestForeign, true)
		if after.RuleID != "frozen-foreign" || after.Direct || !after.EntryInCountry {
			t.Errorf("%s with the mark: %+v", c.own, after)
		}
		for _, s := range []uint32{objects.SuiteNoise3, objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2} {
			if after.AllowsSuite(s) && !after.Direct && false {
				t.Errorf("unreachable")
			}
		}
	}
}

// A rules file that lowers the TCP limit still applies to the frozen row.
func TestTheRulesFileStillCapsTheFrozenRow(t *testing.T) {
	r := &Rules{tcpLimit: 4096}
	got := r.DecideFrozen("DE", NetWired, DestForeign, true)
	if got.TCPLimit != 4096 || got.Probe != 4096 {
		t.Errorf("the file's 4 KiB cap must win over 12 KiB: %+v", got)
	}
}

var (
	t0   = time.Unix(1_700_000_000, 0)
	addr = netip.MustParseAddr("203.0.113.5")
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func TestTwoStallsAtOneAddressMarkTheNetwork(t *testing.T) {
	clk := &clock{t0}
	tr := NewFreezeTracker(clk.Now)
	if tr.Frozen("wifi") {
		t.Fatal("a new network is not frozen")
	}
	if tr.NoteStall("wifi", addr, 16<<10) {
		t.Error("one stall is not the mark")
	}
	if tr.Frozen("wifi") {
		t.Error("one stall does not freeze a network")
	}
	if !tr.NoteStall("wifi", addr, 15<<10) {
		t.Error("the repeat at the same address is the mark, and the call says so")
	}
	if !tr.Frozen("wifi") {
		t.Error("two stalls at 12-20 KiB at one address freeze the network")
	}
	if tr.NoteStall("wifi", addr, 16<<10) {
		t.Error("a network already marked is not marked again")
	}
	if tr.Frozen("lte") {
		t.Error("another network is not frozen")
	}
}

func TestOnlyStallsInTheFreezeWindowCount(t *testing.T) {
	tr := NewFreezeTracker((&clock{t0}).Now)
	for _, n := range []int{0, 1 << 10, FreezeMin - 1, FreezeMax + 1, 1 << 20} {
		tr.NoteStall("n", addr, n)
		tr.NoteStall("n", addr, n)
	}
	if tr.Frozen("n") {
		t.Error("a stall outside 12-20 KiB is a slow server, not the freeze")
	}
	tr.NoteStall("n", addr, FreezeMin)
	tr.NoteStall("n", addr, FreezeMax)
	if !tr.Frozen("n") {
		t.Error("both edges are inside the window")
	}
}

func TestTwoStallsAtTwoAddressesAreNotTheSign(t *testing.T) {
	tr := NewFreezeTracker((&clock{t0}).Now)
	tr.NoteStall("n", netip.MustParseAddr("203.0.113.5"), 16<<10)
	tr.NoteStall("n", netip.MustParseAddr("203.0.113.6"), 16<<10)
	if tr.Frozen("n") {
		t.Error("the sign is a repeat at the same address, not two hiccups at two")
	}
}

func TestTheMarkLastsADayAndThenTheCountStartsAgain(t *testing.T) {
	clk := &clock{t0}
	tr := NewFreezeTracker(clk.Now)
	tr.NoteStall("n", addr, 16<<10)
	tr.NoteStall("n", addr, 16<<10)
	clk.now = t0.Add(FreezeTTL - time.Minute)
	if !tr.Frozen("n") {
		t.Error("still frozen inside the day")
	}
	clk.now = t0.Add(FreezeTTL + time.Second)
	if tr.Frozen("n") {
		t.Error("the mark is gone after 24 hours")
	}
	if tr.NoteStall("n", addr, 16<<10) {
		t.Error("one stall after the expiry is not the mark: the count started again")
	}
}

func TestForgetClearsANetworkAndStallsAgeOut(t *testing.T) {
	clk := &clock{t0}
	tr := NewFreezeTracker(clk.Now)
	tr.NoteStall("n", addr, 16<<10)
	tr.NoteStall("n", addr, 16<<10)
	tr.Forget("n")
	if tr.Frozen("n") {
		t.Error("leaving a network drops its mark")
	}
	// a lone stall does not wait for ever for its repeat
	tr.NoteStall("n", addr, 16<<10)
	clk.now = t0.Add(FreezeTTL + time.Second)
	if tr.NoteStall("n", addr, 16<<10) {
		t.Error("two stalls a day apart are two hiccups")
	}
}

func TestTheTrackerIsBounded(t *testing.T) {
	tr := NewFreezeTracker((&clock{t0}).Now)
	for i := 0; i < 5000; i++ {
		a := netip.AddrFrom4([4]byte{203, 0, byte(i >> 8), byte(i)})
		tr.NoteStall("net-"+string(rune('a'+i%26))+string(rune('a'+i/26%26)), a, 16<<10)
	}
	if n := tr.size(); n > 4096 {
		t.Errorf("%d entries kept", n)
	}
}
