package routepolicy_test

import (
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

const (
	n3 = objects.SuiteNoise3
	rq = objects.SuiteReality2QUIC
	rt = objects.SuiteReality2TCP
	ws = objects.SuiteWS2
)

func eq(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// One row of the table per case (escalations, ТЕО 2026-11-04): a pure function of country, network
// class and destination class, no network.
func TestDecideTable(t *testing.T) {
	cases := []struct {
		name   string
		own    string
		net    rp.NetClass
		dest   rp.DestClass
		direct bool
		suites []uint32 // nil = unrestricted
		limit  int
		udp    bool
		rotate bool
		entry  bool
		probe  int
	}{
		{"RU mobile to foreign: probe only, entry inside", "RU", rp.NetMobile, rp.DestForeign, false, nil, 12288, false, false, true, 12288},
		{"RU mobile to a neighbour counts as foreign", "RU", rp.NetMobile, rp.DestNeighbor, false, nil, 12288, false, false, true, 12288},
		{"RU mobile to own country", "RU", rp.NetMobile, rp.DestDomestic, true, []uint32{n3, rq, rt, ws}, 0, true, false, false, 0},
		{"RU wired to foreign", "RU", rp.NetWired, rp.DestForeign, true, nil, 0, true, true, false, 0},
		{"RU unknown net is wired", "RU", rp.NetUnknown, rp.DestForeign, true, nil, 0, true, true, false, 0},
		{"CN to foreign", "CN", rp.NetWired, rp.DestForeign, true, []uint32{rq, rt, ws}, 0, true, true, false, 0},
		{"CN mobile to foreign is the same row", "CN", rp.NetMobile, rp.DestForeign, true, []uint32{rq, rt, ws}, 0, true, true, false, 0},
		{"IR to foreign: no QUIC until udp is allowed", "IR", rp.NetWired, rp.DestForeign, true, []uint32{rt, ws}, 0, false, true, false, 0},
		{"others to foreign", "DE", rp.NetMobile, rp.DestForeign, true, nil, 0, true, true, false, 0},
		{"others to own country", "DE", rp.NetWired, rp.DestDomestic, true, nil, 0, true, false, false, 0},
	}
	r := rp.Default()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := r.Decide(c.own, c.net, c.dest)
			if d.RuleID == "" {
				t.Error("a decision names its rule")
			}
			if d.Direct != c.direct || d.TCPLimit != c.limit || d.UDP != c.udp || d.Rotate != c.rotate ||
				d.EntryInCountry != c.entry || d.Probe != c.probe || !eq(d.Suites, c.suites) {
				t.Fatalf("got %+v, want direct=%v suites=%v limit=%d udp=%v rotate=%v entry=%v probe=%d",
					d, c.direct, c.suites, c.limit, c.udp, c.rotate, c.entry, c.probe)
			}
		})
	}
}

func TestAllowsSuite(t *testing.T) {
	d := rp.Default().Decide("CN", rp.NetWired, rp.DestForeign)
	if d.AllowsSuite(n3) || !d.AllowsSuite(rq) {
		t.Fatalf("CN foreign: noise3 must be refused and reality2-quic allowed: %+v", d)
	}
	free := rp.Default().Decide("DE", rp.NetWired, rp.DestForeign)
	if !free.AllowsSuite(ws) || !free.AllowsSuite(rt) {
		t.Fatal("an unrestricted row allows every suite")
	}
}

func TestClassify(t *testing.T) {
	r := rp.Default()
	if r.Classify("RU", "RU") != rp.DestDomestic {
		t.Error("same country is domestic")
	}
	if r.Classify("RU", "DE") != rp.DestForeign {
		t.Error("another country is foreign")
	}
	// Unknown countries are never domestic.
	if r.Classify("RU", "") != rp.DestForeign || r.Classify("", "RU") != rp.DestForeign {
		t.Error("an unknown country is foreign")
	}
	// A neighbour is foreign by default: the class needs data and a measurement.
	if r.Classify("RU", "KZ") != rp.DestForeign {
		t.Error("KZ is foreign by default")
	}
}

func TestNetClassEffective(t *testing.T) {
	if rp.NetUnknown.Effective() != rp.NetWired || rp.NetMobile.Effective() != rp.NetMobile {
		t.Fatal("unknown counts as wired")
	}
}

// H14: without a verified allowed_cidrs range there is no entry for a Russian mobile network.
func TestNoVerifiedEntriesByDefault(t *testing.T) {
	r := rp.Default()
	if r.HasVerifiedEntries() {
		t.Fatal("the built-in table verifies no entry")
	}
	if r.EntryAllowed("RU", rp.NetMobile, netip.MustParseAddr("198.51.100.77")) {
		t.Fatal("an entry outside a verified range must not be allowed")
	}
}
