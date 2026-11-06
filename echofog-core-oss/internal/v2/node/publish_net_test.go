package node_test

import (
	"context"
	"math/rand"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// TestNodeIDLeadsToDevicesAndCardInTheDHT is the T4.2b criterion: a node publishes its own
// records with Step; a reader that knows only the node ID finds devices/2 under it, and from
// the list the device's card (dht.md §2).
func TestNodeIDLeadsToDevicesAndCardInTheDHT(t *testing.T) {
	if runtime.GOOS == "windows" && os.Getenv("ECHOFOG_DHT_NET") == "" {
		t.Skip("many runtimes in one Windows test binary hang in memguard; run in WSL/Linux")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	const N = 24
	rnd := rand.New(rand.NewSource(11))
	net := newDHTNet(t, N)
	net.seedKnowledge(N-1, N-1, rnd)
	for _, nd := range net.nodes {
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
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	publisher := net.nodes[0]
	// The loop started with the node has already tried once on an empty table and waits 5 minutes
	// before the retry (T5.2); PublishNow brings the records forward and Step stores them.
	publisher.rt.DHT().PublishNow()
	publisher.rt.DHT().Step(ctx)

	res, err := net.nodes[N-1].rt.DHT().Resolve(ctx, [32]byte(publisher.id.NodeID))
	if err != nil {
		t.Fatalf("Resolve(node ID): %v", err)
	}
	if res.Devices == nil || !res.Devices.Contains(publisher.device.Public()) {
		t.Fatal("the found device list does not name the publisher's device")
	}
	var found bool
	for _, c := range res.Cards {
		if c.DeviceID == [32]byte(publisher.id.DeviceID) && c.Publishable() {
			found = true
		}
	}
	if !found {
		t.Fatalf("the device's public card was not found (%d cards)", len(res.Cards))
	}
}
