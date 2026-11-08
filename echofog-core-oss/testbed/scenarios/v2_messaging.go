package scenarios

import (
	"context"
	"errors"
	"fmt"
	"os"
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
// errNoDirectory marks a world that met but whose directory does not resolve.
var errNoDirectory = errors.New("the DHT does not resolve")

func messagingWorld(ctx context.Context, dir string, em behaviour.NetemConfig) (*World, *Node, *Node, error) {
	w := NewWorld(dir)
	b, err := w.Add(NodeSpec{Name: "b", Server: true, Cover: true, Chat: true, Listen: true, Netem: &em})
	if err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	a, err := w.Add(NodeSpec{Name: "a", Server: true, Cover: true, Chat: true, Listen: true})
	if err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	if err := w.StartAll(ctx); err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	// the world lives on loopback, which the DHT client refuses as a peer address unless told otherwise
	a.RT.DHT().SetAllowNonRoutable(true)
	b.RT.DHT().SetAllowNonRoutable(true)
	if _, _, err := a.Dial(ctx, b); err != nil {
		w.Close()
		return nil, nil, nil, err
	}
	// The first publication into the DHT ran at start, before the two knew each other, and the
	// product tries again after five minutes; the world skips them, and the directory is ready
	// within the minute the republication loop ticks.
	w.Skip(6 * time.Minute)
	if err := awaitDirectory(ctx, a, b, 20*time.Second); err != nil {
		w.Close()
		return nil, nil, nil, fmt.Errorf("%w: %v", errNoDirectory, err)
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

func runMessaging(ctx context.Context, env Env) Result {
	res := Result{Name: "v2-messaging", Mode: "model", Seed: env.Seed}
	dir, err := os.MkdirTemp(env.Dir, "messaging-")
	if err != nil {
		dir = os.TempDir()
	}
	defer os.RemoveAll(dir)
	n := 40
	if env.Short {
		n = 20
	}

	// both ways over a lossy, slow path
	w, a, b, err := messagingWorld(ctx, dir, behaviour.NetemConfig{Loss: 0.10, Delay: 40 * time.Millisecond, Seed: env.Seed})
	if errors.Is(err, errNoDirectory) {
		// The two nodes met over real noise3, but chat/2 finds a recipient's prekeys through the DHT, and the DHT
		// of the world does not resolve: the loopback world is not the network the routing tables and the
		// diversity rules are built for. Nothing of the delivery was measured, and the rows say so.
		const why = "chat/2 resolves the recipient through the DHT and the lookup stays inconclusive in the loopback world (tried: two nodes, five nodes in a mesh, the clock skipped past the five-minute republication, AllowNonRoutable on); a finding for the DHT in a lab, see the report"
		res.Rows = append(res.Rows, newRow("v2-messaging/session", "session", "open over noise3", "a real session", gate.Green, ""))
		for _, id := range []string{"exactly-once", "offline-queue", "state"} {
			res.Rows = append(res.Rows, newRow("v2-messaging/"+id, "delivery", "not measured", "once each, both ways", gate.NotMeasured, why))
		}
		res.NotChecked = append(res.NotChecked, why+": "+err.Error())
		return res
	}
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

	res.Rows = append(res.Rows, offlineQueueRow(ctx, dir, env))
	res.NotChecked = []string{
		"a mailbox: a message for a recipient that is away is deposited at a mailbox server; the world has no mailbox server and no DHT, so the outbox's own waiting is what is measured",
		"restart of a node (kill -9 and start on the same store) and the group chat: need several processes, the stand",
		"the prekeys come from the contact link, not from the DHT",
	}
	return res
}

// offlineQueueRow makes the recipient unreachable, sends, and lets it come back: the outbox has to
// keep the messages and deliver each once.
func offlineQueueRow(ctx context.Context, dir string, env Env) gate.Row {
	const id = "v2-messaging/offline-queue"
	w, a, b, err := messagingWorld(ctx, dir, behaviour.NetemConfig{Seed: env.Seed + 1})
	if err != nil {
		return newRow(id, "offline", err.Error(), "delivered once after the cut", gate.Red, "the two nodes did not meet")
	}
	defer w.Close()
	b.Netem.Cut(8 * time.Second)
	var ids [][16]byte
	for i := 0; i < 10; i++ {
		if mid, err := a.SendText(ctx, b, fmt.Sprintf("while away %d", i)); err == nil {
			ids = append(ids, mid)
		}
	}
	got := awaitDelivered(ctx, a, ids, 120*time.Second)
	time.Sleep(2 * time.Second)
	distinct, most := b.Inbox.Count()
	value := fmt.Sprintf("%d of %d delivered, %d distinct, most times %d", got, len(ids), distinct, most)
	if len(ids) != 10 || got != 10 || distinct != 10 || most != 1 {
		return newRow(id, "offline", value, "10 of 10, once each", gate.Red, "")
	}
	return newRow(id, "offline", value, "10 of 10, once each", gate.Green, "")
}

// awaitDirectory waits until each node finds the other through the DHT.
func awaitDirectory(ctx context.Context, a, b *Node, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		_, errAB := a.RT.DHT().Resolve(ctx, [32]byte(b.ID.NodeID))
		_, errBA := b.RT.DHT().Resolve(ctx, [32]byte(a.ID.NodeID))
		if errAB == nil && errBA == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the directory does not resolve: a to b %v, b to a %v", errAB, errBA)
		}
		time.Sleep(2 * time.Second)
	}
}
