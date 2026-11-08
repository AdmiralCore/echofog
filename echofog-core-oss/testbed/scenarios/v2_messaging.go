package scenarios

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/chat"
	"github.com/echofog/echofog-core/testbed/behaviour"
	"github.com/echofog/echofog-core/testbed/gate"
)

func messagingScenario() Scenario {
	return Scenario{Name: "v2-messaging", Title: "Messaging: every accepted message arrives exactly once", Live: liveMessaging, Run: runMessaging}
}

// messagingWorld is two chat nodes; b listens behind a netem with loss and delay. They are linked
// by a contact link.
func messagingWorld(ctx context.Context, dir string, em behaviour.NetemConfig) (*World, *Node, *Node, error) {
	w := NewWorld(dir)
	b, err := w.Add(NodeSpec{Name: "b", Server: true, LabChat: true, Listen: true, Netem: &em})
	if err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	a, err := w.Add(NodeSpec{Name: "a", Server: true, LabChat: true, Listen: true})
	if err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	if err := w.StartAll(ctx); err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	if _, _, err := a.Dial(ctx, b); err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	return w, a, b, nil
}

// awaitDelivered waits until the sender sees every message as delivered, or the time is up.
func awaitDelivered(ctx context.Context, from *Node, ids [][16]byte, within time.Duration) (delivered int) {
	deadline := time.Now().Add(within)
	for {
		delivered = 0
		for _, id := range ids {
			if from.Inbox.State(id) == chat.StateDelivered {
				delivered++
			}
		}
		if delivered == len(ids) || time.Now().After(deadline) || ctx.Err() != nil {
			return delivered
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func runMessaging(ctx context.Context, env Env) (res Result) {
	res = Result{Name: "v2-messaging", Mode: "model", Seed: env.Seed}
	dir, err := os.MkdirTemp(env.Dir, "messaging-")
	if err != nil {
		dir = os.TempDir()
	}
	defer scratchDone(dir, &res)
	n := 40
	if env.Short {
		n = 20
	}

	// both ways over a lossy, slow path
	w, a, b, err := messagingWorld(ctx, dir, behaviour.NetemConfig{Loss: 0.10, Delay: 40 * time.Millisecond, Seed: env.Seed})
	if err != nil {
		res.Rows = append(res.Rows, newRow("v2-messaging/session", "session", err.Error(), "a real session", gate.Red, "the two nodes did not meet"))
		return res
	}
	res.Rows = append(res.Rows, newRow("v2-messaging/session", "session", "session open", "a real session", gate.Green, ""))
	var ab, ba [][16]byte
	for i := 0; i < n; i++ {
		if id, err := a.SendText(ctx, b, fmt.Sprintf("a to b %d", i)); err == nil {
			ab = append(ab, id)
		}
		if id, err := b.SendText(ctx, a, fmt.Sprintf("b to a %d", i)); err == nil {
			ba = append(ba, id)
		}
	}
	gotAB := awaitDelivered(ctx, a, ab, 90*time.Second)
	gotBA := awaitDelivered(ctx, b, ba, 90*time.Second)
	time.Sleep(2 * time.Second) // a duplicate would arrive after the confirmation
	bd, bmax := b.Inbox.Count()
	ad, amax := a.Inbox.Count()
	once := gate.Green
	if bd != n || ad != n || bmax != 1 || amax != 1 || len(ab) != n || len(ba) != n {
		once = gate.Red
	}
	res.Rows = append(res.Rows, newRow("v2-messaging/exactly-once", "messages arrived / most times one arrived",
		fmt.Sprintf("a to b %d of %d, b to a %d of %d, most times %d", bd, n, ad, n, max(amax, bmax)), fmt.Sprintf("%d each way, once each", n), once, ""))
	st := gate.Green
	if gotAB != len(ab) || gotBA != len(ba) {
		st = gate.Red
	}
	res.Rows = append(res.Rows, newRow("v2-messaging/state", "sender sees delivered",
		fmt.Sprintf("a %d of %d, b %d of %d", gotAB, len(ab), gotBA, len(ba)), "all delivered", st, ""))
	w.Close()

	orow, ofind := offlineQueueRow(ctx, dir, env)
	res.Rows = append(res.Rows, orow)
	if ofind != "" {
		res.Findings = append(res.Findings, ofind)
	}
	res.NotChecked = []string{
		"a mailbox: a message for a recipient that is away is deposited at a mailbox server; the world has no mailbox server and no DHT, so the outbox's own waiting is what is measured",
		"restart of a node (kill -9 and start on the same store) and the group chat: need several processes, the stand",
		"the directory: the two chat services share the lab's directory (device list and prekeys of a node of the world) instead of the DHT, whose lookup stays inconclusive on loopback (tried: two and five nodes, AllowNonRoutable, the clock moved past the republication); the DHT, the card/prekeys publication and the bundle fetch are not exercised",
	}
	return res
}

// offlineQueueRow makes the recipient unreachable, sends, and lets it come back: the outbox has to
// keep the messages and deliver each once, within offlineLimit of the cut. When it does not, the row
// keeps watching (up to offlineWatch) to say when the sessions went and came back, so the finding
// carries the time a user would have waited.
func offlineQueueRow(ctx context.Context, dir string, env Env) (gate.Row, string) {
	const id = "v2-messaging/offline-queue"
	const limit = "10 of 10 within 2 minutes of an 8 s cut, once each"
	w, a, b, err := messagingWorld(ctx, dir, behaviour.NetemConfig{Seed: env.Seed + 1})
	if err != nil {
		return newRow(id, "offline", err.Error(), limit, gate.Red, "the two nodes did not meet"), ""
	}
	defer w.Close()
	start := time.Now()
	b.Netem.Cut(8 * time.Second)
	var ids [][16]byte
	for i := 0; i < 10; i++ {
		if mid, err := a.SendText(ctx, b, fmt.Sprintf("while away %d", i)); err == nil {
			ids = append(ids, mid)
		}
	}
	var samples []recoverySample
	delivered := func() int { return awaitDelivered(ctx, a, ids, 0) }
	var at time.Duration
	for ctx.Err() == nil {
		got := delivered()
		at = time.Since(start)
		samples = append(samples, recoverySample{At: at, Sender: len(a.RT.Status().Sessions), Receiver: len(b.RT.Status().Sessions), Delivered: got})
		if (got == len(ids) && len(ids) > 0) || at > offlineWatchFor(env) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	time.Sleep(2 * time.Second) // a duplicate would arrive after the confirmation
	got := samples[len(samples)-1].Delivered
	distinct, most := b.Inbox.Count()
	value := fmt.Sprintf("%d of %d delivered at %ds, %d distinct, most times %d", got, len(ids), int(at.Seconds()), distinct, most)
	if len(ids) == 10 && got == 10 && distinct == 10 && most == 1 && at <= offlineLimit {
		return newRow(id, "offline", value, limit, gate.Green, ""), ""
	}
	why := offlineWhy(a, b, ids)
	pctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	_, _, derr := a.Dial(pctx, b)
	stop()
	if derr != nil {
		why += "; explicit re-dial after the verdict: " + derr.Error()
	} else {
		why += "; explicit re-dial after the verdict: ok"
	}
	timeline := describeRecovery(samples, 10)
	finding := "after an 8 s cut of the only path the chat outbox did not deliver within 2 minutes (" + timeline + "). In diagnostic runs under a loaded scheduler (GOMAXPROCS=2 and other work) the receiver dropped its session during the cut, the sender kept a session whose paths were gone for about two minutes and the outbox reused it, and a new dial waited for the pair backoff (H15) that failed attempts during the cut started; the messages came 7 to 8 minutes after an outage of 8 seconds. On a free machine the session survives the cut and the row is green. A finding of the session and path layer (the 10 s rule of a failing path, the session kept without a path, the backoff after a transient failure), not of the scenario"
	return newRow(id, "offline", value, limit, gate.Red, timeline+"; "+why), finding
}

// offlineWatchFor is how long the row watches after the limit.
func offlineWatchFor(env Env) time.Duration {
	if env.Short {
		return offlineWatchShort
	}
	return offlineWatch
}

const (
	offlineLimit = 2 * time.Minute
	offlineWatch = 9 * time.Minute
	// offlineWatchShort keeps go test inside its default ten minutes when the row is red.
	offlineWatchShort = 3 * time.Minute
)

// offlineWhy says what the two ends knew when the messages did not arrive: every session with its
// paths and idle time, the counters of the lab path, the state of every message (1 waiting, 2 in a
// mailbox, 3 delivered, 4 failed) and the last events of both runtimes.
func offlineWhy(a, b *Node, ids [][16]byte) string {
	states := map[chat.State]int{}
	for _, id := range ids {
		states[a.Inbox.State(id)]++
	}
	sessions := func(n *Node) string {
		var parts []string
		for _, s := range n.RT.Status().Sessions {
			parts = append(parts, fmt.Sprintf("{%s paths %v active %d idle %s going-away %v}", s.Remote, s.Paths, s.ActivePath,
				time.Since(s.LastActivity).Round(time.Second), s.GoingAway))
		}
		return fmt.Sprintf("%d %s", len(parts), strings.Join(parts, " "))
	}
	em := "no netem"
	if b.Netem != nil {
		st := b.Netem.Stats()
		em = fmt.Sprintf("netem forwarded %d dropped %d", st.Forwarded, st.Dropped)
	}
	return fmt.Sprintf("sender sessions %s; receiver sessions %s; %s; message states %v; sender events %q; receiver events %q",
		sessions(a), sessions(b), em, states, a.EventTail(8), b.EventTail(8))
}

// recoverySample is what the two ends knew at a moment after the cut.
type recoverySample struct {
	At               time.Duration
	Sender, Receiver int // sessions
	Delivered        int
}

// describeRecovery turns the samples into the timeline of a recovery that took too long.
func describeRecovery(samples []recoverySample, total int) string {
	var parts []string
	prevS, prevR := -1, -1
	bothBack := false
	for _, s := range samples {
		if prevR > 0 && s.Receiver == 0 {
			parts = append(parts, fmt.Sprintf("receiver lost its session at %ds", int(s.At.Seconds())))
		}
		if prevS > 0 && s.Sender == 0 {
			parts = append(parts, fmt.Sprintf("sender lost its session at %ds", int(s.At.Seconds())))
		}
		if !bothBack && s.Sender > 0 && s.Receiver > 0 && (prevS == 0 || prevR == 0) {
			parts = append(parts, fmt.Sprintf("both sessions back at %ds", int(s.At.Seconds())))
			bothBack = true
		}
		if s.Delivered == total {
			parts = append(parts, fmt.Sprintf("all %d delivered at %ds", total, int(s.At.Seconds())))
			return strings.Join(parts, "; ")
		}
		prevS, prevR = s.Sender, s.Receiver
	}
	last := time.Duration(0)
	if n := len(samples); n > 0 {
		last = samples[n-1].At
	}
	parts = append(parts, fmt.Sprintf("not delivered by %ds", int(last.Seconds())))
	return strings.Join(parts, "; ")
}
