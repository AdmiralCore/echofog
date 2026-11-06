package node_test

import (
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// T6.4c: while the client's own country is unknown the unknown row applies: TLS-shaped carriers over
// TCP only; no noise3, no QUIC, no UDP, no hole punching, and the same for a bootstrap dial.

func isUDPSuite(c dialCall) bool {
	return c.suite == objects.SuiteNoise3 || c.suite == objects.SuiteReality2QUIC
}

func TestUnknownCountryDialsTLSOverTCPOnly(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true})
	_, _ = w.open(t, node.DialOpt{})
	if n := w.log.count(isUDPSuite); n != 0 {
		t.Fatalf("%d noise3 or QUIC dials while the country is unknown, want none", n)
	}
	if n := w.log.count(func(c dialCall) bool { return !isUDPSuite(c) }); n == 0 {
		t.Fatal("no TCP carrier was tried: the unknown row leaves reality2-tcp and ws2")
	}
}

func TestUnknownCountryBootstrapIsFilteredToo(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true})
	mask := keys.New(keys.X25519, make([]byte, 32))
	for _, s := range []uint32{objects.SuiteNoise3, objects.SuiteReality2QUIC} {
		_, err := w.a.rt.DialBootstrap(w.ctx, node.BootstrapTarget{
			Addr: &pb.Addr{Suite: s, Host: "203.0.113.5", Port: 4430}, Mask: mask}, node.DialOpt{})
		if err == nil {
			t.Fatalf("suite %d: a bootstrap dial over UDP with an unknown country", s)
		}
	}
	if n := w.log.count(isUDPSuite); n != 0 {
		t.Fatalf("%d UDP bootstrap dials, want none", n)
	}
}

func TestUnknownCountryNoHolePunching(t *testing.T) {
	pn := &countingPunch{}
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true, features: true, punchNet: pn})
	_, _ = w.open(t, node.DialOpt{})
	time.Sleep(500 * time.Millisecond)
	if n := pn.count(); n != 0 {
		t.Fatalf("the punching network was used %d times with an unknown country", n)
	}
}

// The country becomes known: the next dial follows the new row; no live session is dropped.
func TestCountryBecomingKnownRecalculatesNewPathsOnly(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true})
	live := node.OpenSessions(w.a.rt)
	w.a.rt.SetCountry("DE")
	_, _ = w.open(t, node.DialOpt{})
	if n := w.log.count(func(c dialCall) bool { return c.suite == objects.SuiteNoise3 }); n == 0 {
		t.Fatal("a known country (DE, destination DE) still gets no noise3 attempt")
	}
	if got := node.OpenSessions(w.a.rt); got < live {
		t.Fatalf("open sessions fell from %d to %d when the country became known", live, got)
	}
}

func TestUserCountryOverridesGeoIPAndAutoGoesBack(t *testing.T) {
	w := newRouteWorld(t, routeOpts{})
	if cc, src := w.a.rt.OwnCountry(); cc != "RU" || src != node.CountryAuto {
		t.Fatalf("auto: %q %q, want RU auto", cc, src)
	}
	w.a.rt.SetCountry("DE")
	if cc, src := w.a.rt.OwnCountry(); cc != "DE" || src != node.CountrySet {
		t.Fatalf("set: %q %q, want DE set", cc, src)
	}
	w.a.rt.SetCountry("")
	if cc, src := w.a.rt.OwnCountry(); cc != "RU" || src != node.CountryAuto {
		t.Fatalf("auto again: %q %q, want RU auto", cc, src)
	}
}

// The platform hint ranks below GeoIP and above nothing; time zone and language are no source at all.
func TestCountryHintRanksBelowGeoIP(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true})
	if cc, src := w.a.rt.OwnCountry(); cc != "" || src != node.CountryUnknown {
		t.Fatalf("nothing known: %q %q", cc, src)
	}
	w.a.rt.SetCountryHint("DE")
	if cc, src := w.a.rt.OwnCountry(); cc != "DE" || src != node.CountryHint {
		t.Fatalf("hint: %q %q, want DE hint", cc, src)
	}
	w2 := newRouteWorld(t, routeOpts{})
	w2.a.rt.SetCountryHint("DE")
	if cc, src := w2.a.rt.OwnCountry(); cc != "RU" || src != node.CountryAuto {
		t.Fatalf("GeoIP and hint: %q %q, want RU auto", cc, src)
	}
}

// A user-set RU on a mobile network (no address known) is the worst-case row.
func TestUserSetRUMobileIsTheWorstCaseRow(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, net: rp.NetMobile, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	w.a.rt.SetCountry("RU")
	p, err := w.open(t, node.DialOpt{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if n := w.log.count(func(c dialCall) bool { return strings.HasPrefix(c.host, "203.0.113.") }); n != 0 {
		t.Fatalf("%d direct dials abroad from RU mobile", n)
	}
	rep, ok := w.a.rt.RouteReport(w.b.id.DeviceID)
	if !ok || rep.RuleID != "ru-mobile-foreign" || rep.Country != "RU" || rep.CountrySource != node.CountrySet {
		t.Fatalf("report %+v %v, want ru-mobile-foreign, RU, set (path %v)", rep, ok, p.Path)
	}
}

func TestReportSaysTheCountryIsUnknown(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true})
	_, _ = w.open(t, node.DialOpt{})
	rep, ok := w.a.rt.RouteReport(w.b.id.DeviceID)
	if !ok || rep.RuleID != "unknown" || rep.Country != "" || rep.CountrySource != node.CountryUnknown {
		t.Fatalf("report %+v %v, want the unknown row", rep, ok)
	}
}

func TestCountryChangeIsReported(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true})
	w.a.rt.SetCountry("DE")
	w.a.rt.SetCountry("DE") // no change, no second event
	w.a.rt.SetCountry("FR")
	got := 0
	w.events.mu.Lock()
	for _, e := range w.events.list {
		if e.Kind == node.EventCountryChanged {
			got++
		}
	}
	w.events.mu.Unlock()
	if got != 2 {
		t.Fatalf("%d country-changed events, want 2", got)
	}
}
