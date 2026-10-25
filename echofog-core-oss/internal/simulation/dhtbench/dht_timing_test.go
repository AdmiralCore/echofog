// Package dhtbench times the DHT on an in-process cluster: how long a value lookup, a miss, a
// publish and a routing-table refresh take with 25 nodes, both on a clean network and on one whose
// routing tables are full of peers that no longer answer (the usual state of a small network).
// It runs alone in its own package: many nodes in one Windows test binary run out of locked pages.
//
//	go test ./internal/simulation/dhtbench/ -run TestDHTTiming -v -count=1
package dhtbench

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"
	"github.com/echofog/echofog-core/internal/simulation"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/node"
)

func nodeCount() int {
	if v := os.Getenv("SIM_NODE_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 4 {
			return n
		}
	}
	return 25
}

// deadPeers is how many unreachable public peers each node's table gets: addresses in TEST-NET-3
// never answer, as the addresses of clients that left the network do not.
const deadPeers = 40

func timed(t *testing.T, what string, f func() error) time.Duration {
	t.Helper()
	start := time.Now()
	err := f()
	d := time.Since(start)
	status := "ok"
	if err != nil {
		status = err.Error()
	}
	t.Logf("TIMING %-42s %8.0f ms  %s", what, float64(d)/float64(time.Millisecond), status)
	return d
}

// poisonTables gives every node the same dead peers, as a real network does: the clients that
// left are in everybody's table.
func poisonTables(c *simulation.Cluster) {
	type dead struct {
		pub  types.PeerPublicKey
		addr string
	}
	pool := make([]dead, deadPeers)
	for i := range pool {
		_, _ = rand.Read(pool[i].pub[:])
		var b [2]byte
		_, _ = rand.Read(b[:])
		pool[i].addr = fmt.Sprintf("203.0.113.%d:%d", 1+int(b[0])%250, 20000+int(b[1]))
	}
	for _, n := range c.Nodes {
		for _, p := range pool {
			n.DhtEngine.AddKnownPeer(p.pub, p.addr)
		}
	}
}

func TestDHTTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster timing is not a unit test")
	}
	count := nodeCount()
	c, err := simulation.NewCluster(t, count)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WaitForConvergence(20*time.Second, 2); err != nil {
		t.Logf("convergence: %v", err)
	}
	time.Sleep(2 * time.Second) // peer exchange and self lookups settle

	run := func(label string, exit *node.Node) {
		reader := c.Nodes[count-1]
		key := vpnmarket.Key(exit.ID)

		// publish: Start signs a real advert and stores it; it returns at once, so what matters is
		// when another node can read the record
		var val []byte
		timed(t, label+" publish until readable elsewhere", func() error {
			if err := exit.VPNMarket.Publisher.Start(context.Background(), vpnmarket.ShareConfig{PricePerMB: 0, UploadKbps: 1000}); err != nil {
				return err
			}
			val = exit.VPNMarket.Publisher.Current()
			deadline := time.Now().Add(40 * time.Second)
			for time.Now().Before(deadline) {
				for _, n := range c.Nodes {
					if n.ID == exit.ID {
						continue
					}
					if raw, err := n.Swarm.GetStorage().Get(dht.RecordKey(key)); err == nil && len(raw) > 0 {
						return nil
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			return fmt.Errorf("record never replicated")
		})
		timed(t, label+" GetFresh hit (first)", func() error {
			got, err := reader.DhtEngine.GetFresh(key)
			if err != nil {
				return err
			}
			if string(got) != string(val) {
				return fmt.Errorf("wrong value")
			}
			return nil
		})
		timed(t, label+" GetFresh hit (again)", func() error {
			_, err := reader.DhtEngine.GetFresh(key)
			return err
		})
		missKey := vpnmarket.Key(c.Nodes[2].ID) // node 2 is no exit: nobody holds this
		timed(t, label+" GetFresh miss", func() error {
			if _, err := reader.DhtEngine.GetFresh(missKey); err == nil {
				return fmt.Errorf("found a value nobody published")
			}
			return nil
		})
		timed(t, label+" GetFresh miss (again)", func() error {
			_, _ = reader.DhtEngine.GetFresh(missKey)
			return nil
		})
		// a walk that cannot succeed: how long until a node lookup converges
		var unknown types.PeerID
		_, _ = rand.Read(unknown[:])
		timed(t, label+" FindPeer (unknown ID, converges)", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := reader.DhtEngine.FindPeer(ctx, unknown); err == nil {
				return fmt.Errorf("found a peer that does not exist")
			}
			return nil
		})
		timed(t, label+" exit directory Refresh", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			n := reader.VPNMarket.Directory.Refresh(ctx)
			if n == 0 {
				return fmt.Errorf("no exits found")
			}
			return nil
		})
		timed(t, label+" SelfLookup", func() error {
			reader.DhtEngine.SelfLookup(context.Background())
			return nil
		})
		timed(t, label+" RefreshAllBuckets", func() error {
			reader.DhtEngine.RefreshAllBuckets(context.Background())
			return nil
		})
	}

	run("clean", c.Nodes[1])
	poisonTables(c)
	c.Nodes[count-1].DhtEngine.ForgetLookups() // measure walks, not what the clean run remembered
	t.Logf("tables poisoned with %d dead peers each; reader knows %d peers", deadPeers, len(c.Nodes[count-1].DhtEngine.KnownPeers()))
	run("stale", c.Nodes[3])
}
