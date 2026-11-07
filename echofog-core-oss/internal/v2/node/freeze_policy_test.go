package node_test

import (
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

var foreignIP = netip.MustParseAddr("203.0.113.5")

func (c *captureEvents) all() []node.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]node.Event(nil), c.list...)
}

func freezeNetwork(t *testing.T, r *node.Runtime) {
	t.Helper()
	r.SetNetworkID("lab-wan")
	node.NoteStall(r, foreignIP, 16<<10)
	node.NoteStall(r, foreignIP, 16<<10)
	if !node.IsFrozen(r) {
		t.Fatal("two stalls at one address must mark the network frozen")
	}
}

func foreignDials(w *routeWorld) int {
	return w.log.count(func(c dialCall) bool { return strings.HasPrefix(c.host, "203.0.113.") })
}

// A wired network in a country with no freeze rule of its own: before the mark, the client tries
// the foreign address directly; after it, no new path does, and the first hop is the home entry.
func TestAFrozenWiredNetworkGoesThroughTheHomeEntry(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	w.a.rt.SetNetworkID("lab-wan")
	_, _ = w.open(t, node.DialOpt{}) // the direct attempt fails (the fake suites are unreachable) and the relay takes over
	if foreignDials(w) == 0 {
		t.Fatal("before the mark a wired client dials the foreign address itself")
	}
	before := foreignDials(w)

	freezeNetwork(t, w.a.rt)
	p, err := w.open(t, node.DialOpt{})
	if err != nil {
		t.Fatalf("Open through the entry: %v", err)
	}
	if p.Path != mux.PathRelay {
		t.Fatalf("path %v, want the relay path through the home entry", p.Path)
	}
	if n := foreignDials(w); n != before {
		t.Fatalf("%d new direct dials to the foreign address after the mark, want none", n-before)
	}
	rep, ok := w.a.rt.RouteReport(w.b.id.DeviceID)
	if !ok || rep.RuleID != "frozen-foreign" || !rep.ViaEntry || !rep.Frozen {
		t.Fatalf("report %+v: want the frozen row, via the entry, marked frozen", rep)
	}
}

// No entry: a plain error in the connection report, fast, and no storm of retries behind it.
func TestAFrozenNetworkWithoutAnEntryFailsPlainlyAndQuietly(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true, noEntry: true})
	freezeNetwork(t, w.a.rt)
	before := foreignDials(w)

	start := time.Now()
	for i := 0; i < 5; i++ {
		_, err := w.open(t, node.DialOpt{})
		if err == nil {
			t.Fatal("no entry and a frozen network: there is no way to the foreign peer")
		}
		if !strings.Contains(err.Error(), node.ErrNoDomesticEntry.Error()) && !strings.Contains(err.Error(), node.ErrRouteUnsupported.Error()) {
			t.Fatalf("error %v, want ErrNoDomesticEntry", err)
		}
	}
	if time.Since(start) > 60*time.Second {
		t.Fatal("the error must come within a minute")
	}
	if n := foreignDials(w) - before; n != 0 {
		t.Fatalf("%d direct dials while frozen without an entry: that is a storm of retries", n)
	}
	ev, ok := w.events.last(node.EventNoDomesticEntry)
	if !ok {
		t.Fatal("the application is told")
	}
	for _, bad := range []string{"сделал", "сделала", "ошибся", "ошиблась"} {
		if strings.Contains(ev.Message, bad) {
			t.Errorf("the message must be neutral: %q", ev.Message)
		}
	}
	if !strings.Contains(ev.Message, "замороз") {
		t.Errorf("the message must say what the network does: %q", ev.Message)
	}
}

func TestOneStallDoesNotFreezeAndTheMarkIsAnEvent(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	w.a.rt.SetNetworkID("lab-wan")
	node.NoteStall(w.a.rt, foreignIP, 16<<10)
	if node.IsFrozen(w.a.rt) {
		t.Fatal("one stall is not the sign")
	}
	if w.events.has(node.EventNetworkFrozen) {
		t.Fatal("no event before the mark")
	}
	node.NoteStall(w.a.rt, foreignIP, 17<<10)
	if !w.events.has(node.EventNetworkFrozen) {
		t.Fatal("the mark is reported to the application")
	}
	for i := 0; i < 3; i++ {
		node.NoteStall(w.a.rt, foreignIP, 17<<10)
	}
	n := 0
	for _, e := range w.events.all() {
		if e.Kind == node.EventNetworkFrozen {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d frozen events, want one", n)
	}
}

func TestChangingTheNetworkResetsTheMark(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	freezeNetwork(t, w.a.rt)
	w.a.rt.SetNetworkID("mobile-4g")
	if node.IsFrozen(w.a.rt) {
		t.Fatal("another network is not frozen")
	}
	if got := w.a.rt.NetworkID(); got != "mobile-4g" {
		t.Fatalf("network id %q", got)
	}
	_, _ = w.open(t, node.DialOpt{})
	if foreignDials(w) == 0 {
		t.Fatal("on the new network the client dials the foreign address directly again")
	}
	w.a.rt.SetNetworkID("lab-wan")
	if node.IsFrozen(w.a.rt) {
		t.Fatal("coming back to the first network is a new start: the change dropped what was learned")
	}
}

// The watcher wraps foreign flows only: a domestic one is returned as it is.
func TestOnlyForeignFlowsAreWatched(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	w.a.rt.SetNetworkID("lab-wan")
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if c := node.WrapFreeze(w.a.rt, a, netip.MustParseAddr("198.51.100.77")); c != a {
		t.Fatal("a domestic flow is not wrapped")
	}
	if c := node.WrapFreeze(w.a.rt, a, foreignIP); c == a {
		t.Fatal("a foreign flow is wrapped")
	}
}

// The watcher itself: a flow that heard 16 KiB and then nothing, while it kept writing, is the
// freeze; the same flow left alone is a quiet connection.
func TestTheWatcherNotesAFlowThatStoppedInTheFreezeWindow(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true, freezeStall: 50 * time.Millisecond})
	stalled := func(heard int, keepWriting bool) {
		t.Helper()
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		c := node.WrapFreeze(w.a.rt, a, foreignIP)
		go func() { _, _ = io.Copy(io.Discard, b) }()
		go func() {
			if heard > 0 {
				_, _ = b.Write(make([]byte, heard))
			}
		}()
		got := 0
		buf := make([]byte, 4096)
		for got < heard {
			n, err := c.Read(buf)
			got += n
			if err != nil {
				t.Fatalf("read: %v", err)
			}
		}
		if keepWriting {
			_, _ = c.Write([]byte("request"))
		}
		time.Sleep(250 * time.Millisecond) // five times the watcher's 50 ms
	}

	w.a.rt.SetNetworkID("n1")
	stalled(5<<10, true)
	stalled(5<<10, true)
	if node.IsFrozen(w.a.rt) {
		t.Fatal("stalls at 5 KiB are a slow server")
	}
	stalled(16<<10, false)
	stalled(16<<10, false)
	if node.IsFrozen(w.a.rt) {
		t.Fatal("a connection that heard 16 KiB and was then left alone is idle, not frozen")
	}
	stalled(25<<10, true)
	stalled(25<<10, true)
	if node.IsFrozen(w.a.rt) {
		t.Fatal("25 KiB is past the window")
	}
	stalled(16<<10, true)
	if node.IsFrozen(w.a.rt) {
		t.Fatal("one stalled flow is not enough")
	}
	stalled(16<<10, true)
	if !node.IsFrozen(w.a.rt) {
		t.Fatal("a flow that stopped at 16 KiB while it waited for an answer, twice at one address, is the freeze")
	}
}

func TestTheWatcherLetsDataThroughUnchanged(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := node.WrapFreeze(w.a.rt, a, foreignIP)
	go func() { _, _ = c.Write([]byte("hello")) }()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(b, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("got %q, %v", buf, err)
	}
}
