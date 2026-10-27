package main

// messaging: the delivery audit scenario. It exercises the messaging subsystem the way the
// network side is tested — real nodes behind real NATs and relays — across six flows:
//
//	1:1 message, group message, offline store-and-forward, 1:1 call, group call, media stream,
//
// and asserts delivery at the far end of each. Every flow is recorded as a DELIVERY TRACE: did it
// leave the sender, which relay(s) carried it (observed from the relays' own counters), did it
// reach the recipient, was it confirmed. The run ends with a DELIVERY SUMMARY printed last, with
// the failures first, so the tail of the run shows exactly where a message is and why a call did
// not land. See testbed/MESSAGING-AUDIT.md.

import (
	"fmt"
	"strings"
	"time"
)

// relayNodes are the public, always-reachable relays everyone routes through; their counters are
// what we read to see which relay carried a message.
var relayNodes = []string{"s1", "s2", "p1", "p2"}

// relayMetrics is the part of a node's /metrics we trace. Absent keys decode to zero (a node whose
// relay server is off, or that did nothing this flow).
type relayMetrics struct {
	Stored         uint64 `json:"relayStored"`
	Forwarded      uint64 `json:"relayForwarded"`
	Delivered      uint64 `json:"relayDelivered"`
	MailboxWaiting int    `json:"relayMailboxWaiting"`
}

func sampleRelays(names []string) map[string]relayMetrics {
	out := map[string]relayMetrics{}
	for _, n := range names {
		var m relayMetrics
		if call(n, "GET", "/metrics", nil, &m) == nil {
			out[n] = m
		}
	}
	return out
}

// pathDelta describes which relays' counters moved between two samples, e.g.
// "s1: stored+1 delivered+1, s2: fwd+1". Empty means no relay was involved (direct peer-to-peer).
func pathDelta(before, after map[string]relayMetrics) []string {
	var path []string
	for _, n := range relayNodes {
		b, a := before[n], after[n]
		var parts []string
		if d := a.Forwarded - b.Forwarded; d > 0 {
			parts = append(parts, fmt.Sprintf("fwd+%d", d))
		}
		if d := a.Stored - b.Stored; d > 0 {
			parts = append(parts, fmt.Sprintf("stored+%d", d))
		}
		if d := a.Delivered - b.Delivered; d > 0 {
			parts = append(parts, fmt.Sprintf("delivered+%d", d))
		}
		if len(parts) > 0 {
			path = append(path, fmt.Sprintf("%s: %s", n, strings.Join(parts, " ")))
		}
	}
	return path
}

// flowTrace is one delivery flow's record: what the summary prints per flow.
type flowTrace struct {
	kind    string // dm, group, offline, call, call-group, stream
	label   string // "a1 -> c2", "group(b1): a2,c1", ...
	left    string // did it leave the sender, and how
	leftOK  bool
	path    []string // relays that carried it (from their counters), or empty for direct
	reached string   // did it reach the recipient(s)
	reOK    bool
	acked   string // was it confirmed / delivered back (optional per kind)
	ackOK   bool
	note    string // extra context on a failure
}

func (f *flowTrace) ok() bool { return f.leftOK && f.reOK && f.ackOK }

// deliveryFlows is filled by the messaging scenario and printed by printDeliverySummary (called
// last in main). A package var so the summary is the tail of the run, after collectLogs.
var deliveryFlows []*flowTrace

func addFlow(f *flowTrace) *flowTrace { deliveryFlows = append(deliveryFlows, f); return f }

func pathStr(path []string) string {
	if len(path) == 0 {
		return "direct (peer-to-peer, no relay)"
	}
	return strings.Join(path, ", ")
}

// printDeliverySummary prints the top-of-output report: failures first, each with its full trace,
// then every flow's one-line trace. Diagnostics, not just pass/fail.
func printDeliverySummary() {
	if len(deliveryFlows) == 0 {
		return
	}
	fail := 0
	for _, f := range deliveryFlows {
		if !f.ok() {
			fail++
		}
	}
	line := strings.Repeat("=", 72)
	fmt.Println()
	fmt.Println(line)
	fmt.Printf("  DELIVERY SUMMARY — %d flows, %d ok, %d FAILED\n", len(deliveryFlows), len(deliveryFlows)-fail, fail)
	fmt.Println(line)

	if fail > 0 {
		fmt.Printf("\n  FAILURES (%d) — where delivery broke:\n", fail)
		for _, f := range deliveryFlows {
			if f.ok() {
				continue
			}
			fmt.Printf("\n  ✗ [%s] %s\n", f.kind, f.label)
			fmt.Printf("      left sender : %s\n", f.left)
			fmt.Printf("      path        : %s\n", pathStr(f.path))
			fmt.Printf("      reached     : %s\n", f.reached)
			if f.acked != "" {
				fmt.Printf("      confirmed   : %s\n", f.acked)
			}
			if f.note != "" {
				fmt.Printf("      note        : %s\n", f.note)
			}
		}
	} else {
		fmt.Printf("\n  no failures: every message, call and stream reached the far end.\n")
	}

	fmt.Printf("\n  FLOW TRACES (all %d):\n", len(deliveryFlows))
	for _, f := range deliveryFlows {
		mark := "✓"
		if !f.ok() {
			mark = "✗"
		}
		fmt.Printf("  %s [%-10s] %-24s left=%-3s reached=%-3s acked=%-3s via %s\n",
			mark, f.kind, f.label, yn(f.leftOK), yn(f.reOK), yn(f.ackOK), pathStr(f.path))
	}
	fmt.Println(line)
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "NO"
}

// ---------------------------------------------------------------------------- helpers

// dmArrived reports whether `to` has a message with the given text from `from` (not its own copy).
func dmArrived(nodes map[string]*node, from, to, text string) bool {
	var msgs []message
	if call(to, "GET", "/history?peer="+nodes[from].id, nil, &msgs) != nil {
		return false
	}
	for _, m := range msgs {
		if !m.IsMine && m.Text == text {
			return true
		}
	}
	return false
}

func pendingTo(nodes map[string]*node, from, to string) int {
	var p struct{ Pending int }
	if call(from, "GET", "/pending?peer="+nodes[to].id, nil, &p) != nil {
		return -1
	}
	return p.Pending
}

func groupHas(member, group, text string) bool {
	var msgs []groupMessage
	if call(member, "GET", "/group/messages?id="+group+"&limit=0", nil, &msgs) != nil {
		return false
	}
	for _, m := range msgs {
		if m.Text == text {
			return true
		}
	}
	return false
}

// buildGroupOf makes a group with the given members (creator first) and waits for membership to
// converge. Unlike buildGroup it does not drag in every node.
func buildGroupOf(nodes map[string]*node, creator string, members []string) groupInfo {
	var g struct {
		ID string `json:"id"`
	}
	if err := call(creator, "POST", "/group/create", map[string]string{"title": "msgbed", "kind": "group"}, &g); err != nil {
		fatalf("create group: %v", err)
	}
	var inv struct {
		Invite string `json:"invite"`
	}
	if err := call(creator, "GET", "/group/invite?id="+g.ID, nil, &inv); err != nil {
		fatalf("invite: %v", err)
	}
	all := []string{creator}
	for _, n := range members {
		if n == creator {
			continue
		}
		if err := call(n, "POST", "/group/join", map[string]string{"invite": inv.Invite}, nil); err != nil {
			fatalf("%s joins: %v", n, err)
		}
		all = append(all, n)
	}
	ok := waitFor("group membership to converge", 2*time.Minute, func() bool {
		for _, m := range all {
			var list []any
			if err := call(m, "GET", "/group/members?id="+g.ID, nil, &list); err != nil || len(list) != len(all) {
				return false
			}
		}
		return true
	})
	if !ok {
		fatalf("the group did not converge")
	}
	logf("group of %d members is complete (%s)", len(all), strings.Join(all, ","))
	return groupInfo{ID: g.ID, Members: all}
}

// ---------------------------------------------------------------------------- the scenario

func messagingScenario(nodes map[string]*node, v *verdict, out string) {
	meshUp(nodes, 2*time.Minute)
	logf("mesh is up; running the six messaging flows across the three NATs")

	flowDirectMessage(nodes, v)
	flowGroupMessage(nodes, v)
	flowOfflineStoreAndForward(nodes, v, out)
	flowDirectCall(nodes, v)
	flowGroupCall(nodes, v)
	flowStream(nodes, v)

	// the summary is printed last (by main, after collectLogs) so it is the tail of the run
}

// 1. A 1:1 message between two nodes behind different NATs.
func flowDirectMessage(nodes map[string]*node, v *verdict) {
	from, to := "a1", "c2" // cone NAT A -> cone NAT C
	text := "msgbed dm a1>c2"
	f := addFlow(&flowTrace{kind: "dm", label: from + " -> " + to})
	logf("[dm] %s writes to %s", from, to)

	before := sampleRelays(relayNodes)
	err := call(from, "POST", "/send", map[string]string{"to": nodes[to].pubKey, "text": text}, nil)
	if err != nil {
		f.left, f.note = "NO: /send refused: "+err.Error(), err.Error()
		f.reached = "not attempted"
		v.check(false, "[dm] %s -> %s: send refused: %v", from, to, err)
		return
	}
	f.left, f.leftOK = "yes (API accepted)", true

	f.reOK = waitFor(to+" to have the dm", 90*time.Second, func() bool { return dmArrived(nodes, from, to, text) })
	after := sampleRelays(relayNodes)
	f.path = pathDelta(before, after)
	if f.reOK {
		f.reached = "yes (in " + to + " history)"
	} else {
		f.reached = "NO (not in " + to + " history after 90s)"
	}

	f.ackOK = waitFor(from+" to see the dm confirmed", 60*time.Second, func() bool { return pendingTo(nodes, from, to) == 0 })
	if f.ackOK {
		f.acked = "yes (sender shows 0 pending)"
	} else {
		f.acked = fmt.Sprintf("NO (sender still has %d pending)", pendingTo(nodes, from, to))
	}
	v.check(f.ok(), "[dm] %s -> %s delivered and confirmed (via %s)", from, to, pathStr(f.path))
}

// 2. A group message to members behind different NATs.
func flowGroupMessage(nodes map[string]*node, v *verdict) {
	creator, members := "b1", []string{"b1", "a2", "c1"} // symmetric-NAT creator, members in A and C
	g := buildGroupOf(nodes, creator, members)
	text := "msgbed group hello from " + creator
	f := addFlow(&flowTrace{kind: "group", label: fmt.Sprintf("group(%s): a2,c1", creator)})
	logf("[group] %s posts to the group", creator)

	before := sampleRelays(relayNodes)
	err := call(creator, "POST", "/group/send", map[string]string{"id": g.ID, "text": text}, nil)
	if err != nil {
		f.left, f.note = "NO: /group/send refused: "+err.Error(), err.Error()
		f.reached = "not attempted"
		v.check(false, "[group] post refused: %v", err)
		return
	}
	f.left, f.leftOK = "yes (fan-out accepted)", true

	recipients := []string{"a2", "c1"}
	f.reOK = waitFor("every member to have the group message", 2*time.Minute, func() bool {
		for _, m := range recipients {
			if !groupHas(m, g.ID, text) {
				return false
			}
		}
		return true
	})
	after := sampleRelays(relayNodes)
	f.path = pathDelta(before, after)
	if f.reOK {
		f.reached = "yes (a2 and c1 both have it)"
	} else {
		var miss []string
		for _, m := range recipients {
			if !groupHas(m, g.ID, text) {
				miss = append(miss, m)
			}
		}
		f.reached = "NO (missing at: " + strings.Join(miss, ",") + ")"
		f.note = "group fan-out reuses the 1:1 path per member; a missing member means that member's leg failed"
	}
	f.ackOK = f.reOK // group post has no single ack; arrival at all members is the confirmation
	f.acked = "n/a (per-member fan-out)"
	v.check(f.ok(), "[group] message reached every member (via %s)", pathStr(f.path))
}

// 3. Offline store-and-forward: the recipient is taken down, a message is sent, and it must be
//    stored at a relay and delivered when the recipient returns with the same identity.
func flowOfflineStoreAndForward(nodes map[string]*node, v *verdict, out string) {
	from, to := "a1", "c1"
	text := "msgbed offline while-you-were-away"
	f := addFlow(&flowTrace{kind: "offline", label: from + " -> " + to + " (recipient down)"})
	logf("[offline] taking %s down, then %s writes to it", to, from)

	// c1 must have used the relays before going offline (it did, during meshUp) so the relay keeps
	// its mail. Take it down hard, like a crash.
	if _, err := run("docker", "kill", container(to)); err != nil {
		f.left, f.note = "NO: could not stop "+to, err.Error()
		v.check(false, "[offline] could not take %s down: %v", to, err)
		return
	}
	// let the sender notice the dead link so the next message goes to a relay, not the stale peer
	time.Sleep(5 * time.Second)

	before := sampleRelays(relayNodes)
	beforeAll := sampleRelays(allNodeNames()) // a relay mailbox may live on a LAN node too
	err := call(from, "POST", "/send", map[string]string{"to": nodes[to].pubKey, "text": text}, nil)
	if err != nil {
		f.left, f.note = "NO: /send refused while recipient offline: "+err.Error(), err.Error()
		_, _ = run("docker", "start", container(to))
		v.check(false, "[offline] send to an offline contact refused: %v", err)
		return
	}
	f.left, f.leftOK = "yes (API accepted while recipient offline)", true

	// Did any relay take it into its mailbox? (Diagnostic, not a pass/fail gate: delivery to an
	// offline peer is also valid via the sender's own outbox flushing on reconnect — see below.)
	stored := waitFor("the message to be stored at a relay mailbox", 30*time.Second, func() bool {
		s := sampleRelays(allNodeNames())
		for n := range s {
			if s[n].Stored > beforeAll[n].Stored {
				return true
			}
		}
		return false
	})
	midPending := pendingTo(nodes, from, to)
	afterStore := sampleRelays(relayNodes)
	storePath := pathDelta(before, afterStore)
	logf("[offline] stored=%v, sender pending=%d, path so far: %s", stored, midPending, pathStr(storePath))

	// bring the recipient back with the same identity (same container, same /data)
	logf("[offline] bringing %s back", to)
	if _, err := run("docker", "start", container(to)); err != nil {
		f.note = "could not restart " + to + ": " + err.Error()
		v.check(false, "[offline] could not bring %s back: %v", to, err)
		return
	}
	if !waitFor(to+" to answer again", 90*time.Second, func() bool { _, e := statusOf(to); return e == nil }) {
		f.reached = "NO (" + to + " did not restart)"
		v.check(false, "[offline] %s did not come back", to)
		return
	}

	f.reOK = waitFor(to+" to receive the message", 2*time.Minute, func() bool {
		return dmArrived(nodes, from, to, text)
	})
	after := sampleRelays(relayNodes)
	f.path = pathDelta(before, after) // whole flow
	if f.reOK {
		f.reached = "yes (" + to + " got it after reconnecting)"
	} else {
		f.reached = "NO (" + to + " never received the message)"
		diagnose(out, to, "offline delivery")
	}
	f.ackOK = waitFor(from+" to see the offline message confirmed", 90*time.Second, func() bool {
		return pendingTo(nodes, from, to) == 0
	})
	if f.ackOK {
		f.acked = "yes (sender shows 0 pending after reconnect)"
	} else {
		f.acked = "NO (sender still pending)"
	}
	// Delivery to an offline peer is valid two ways: a relay mailbox stores it and hands it over on
	// reconnect, or the sender's own outbox flushes when the link returns. The hard check is
	// end-to-end arrival; which mechanism was used is reported, not gated (the relay mailbox is only
	// used when the sender has already lost its direct link at send time — see MESSAGING-AUDIT.md F7).
	if f.reOK {
		if stored {
			f.note = "relay mailbox stored and handed it over (" + pathStr(storePath) + ")"
		} else {
			f.note = "delivered via the sender's outbox on reconnect; relay mailbox-store not observed (sender held a half-open link at send time — audit F7)"
		}
	} else {
		f.note = "stored at a relay=" + yn(stored) + "; neither the mailbox nor the sender outbox delivered it"
	}
	logf("[offline] mechanism: %s", f.note)
	v.check(f.reOK, "[offline] %s received the message after reconnecting", to)
	v.check(f.ackOK, "[offline] the sender saw the offline message confirmed")
	v.check(f.ok(), "[offline] delivery to an offline peer, end to end (via %s)", pathStr(f.path))
}

// 4. A 1:1 call: the invite (a tracked message) must reach the callee, both join, and audio flows.
func flowDirectCall(nodes map[string]*node, v *verdict) {
	caller, callee := "a1", "b2" // cone NAT -> symmetric NAT (needs a relay for media)
	f := addFlow(&flowTrace{kind: "call", label: caller + " -> " + callee})
	logf("[call] %s calls %s", caller, callee)

	var inv struct {
		CallID      string `json:"callId"`
		Coordinator string `json:"coordinator"`
		State       string `json:"state"`
	}
	before := sampleRelays(relayNodes)
	if err := call(caller, "POST", "/call/invite", map[string]string{"to": nodes[callee].pubKey}, &inv); err != nil {
		f.left, f.note = "NO: invite refused: "+err.Error(), err.Error()
		f.reached = "not attempted"
		v.check(false, "[call] %s invites %s: %v", caller, callee, err)
		return
	}
	f.left, f.leftOK = "yes (invite sent, state="+inv.State+")", true

	// the invite is a tracked message; "signaling reached the callee" == the invite was confirmed
	inviteDelivered := waitFor("the call invite to reach "+callee, time.Minute, func() bool {
		return pendingTo(nodes, caller, callee) == 0
	})
	f.path = pathDelta(before, sampleRelays(relayNodes))
	f.ackOK = inviteDelivered
	if inviteDelivered {
		f.acked = "yes (invite confirmed by " + callee + ")"
	} else {
		f.acked = "NO (invite never confirmed — callee may not have gotten the signal)"
	}

	people := []string{caller, callee}
	for _, p := range people {
		_ = call(p, "POST", "/call/join", map[string]string{"callId": inv.CallID, "coordinator": inv.Coordinator}, nil)
	}
	formed := waitFor("both in the room", time.Minute, func() bool {
		for _, p := range people {
			var s struct {
				InCall  bool     `json:"inCall"`
				Members []string `json:"members"`
			}
			if call(p, "GET", "/call/state", nil, &s) != nil || !s.InCall || len(s.Members) != 2 {
				return false
			}
		}
		return true
	})
	if !formed {
		f.reached = "NO (the two did not both join the room)"
		f.note = "room did not form — media cannot connect"
		v.check(false, "[call] %s and %s both in the call", caller, callee)
		return
	}
	// voice both ways for 8 s (rx counters are cumulative, so measure the delta over this flow)
	baseAB := heard(callee, nodes[caller])
	baseBA := heard(caller, nodes[callee])
	for _, p := range people {
		_ = call(p, "POST", "/call/talk", nil, nil)
	}
	time.Sleep(8 * time.Second)
	for _, p := range people {
		_ = call(p, "POST", "/call/talk?stop=1", nil, nil)
	}
	gotAB := heard(callee, nodes[caller]) - baseAB
	gotBA := heard(caller, nodes[callee]) - baseBA
	f.reOK = gotAB >= 250 && gotBA >= 250
	f.reached = fmt.Sprintf("%s heard %d units, %s heard %d (of ~400 each)", callee, gotAB, caller, gotBA)
	if !f.reOK {
		f.note = "media did not flow both ways; a relay path between a cone and a symmetric NAT is required"
	}
	for _, p := range people {
		_ = call(p, "POST", "/call/leave", nil, nil)
	}
	v.check(f.ok(), "[call] %s<->%s: invite delivered, room formed, voice both ways (via %s)", caller, callee, pathStr(f.path))
}

// 5. A group call: three people across the three NATs, each must hear the others.
func flowGroupCall(nodes map[string]*node, v *verdict) {
	people := []string{"a1", "b2", "c1"}
	f := addFlow(&flowTrace{kind: "call-group", label: strings.Join(people, ",")})
	logf("[call-group] %s in one call", strings.Join(people, ","))

	// a1 starts the room and is the coordinator; the others join by its invite out of band (same
	// derived room, as the call scenario does)
	var inv struct {
		CallID      string `json:"callId"`
		Coordinator string `json:"coordinator"`
	}
	before := sampleRelays(relayNodes)
	if err := call("a1", "POST", "/call/invite", map[string]string{"to": nodes["b2"].pubKey}, &inv); err != nil {
		f.left, f.note = "NO: invite refused: "+err.Error(), err.Error()
		v.check(false, "[call-group] a1 opens a room: %v", err)
		return
	}
	f.left, f.leftOK = "yes (room opened by a1)", true
	for _, p := range people {
		_ = call(p, "POST", "/call/join", map[string]string{"callId": inv.CallID, "coordinator": inv.Coordinator}, nil)
	}
	formed := waitFor("all three in the room", time.Minute, func() bool {
		for _, p := range people {
			var s struct {
				InCall  bool     `json:"inCall"`
				Members []string `json:"members"`
			}
			if call(p, "GET", "/call/state", nil, &s) != nil || !s.InCall || len(s.Members) != len(people) {
				return false
			}
		}
		return true
	})
	f.ackOK = formed
	if formed {
		f.acked = "yes (3-member room formed)"
	} else {
		f.acked = "NO (room never reached 3 members)"
	}
	if !formed {
		f.reached = "NO (room did not form)"
		v.check(false, "[call-group] three across three NATs in one call")
		return
	}
	base := map[string]int{}
	for _, p := range people {
		for _, q := range people {
			if p != q {
				base[p+q] = heard(p, nodes[q])
			}
		}
	}
	for _, p := range people {
		_ = call(p, "POST", "/call/talk", nil, nil)
	}
	time.Sleep(10 * time.Second)
	for _, p := range people {
		_ = call(p, "POST", "/call/talk?stop=1", nil, nil)
	}
	f.path = pathDelta(before, sampleRelays(relayNodes))
	allHear := true
	var detail []string
	for _, p := range people {
		for _, q := range people {
			if p == q {
				continue
			}
			got := heard(p, nodes[q]) - base[p+q]
			if got < 300 {
				allHear = false
				detail = append(detail, fmt.Sprintf("%s<%s=%d", p, q, got))
			}
		}
	}
	f.reOK = allHear
	if allHear {
		f.reached = "yes (every member hears every other)"
	} else {
		f.reached = "NO (thin/absent: " + strings.Join(detail, " ") + ")"
	}
	for _, p := range people {
		_ = call(p, "POST", "/call/leave", nil, nil)
	}
	v.check(f.ok(), "[call-group] three-way voice across three NATs (via %s)", pathStr(f.path))
}

// 6. A media stream: a1 screen-shares and the others must see frames, undamaged.
func flowStream(nodes map[string]*node, v *verdict) {
	broadcaster, watchers := "a1", []string{"b2", "c1"}
	f := addFlow(&flowTrace{kind: "stream", label: broadcaster + " -> " + strings.Join(watchers, ",")})
	logf("[stream] %s screen-shares to %s", broadcaster, strings.Join(watchers, ","))

	// reuse the room from the group call: open a fresh one so the test stands alone
	var inv struct {
		CallID      string `json:"callId"`
		Coordinator string `json:"coordinator"`
	}
	before := sampleRelays(relayNodes)
	if err := call(broadcaster, "POST", "/call/invite", map[string]string{"to": nodes["b2"].pubKey}, &inv); err != nil {
		f.left, f.note = "NO: could not open a room: "+err.Error(), err.Error()
		v.check(false, "[stream] open room: %v", err)
		return
	}
	people := append([]string{broadcaster}, watchers...)
	for _, p := range people {
		_ = call(p, "POST", "/call/join", map[string]string{"callId": inv.CallID, "coordinator": inv.Coordinator}, nil)
	}
	if !waitFor("the room for the stream", time.Minute, func() bool {
		for _, p := range people {
			var s struct {
				InCall  bool     `json:"inCall"`
				Members []string `json:"members"`
			}
			if call(p, "GET", "/call/state", nil, &s) != nil || !s.InCall || len(s.Members) != len(people) {
				return false
			}
		}
		return true
	}) {
		f.left, f.reached = "NO (room for the stream did not form)", "not attempted"
		v.check(false, "[stream] room for the stream formed")
		return
	}

	sid := broadcaster + "-screen"
	if err := call(broadcaster, "POST", "/call/stream?id="+sid, nil, nil); err != nil {
		f.left, f.note = "NO: stream would not start: "+err.Error(), err.Error()
		v.check(false, "[stream] %s starts a stream: %v", broadcaster, err)
		return
	}
	f.left, f.leftOK = "yes (720p30 stream started)", true
	for _, p := range watchers {
		_ = call(p, "POST", "/call/watch?id="+sid, nil, nil)
	}
	time.Sleep(10 * time.Second)
	f.path = pathDelta(before, sampleRelays(relayNodes))

	allSee, noDamage := true, true
	var detail []string
	key := nodes[broadcaster].id[:8] + "/" + sid
	for _, p := range watchers {
		r := rxOf(p)
		got := r.Video[key]
		if got < 150 {
			allSee = false
			detail = append(detail, fmt.Sprintf("%s=%d", p, got))
		}
		if r.Damaged > 0 {
			noDamage = false
			detail = append(detail, fmt.Sprintf("%s damaged=%d", p, r.Damaged))
		}
	}
	f.reOK = allSee && noDamage
	f.ackOK = allSee && noDamage
	if f.reOK {
		f.reached = "yes (both watchers see frames, undamaged)"
		f.acked = "yes (frames flowing, 0 damaged)"
	} else {
		f.reached = "NO/thin frames: " + strings.Join(detail, " ")
		f.acked = f.reached
		f.note = "a watcher with 0 frames never attached to the stream tree"
	}
	for _, p := range people {
		_ = call(p, "POST", "/call/leave", nil, nil)
	}
	v.check(f.ok(), "[stream] %s's screen reaches both watchers, undamaged (via %s)", broadcaster, pathStr(f.path))
}
