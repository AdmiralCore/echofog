package node_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
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
	// the control, in a world of its own (an open session is reused, and only new paths are recalculated):
	// without the mark a wired client dials the foreign address itself
	c := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	c.a.rt.SetNetworkID("lab-wan")
	_, _ = c.open(t, node.DialOpt{}) // the direct attempt fails (the fake suites are unreachable) and the relay takes over
	if foreignDials(c) == 0 {
		t.Fatal("before the mark a wired client dials the foreign address itself")
	}

	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true})
	freezeNetwork(t, w.a.rt)
	p, err := w.open(t, node.DialOpt{})
	if err != nil {
		t.Fatalf("Open through the entry: %v", err)
	}
	if p.Path != mux.PathRelay {
		t.Fatalf("path %v, want the relay path through the home entry", p.Path)
	}
	if n := foreignDials(w); n != 0 {
		t.Fatalf("%d direct dials to the foreign address after the mark, want none", n)
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
	if !strings.Contains(ev.Message, "замораж") {
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

// M1 of the T6.4d acceptance: the watcher is on the real dial path. A fake carrier suite calls the
// DialContext it was given, as a real one does; a socket to a foreign address gives 16 KiB and goes
// quiet once the node has written. Returning r.cfg.DialContext in dial.go and paths.go turns this red.
func TestTheWatcherIsOnTheRealDialPathAndMarksTheNetwork(t *testing.T) {
	var w *routeWorld
	stalledFlows := 0
	// one direct attempt of the plan: the flow, and its repeat to the same address
	one := func(req carrier.DialRequest) {
		conn, err := req.DialContext(context.Background(), "tcp", "203.0.113.5:443")
		if err != nil {
			t.Errorf("DialContext: %v", err)
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		for got := 0; got < 16<<10; {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			got += n
		}
		_, _ = conn.Write([]byte("request"))
		time.Sleep(200 * time.Millisecond) // four times the stall: the watcher has spoken
		stalledFlows++
	}
	hook := func(req carrier.DialRequest) {
		if req.DialContext == nil {
			t.Error("a dial request without a DialContext")
			return
		}
		one(req)
		one(req)
	}
	socket := func(ctx context.Context, network, address string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			_, _ = b.Write(make([]byte, 16<<10))
			_, _ = io.Copy(io.Discard, b) // the freeze: the node writes, nothing comes back
		}()
		return a, nil
	}
	w = newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true,
		hook: hook, dialCtx: socket, freezeStall: 50 * time.Millisecond})
	w.a.rt.SetNetworkID("lab-wan")
	if node.IsFrozen(w.a.rt) {
		t.Fatal("not frozen before any dial")
	}
	p, err := w.open(t, node.DialOpt{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if stalledFlows < 2 {
		t.Fatalf("only %d direct flows were tried: the scenario needs the flow and its repeat", stalledFlows)
	}
	if !node.IsFrozen(w.a.rt) {
		t.Fatal("two flows to the foreign address stopped at 16 KiB through the real dial path: the network must be marked")
	}
	if !w.events.has(node.EventNetworkFrozen) {
		t.Fatal("the application is told")
	}
	if p.Path != mux.PathRelay {
		t.Fatalf("path %v, want the relay path through the home entry", p.Path)
	}
	rep, ok := w.a.rt.RouteReport(w.b.id.DeviceID)
	if !ok || rep.RuleID != "frozen-foreign" || !rep.ViaEntry {
		t.Fatalf("report %+v: the new path goes by the frozen row", rep)
	}
}

// M2 (a): only a flow younger than the limit counts: the handshake and the first data, not a long
// request on an old connection.
func TestAFlowOlderThanTheLimitIsNotTheFreeze(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true,
		freezeStall: 50 * time.Millisecond, freezeFlowAge: 150 * time.Millisecond})
	w.a.rt.SetNetworkID("n1")
	flow := func(waitBefore time.Duration) {
		t.Helper()
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		c := node.WrapFreeze(w.a.rt, a, foreignIP)
		go func() { _, _ = io.Copy(io.Discard, b) }()
		go func() { _, _ = b.Write(make([]byte, 16<<10)) }()
		for got, buf := 0, make([]byte, 4096); got < 16<<10; {
			n, err := c.Read(buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			got += n
		}
		time.Sleep(waitBefore) // the connection ages before it writes
		_, _ = c.Write([]byte("request"))
		time.Sleep(250 * time.Millisecond)
	}
	flow(250 * time.Millisecond)
	flow(250 * time.Millisecond)
	if node.IsFrozen(w.a.rt) {
		t.Fatal("two old flows that stopped are two slow requests, not the freeze")
	}
	flow(0)
	flow(0)
	if !node.IsFrozen(w.a.rt) {
		t.Fatal("two young flows that stopped at 16 KiB are the freeze")
	}
}

// M2 (b), the hostile server: 16 KiB and silence, twice, while an honest flow to the same address is
// being answered. No mark.
func TestAHostileServerWithAnHonestSecondFlowDoesNotMarkTheNetwork(t *testing.T) {
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true,
		freezeStall: 50 * time.Millisecond})
	w.a.rt.SetNetworkID("n1")

	// the honest flow: the same address keeps answering on it
	ha, hb := net.Pipe()
	defer ha.Close()
	defer hb.Close()
	honest := node.WrapFreeze(w.a.rt, ha, foreignIP)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(15 * time.Millisecond):
				_, _ = hb.Write([]byte("fine"))
			}
		}
	}()
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := honest.Read(buf); err != nil {
				return
			}
		}
	}()

	hostile := func() {
		t.Helper()
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		c := node.WrapFreeze(w.a.rt, a, foreignIP)
		go func() { _, _ = io.Copy(io.Discard, b) }()
		go func() { _, _ = b.Write(make([]byte, 16<<10)) }()
		for got, buf := 0, make([]byte, 4096); got < 16<<10; {
			n, err := c.Read(buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			got += n
		}
		_, _ = c.Write([]byte("request"))
		time.Sleep(250 * time.Millisecond)
	}
	hostile()
	hostile()
	if node.IsFrozen(w.a.rt) {
		t.Fatal("a server that gives 16 KiB and goes quiet on two flows while it answers a third must not freeze the network")
	}
}

// M2 (c): with no verified entry the mark costs the direct foreign paths and buys nothing, so it
// lasts an hour; with one, a day.
func TestTheMarkIsAnHourWithoutAVerifiedEntryAndADayWithOne(t *testing.T) {
	check := func(name string, o routeOpts, want time.Duration) {
		t.Helper()
		w := newRouteWorld(t, o)
		freezeNetwork(t, w.a.rt)
		until := node.FrozenUntil(w.a.rt)
		d := time.Until(until)
		if d < want-time.Minute || d > want+time.Minute {
			t.Errorf("%s: the mark ends in %v, want about %v", name, d.Round(time.Second), want)
		}
	}
	check("a verified entry", routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true}, 24*time.Hour)
	check("no verified entry", routeOpts{net: rp.NetWired, bCover: true}, time.Hour)
}

// The second place the watcher is wired: a new path of a live session (AddPath). The session is
// opened first, with the scenario off; then AddPath makes the fake suite dial through the
// DialContext it was given, twice, and the sockets go quiet. Returning r.cfg.DialContext in paths.go
// turns this red.
func TestTheWatcherIsOnTheNewPathDialToo(t *testing.T) {
	var active atomic.Bool
	hook := func(req carrier.DialRequest) {
		if !active.Load() {
			return
		}
		for i := 0; i < 2; i++ {
			conn, err := req.DialContext(context.Background(), "tcp", "203.0.113.5:443")
			if err != nil {
				t.Errorf("DialContext: %v", err)
				return
			}
			buf := make([]byte, 4096)
			for got := 0; got < 16<<10; {
				n, err := conn.Read(buf)
				if err != nil {
					break
				}
				got += n
			}
			_, _ = conn.Write([]byte("request"))
			time.Sleep(200 * time.Millisecond)
			conn.Close()
		}
	}
	socket := func(ctx context.Context, network, address string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			_, _ = b.Write(make([]byte, 16<<10))
			_, _ = io.Copy(io.Discard, b)
		}()
		return a, nil
	}
	w := newRouteWorld(t, routeOpts{net: rp.NetWired, rules: entryRules(t, "198.51.100.0/24"), bCover: true,
		features: true, hook: hook, dialCtx: socket, freezeStall: 50 * time.Millisecond})
	// the session is opened under a mark, so no direct dial happens and no pair is backed off; then
	// the client moves to another network, where the mark is gone and a direct path is allowed
	freezeNetwork(t, w.a.rt)
	if _, err := w.open(t, node.DialOpt{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	w.a.rt.SetNetworkID("other-wan")
	if node.IsFrozen(w.a.rt) {
		t.Fatal("not frozen before the new path is dialled")
	}
	active.Store(true)
	_, perr := w.a.rt.AddPath(w.ctx, w.b.id.DeviceID, node.DialOpt{Card: w.b.peerCard(t)}) // the fake suites fail; the sockets are what counts
	t.Logf("AddPath: %v", perr)
	if !node.IsFrozen(w.a.rt) {
		t.Fatal("two flows of a new path stopped at 16 KiB: the network must be marked")
	}
}
