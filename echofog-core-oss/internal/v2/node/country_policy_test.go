package node_test

import (
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// T6.4c: while the client's own country is unknown the unknown row applies: TLS-shaped carriers over
// TCP only; no noise3, no QUIC, no UDP, no hole punching, and the same for a bootstrap dial.

func isUDPSuite(c dialCall) bool {
	return c.suite == objects.SuiteNoise3 || c.suite == objects.SuiteReality2QUIC
}

// bTLS gives B a reality2-tcp and a reality2-quic address (distinct pairs, so one failed dial does
// not back the other off) besides its noise3 one.
func bTLS(c *cards.Config) {
	c.Listeners = append(c.Listeners,
		cards.Listener{Suite: objects.SuiteReality2TCP, Host: "203.0.113.10", Port: 443, SNI: "example.com", Mask: c.Listeners[0].Mask},
		cards.Listener{Suite: objects.SuiteReality2QUIC, Host: "203.0.113.11", Port: 443, SNI: "example.com", Mask: c.Listeners[0].Mask})
}

// A client with a known GeoIP country can still say "do not specify": the unknown row applies
// until the user picks a country again or returns to automatic (V2-FIELD-1).
func TestCountryNoneForcesTheUnknownRow(t *testing.T) {
	w := newRouteWorld(t, routeOpts{bCover: true})
	if cc, src := w.a.rt.OwnCountry(); cc != "RU" || src != node.CountryAuto {
		t.Fatalf("before: %q %s, want the GeoIP country", cc, src)
	}
	w.a.rt.SetCountry(node.CountryNone)
	if cc, src := w.a.rt.OwnCountry(); cc != "" || src != node.CountryUnknown {
		t.Fatalf("do not specify: %q %s, want an unknown country", cc, src)
	}
	w.a.rt.SetCountry("de")
	if cc, src := w.a.rt.OwnCountry(); cc != "DE" || src != node.CountrySet {
		t.Fatalf("manual: %q %s", cc, src)
	}
	w.a.rt.SetCountry("")
	if cc, src := w.a.rt.OwnCountry(); cc != "RU" || src != node.CountryAuto {
		t.Fatalf("automatic again: %q %s", cc, src)
	}
}

// An explicit dial (an invitation link, an entry the user added) is not cut by the unknown row:
// the user named that carrier, and the client reaches the node it was given (V2-FIELD-1).
func TestExplicitEntryDialsTheCarrierItNames(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true, bMutate: bTLS})
	if cc, src := w.a.rt.OwnCountry(); cc != "" || src != node.CountryUnknown {
		t.Fatalf("the world must start unknown: %q %s", cc, src)
	}
	_, _ = w.open(t, node.DialOpt{Explicit: true})
	if n := w.log.count(isUDPSuite); n == 0 {
		t.Fatal("an explicit dial left the UDP carriers out: the country policy cut the link")
	}
}

// The same for a bootstrap entry: a user's line is explicit, an official one stays under the
// policy.
func TestExplicitBootstrapEntryIsNotFiltered(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true})
	mask := keys.New(keys.X25519, make([]byte, 32))
	_, _ = w.a.rt.DialBootstrap(w.ctx, node.BootstrapTarget{
		Addr: &pb.Addr{Suite: objects.SuiteNoise3, Host: "203.0.113.5", Port: 4430}, Mask: mask}, node.DialOpt{Explicit: true})
	if n := w.log.count(isUDPSuite); n == 0 {
		t.Fatal("an explicit bootstrap entry does not try the suite it names")
	}
}

// T5.6: the built-in list must contain an entry the unknown row still lets through. reality2-tcp
// and ws2 are that entry kind; noise3 and QUIC are not dialed while the country is unknown.
func TestUnknownCountryLeavesATCPBootstrapEntry(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true})
	mask := keys.New(keys.X25519, make([]byte, 32))
	_, _ = w.a.rt.DialBootstrap(w.ctx, node.BootstrapTarget{
		Addr: &pb.Addr{Suite: objects.SuiteReality2TCP, Host: "203.0.113.5", Port: 443}, Mask: mask}, node.DialOpt{})
	if n := w.log.count(func(c dialCall) bool { return c.suite == objects.SuiteReality2TCP }); n == 0 {
		t.Fatal("the unknown row leaves no reality2-tcp bootstrap entry: T5.6 has nothing to dial")
	}
}

func TestUnknownCountryDialsTLSOverTCPOnly(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true, bMutate: bTLS})
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

// The country becomes known: the next dial follows the new row (no relay here, so a failed dial
// leaves nothing to reuse) ...
func TestCountryBecomingKnownRecalculatesNewPaths(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, noEntry: true, bCover: true, bMutate: bTLS})
	_, _ = w.open(t, node.DialOpt{})
	if n := w.log.count(isUDPSuite); n != 0 {
		t.Fatalf("%d UDP dials before the country is known", n)
	}
	w.a.rt.SetCountry("DE")
	_, _ = w.open(t, node.DialOpt{})
	if n := w.log.count(isUDPSuite); n == 0 {
		t.Fatal("a known country (DE) still gets no QUIC attempt: the new paths follow the new row")
	}
}

// ... and a live session is not dropped by it.
func TestCountryBecomingKnownKeepsLiveSessions(t *testing.T) {
	w := newRouteWorld(t, routeOpts{noLocal: true, bCover: true, bMutate: bTLS})
	if _, err := w.open(t, node.DialOpt{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	live := node.OpenSessions(w.a.rt)
	w.a.rt.SetCountry("DE")
	w.a.rt.SetCountry("RU")
	time.Sleep(200 * time.Millisecond)
	if got := node.OpenSessions(w.a.rt); got != live {
		t.Fatalf("open sessions %d -> %d when the country changed", live, got)
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
