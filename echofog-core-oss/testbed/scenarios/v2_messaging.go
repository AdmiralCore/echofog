package scenarios

import (
	"context"
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

	res.Rows = append(res.Rows, offlineQueueRow(ctx, dir, env))
	res.NotChecked = []string{
		"a mailbox: a message for a recipient that is away is deposited at a mailbox server; the world has no mailbox server and no DHT, so the outbox's own waiting is what is measured",
		"restart of a node (kill -9 and start on the same store) and the group chat: need several processes, the stand",
		"the directory: the two chat services share the lab's directory (device list and prekeys of a node of the world) instead of the DHT, whose lookup stays inconclusive on loopback (tried: two and five nodes, AllowNonRoutable, the clock moved past the republication); the DHT, the card/prekeys publication and the bundle fetch are not exercised",
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
