package node_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// blockPinger keeps every liveness check in flight until its context ends.
type blockPinger struct{ started chan struct{} }

func (p blockPinger) Ping(ctx context.Context, _ *objects.Card) error {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestRoutingTableDoesNotBlockSessionAcceptance is the T5.1b-dht200 inheritance: the paths that
// feed the routing table from authenticated sessions (AddFromSession, noteSession) must not wait
// for the liveness check of a full bucket, which can last 10 s.
func TestRoutingTableDoesNotBlockSessionAcceptance(t *testing.T) {
	nd := newTestNode(t, cards.Visible, true, nil, nil)
	nd.start(t)
	tbl := nd.rt.RoutingTable()
	if tbl == nil {
		t.Fatal("no routing table")
	}
	bp := blockPinger{started: make(chan struct{}, 1)}
	tbl.SetPinger(bp)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// About half of all devices fall into the bucket of the farthest half: 120 cards fill it
		// for certain, and the next ones ask for a check.
		for i := 0; i < 120; i++ {
			c := thirdPartyCard(t, fmt.Sprintf("bucket-filler-%d", i), fmt.Sprintf("11.%d.%d.1", i/250, i%250), 4430)
			tbl.AddFromSession(c)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("feeding the table from sessions blocked on a liveness check")
	}
	select {
	case <-bp.started:
	default:
		t.Fatal("no liveness check was asked for: the test did not fill a bucket")
	}
}

// TestDHTClientOverRuntimes runs the lookup client of real runtimes (dht/2 over memory
// carriers): a record put by one node is found by another, and Host.DHT() is wired.
func TestDHTClientOverRuntimes(t *testing.T) {
	if runtime.GOOS == "windows" && os.Getenv("ECHOFOG_DHT_NET") == "" {
		t.Skip("many runtimes in one Windows test binary hang in memguard; run in WSL/Linux")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	const N = 24
	rnd := rand.New(rand.NewSource(9))
	net := newDHTNet(t, N)
	net.seedKnowledge(N-1, N-1, rnd)
	for i, nd := range net.nodes {
		nd := nd
		node.SetDHTDialHook(nd.rt, func(c *objects.Card) node.DialOpt {
			j, ok := net.index[c.DeviceID]
			if !ok {
				return node.DialOpt{Card: c}
			}
			return node.DialOpt{Card: c, CarrierFunc: func(context.Context) (mux.Carrier, error) {
				pair := memcarrier.NewQUIC()
				net.queues[j].push(pair.Server)
				return pair.Client, nil
			}}
		})
		if nd.rt.DHT() == nil {
			t.Fatalf("node %d: Host.DHT() is nil", i)
		}
	}

	sg := testSigner(t, "client-publisher")
	body, err := objects.Marshal(&pb.MailboxHint{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := objects.Seal(objects.LabelMailboxHint, sg, 4, uint64(time.Now().Add(time.Hour).Unix()), body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := net.nodes[0].rt.DHT().Put(ctx, rec)
	if err != nil || res.Stored == 0 {
		t.Fatalf("Put: %v %+v", err, res)
	}
	for _, reader := range []int{5, 11, N - 1} {
		env, err := net.nodes[reader].rt.DHT().Get(ctx, objects.LabelMailboxHint, sg.Public().ID())
		if err != nil || env.Seq != 4 {
			t.Fatalf("reader %d: %v %v", reader, env, err)
		}
	}
	_ = dht.K
}
