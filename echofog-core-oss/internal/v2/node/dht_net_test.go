package node_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// dhtNet is a test network of real runtimes over in-memory carriers. Every node has its own
// public address, so the routing tables and the diversity rules behave as in a real network.
type dhtNet struct {
	t      *testing.T
	nodes  []*testNode
	queues []*carrierQueue
	pub    []*objects.Card
	index  map[[32]byte]int
}

func newDHTNet(t *testing.T, n int) *dhtNet {
	t.Helper()
	net := &dhtNet{t: t, index: map[[32]byte]int{}}
	for i := 0; i < n; i++ {
		q := newCarrierQueue()
		host := fmt.Sprintf("10.0.%d.1", i)
		nd := newTestNodeFull(t, cards.Visible, true, []carrier.Listener{q}, nil,
			func(c *cards.Config) {
				for k := range c.Listeners {
					c.Listeners[k].Host = host
				}
			},
			func(c *node.Config) {
				c.Profile = node.Profile{Platform: node.PlatformDesktop, Roles: []node.Role{node.RoleDHTServer}}
			})
		nd.start(t)
		net.nodes = append(net.nodes, nd)
		net.queues = append(net.queues, q)
		c := nd.publicCard(t)
		net.pub = append(net.pub, c)
		net.index[c.DeviceID] = i
	}
	return net
}

// seedKnowledge gives every node `known` routing entries and `reached` working-session cards.
func (n *dhtNet) seedKnowledge(known, reached int, rnd *rand.Rand) {
	N := len(n.nodes)
	for i, nd := range n.nodes {
		perm := rnd.Perm(N)
		added := 0
		for _, j := range perm {
			if j == i {
				continue
			}
			if added < known {
				nd.rt.RoutingTable().AddFromCard(n.pub[j])
			}
			if added < reached {
				nd.rt.Peers().NoteSession(n.pub[j], cardAddr(n.t, n.pub[j]), time.Now())
			}
			added++
			if added >= known && added >= reached {
				break
			}
		}
	}
}

func (n *dhtNet) open(from, to int) *mux.Stream {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, st, err := n.nodes[from].rt.Open(ctx, n.nodes[to].id.DeviceID, service.DHT, node.DialOpt{
		Card: n.pub[to],
		CarrierFunc: func(context.Context) (mux.Carrier, error) {
			pair := memcarrier.NewQUIC()
			n.queues[to].push(pair.Server)
			return pair.Client, nil
		},
	})
	if err != nil {
		n.t.Fatalf("node %d opens dht/2 to %d: %v", from, to, err)
	}
	return st
}

var netReq uint64

func (n *dhtNet) roundTrip(from, to int, typ uint64, payload []byte) (uint64, []byte) {
	n.t.Helper()
	st := n.open(from, to)
	defer st.Close()
	if err := st.WriteFrame(typ, 0, payload); err != nil {
		n.t.Fatalf("write: %v", err)
	}
	f, err := st.ReadFrame()
	if err != nil {
		n.t.Fatalf("read answer from node %d: %v", to, err)
	}
	return f.Type, f.Payload
}

// lookup is a plain iterative FIND_NODE (the real client is T5.2): it returns the node indexes of
// the closest nodes it found, closest first.
func (n *dhtNet) lookup(from int, key [32]byte) []int {
	n.t.Helper()
	tbl := n.nodes[from].rt.RoutingTable()
	known := map[int]bool{}
	for _, c := range tbl.Closest(key, dht.K) {
		known[n.index[c.DeviceID]] = true
	}
	queried := map[int]bool{from: true}
	byDist := func() []int {
		out := make([]int, 0, len(known))
		for i := range known {
			out = append(out, i)
		}
		sort.Slice(out, func(a, b int) bool {
			return lessID(n.pub[out[a]].DeviceID, n.pub[out[b]].DeviceID, key)
		})
		return out
	}
	for q := 0; q < 64; q++ {
		var next = -1
		for _, i := range byDist() {
			if len(known) > 0 && !queried[i] {
				next = i
				break
			}
		}
		if next < 0 {
			break
		}
		// Stop when the 20 closest are all queried.
		all := true
		for k, i := range byDist() {
			if k >= dht.K {
				break
			}
			if !queried[i] {
				all = false
			}
		}
		if all {
			break
		}
		queried[next] = true
		netReq++
		req, _ := dht.MarshalFindNode(netReq, key)
		typ, body := n.roundTrip(from, next, dht.TypeFindNode, req)
		if typ != dht.TypeNodes {
			n.t.Fatalf("FIND_NODE answered with 0x%x", typ)
		}
		nodes, err := dht.UnmarshalNodes(body)
		if err != nil {
			n.t.Fatalf("NODES: %v", err)
		}
		for _, raw := range nodes.Cards {
			c, err := objects.VerifyCard(raw, time.Now())
			if err != nil {
				n.t.Fatalf("NODES carried an invalid card: %v", err)
			}
			if j, ok := n.index[c.DeviceID]; ok && j != from {
				known[j] = true
				tbl.AddFromCard(c)
			}
		}
	}
	out := byDist()
	// Only nodes that answered count as responsive.
	var res []int
	for _, i := range out {
		if queried[i] {
			res = append(res, i)
		}
		if len(res) == dht.K {
			break
		}
	}
	return res
}

func lessID(a, b, key [32]byte) bool {
	return dht.DeviceID(a) != dht.DeviceID(b) && lessDist(a, b, key)
}

func lessDist(a, b, key [32]byte) bool {
	for i := range a {
		da, db := a[i]^key[i], b[i]^key[i]
		if da != db {
			return da < db
		}
	}
	return false
}

// TestDHTNetwork200 runs a network of 200 runtimes: a record published with an iterative lookup
// lands only on nodes near its key, a reader elsewhere finds it, and cards nobody reached never
// appear in answers. Many nodes with memguard hang a Windows test binary, so it runs on Linux
// (WSL); set ECHOFOG_DHT_NET=1 to force it elsewhere and ECHOFOG_DHT_NODES to change the size.
func TestDHTNetwork200(t *testing.T) {
	if runtime.GOOS == "windows" && os.Getenv("ECHOFOG_DHT_NET") == "" {
		t.Skip("200 runtimes in one Windows test binary hang in memguard; run in WSL/Linux")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	N := 200
	if v, err := strconv.Atoi(os.Getenv("ECHOFOG_DHT_NODES")); err == nil && v >= 20 {
		N = v
	}
	rnd := rand.New(rand.NewSource(7))
	net := newDHTNet(t, N)
	net.seedKnowledge(N/2, 40, rnd)

	// Bait: a card that points at a victim address, known to one node as a PEX candidate only.
	bait := thirdPartyCard(t, "bait", "198.51.100.200", 4430)
	net.nodes[3].rt.Peers().AddCandidate(bait, time.Now())

	publisher := 0
	sg := testSigner(t, "net-publisher")
	notAfter := uint64(time.Now().Add(time.Hour).Unix())
	rec, err := objects.Seal("echofog/test/net", sg, 1, notAfter, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := objects.DHTKey("echofog/test/net", sg.Public().ID(), objects.DHTDay(time.Now()))

	// True ranking by distance, for the assertions.
	rank := make([]int, N)
	for i := range rank {
		rank[i] = i
	}
	sort.Slice(rank, func(a, b int) bool { return lessDist(net.pub[rank[a]].DeviceID, net.pub[rank[b]].DeviceID, key) })
	trueRank := map[int]int{}
	for r, i := range rank {
		trueRank[i] = r
	}

	replicas := net.lookup(publisher, key)
	if len(replicas) < dht.K {
		t.Fatalf("lookup found %d nodes, want %d", len(replicas), dht.K)
	}
	near := 0
	for _, i := range replicas {
		if trueRank[i] < dht.K+10 {
			near++
		}
	}
	if near < dht.K-5 {
		t.Fatalf("only %d of the %d nodes found are among the true closest %d", near, len(replicas), dht.K+10)
	}

	stored := 0
	for k, i := range replicas {
		// STORE is limited to 2 per second per session; every node here opens its own session.
		netReq++
		req, _ := dht.MarshalStore(netReq, rec)
		typ, body := net.roundTrip(publisher, i, dht.TypeStore, req)
		if typ != dht.TypeStored {
			t.Fatalf("replica %d (node %d): STORE answered 0x%x", k, i, typ)
		}
		st, _ := dht.UnmarshalStored(body)
		if st.Status == dht.StatusStored {
			stored++
		}
	}
	if stored < dht.K-5 {
		t.Fatalf("stored on %d replicas", stored)
	}

	// Records sit only on nodes near the key: each node checked "among the 20 closest I know".
	holders := 0
	for i, nd := range net.nodes {
		if nd.rt.DHTServer().Len() > 0 {
			holders++
			if trueRank[i] > 3*dht.K {
				t.Errorf("node %d at rank %d stores the record", i, trueRank[i])
			}
		}
	}
	if holders < dht.K-5 {
		t.Fatalf("%d holders", holders)
	}

	// Readers elsewhere find it: lookup then FIND_VALUE over the closest nodes.
	for _, reader := range []int{N / 3, N / 2, N - 1} {
		closest := net.lookup(reader, key)
		var got []byte
		for _, i := range closest {
			netReq++
			req, _ := dht.MarshalFindValue(netReq, key)
			typ, body := net.roundTrip(reader, i, dht.TypeFindValue, req)
			if typ != dht.TypeValue {
				t.Fatalf("FIND_VALUE answered 0x%x", typ)
			}
			v, err := dht.UnmarshalValue(body)
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Record) > 0 {
				got = v.Record
				break
			}
		}
		if !bytes.Equal(got, rec) {
			t.Fatalf("reader %d did not find the record", reader)
		}
	}

	// The bait card is in one node's PEX candidates but nobody ever reached it: it never appears.
	asker := N - 2
	for i := 0; i < N; i += N / 20 {
		target := (i + 1) % N
		if target == asker {
			continue
		}
		if net.roundTripBait(asker, target, bait) {
			t.Fatalf("node %d passed on a card it never reached", target)
		}
	}
	if net.roundTripBait(asker, 3, bait) {
		t.Fatal("the node that holds the bait as a candidate passed it on")
	}
}

func (n *dhtNet) roundTripBait(from, at int, bait *objects.Card) bool {
	netReq++
	req, _ := dht.MarshalFindNode(netReq, bait.DeviceID)
	_, body := n.roundTrip(from, at, dht.TypeFindNode, req)
	nodes, _ := dht.UnmarshalNodes(body)
	for _, c := range nodes.Cards {
		if bytes.Equal(c, bait.Env.Raw) {
			return true
		}
	}
	return false
}
