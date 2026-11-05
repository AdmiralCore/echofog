package node_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/dht"
	dhtpb "github.com/echofog/echofog-core/internal/v2/dht/pb"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

func dhtServerNode(t *testing.T, listeners []carrier.Listener, roles ...node.Role) *testNode {
	t.Helper()
	return newTestNodeWith(t, cards.Visible, true, listeners, nil, func(c *node.Config) {
		c.Profile = node.Profile{Platform: node.PlatformDesktop, Roles: roles}
	})
}

func openDHT(t *testing.T, a, b *testNode, queue *carrierQueue) (*mux.Stream, error) {
	t.Helper()
	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, st, err := a.rt.Open(ctx, b.id.DeviceID, service.DHT, node.DialOpt{Card: b.publicCard(t), Carrier: pair.Client})
	return st, err
}

func ask(t *testing.T, st *mux.Stream, typ uint64, payload []byte) (uint64, []byte) {
	t.Helper()
	if err := st.WriteFrame(typ, 0, payload); err != nil {
		t.Fatalf("write 0x%x: %v", typ, err)
	}
	f, err := st.ReadFrame()
	if err != nil {
		t.Fatalf("read answer to 0x%x: %v", typ, err)
	}
	return f.Type, f.Payload
}

// TestDHTServerOverTwoRuntimes drives dht/2 across a real session: FIND_NODE passes on only cards
// the answerer reached, STORE and FIND_VALUE round-trip, and the label rules answer on the wire.
func TestDHTServerOverTwoRuntimes(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := dhtServerNode(t, []carrier.Listener{queue}, node.RoleDHTServer)
	b.start(t)
	a.start(t)

	reached := thirdPartyCard(t, "reached", "192.0.2.77", 9443)
	victim := thirdPartyCard(t, "victim", "198.51.100.9", 4430)
	if !b.rt.Peers().NoteSession(reached, cardAddr(t, reached), time.Now()) {
		t.Fatal("reached card not recorded")
	}
	b.rt.Peers().AddCandidate(victim, time.Now()) // learned from PEX/DHT, never connected to

	st, err := openDHT(t, a, b, queue)
	if err != nil {
		t.Fatalf("Open dht/2: %v", err)
	}
	defer st.Close()

	var target [32]byte
	req, _ := dht.MarshalFindNode(1, target)
	typ, body := ask(t, st, dht.TypeFindNode, req)
	if typ != dht.TypeNodes {
		t.Fatalf("answer type 0x%x, want NODES", typ)
	}
	nodes, err := dht.UnmarshalNodes(body)
	if err != nil || nodes.Req != 1 {
		t.Fatalf("NODES: %+v %v", nodes, err)
	}
	if len(nodes.Cards) != 1 || !bytes.Equal(nodes.Cards[0], reached.Env.Raw) {
		t.Fatalf("NODES carried %d cards; want only the one the node reached", len(nodes.Cards))
	}
	for _, c := range nodes.Cards {
		if bytes.Equal(c, victim.Env.Raw) {
			t.Fatal("the card of a node nobody reached (reflection bait) was passed on")
		}
	}

	// STORE, then FIND_VALUE under the signer-keyed key.
	sg := testSigner(t, "publisher")
	rec, err := objects.Seal("echofog/test/1", sg, 1, uint64(time.Now().Add(time.Hour).Unix()), nil)
	if err != nil {
		t.Fatal(err)
	}
	sreq, _ := dht.MarshalStore(2, rec)
	typ, body = ask(t, st, dht.TypeStore, sreq)
	stored, err := dht.UnmarshalStored(body)
	if typ != dht.TypeStored || err != nil || stored.Req != 2 || stored.Status != dht.StatusStored {
		t.Fatalf("STORE answer 0x%x %+v %v", typ, stored, err)
	}
	key := objects.DHTKey("echofog/test/1", sg.Public().ID(), objects.DHTDay(time.Now()))
	freq, _ := dht.MarshalFindValue(3, key)
	typ, body = ask(t, st, dht.TypeFindValue, freq)
	val, err := dht.UnmarshalValue(body)
	if typ != dht.TypeValue || err != nil || val.Req != 3 || !bytes.Equal(val.Record, rec) {
		t.Fatalf("FIND_VALUE answer 0x%x %+v %v", typ, val, err)
	}

	// A name record is refused; garbage is an ERROR bad-record.
	nameRec, _ := objects.Seal(objects.LabelName, sg, 1, uint64(time.Now().Add(time.Hour).Unix()), nil)
	nreq, _ := dht.MarshalStore(4, nameRec)
	typ, body = ask(t, st, dht.TypeStore, nreq)
	stored, _ = dht.UnmarshalStored(body)
	if typ != dht.TypeStored || stored.Status != dht.StatusRefused {
		t.Fatalf("name record: 0x%x %+v, want refused", typ, stored)
	}
	time.Sleep(time.Second) // STORE is limited to 2 per second per session
	greq, _ := dht.MarshalStore(5, []byte{1, 2, 3})
	typ, body = ask(t, st, dht.TypeStore, greq)
	e, err := dht.UnmarshalError(body)
	if typ != dht.TypeError || err != nil || e.Req != 5 || e.Code != dht.CodeBadRecord {
		t.Fatalf("garbage: 0x%x %+v %v, want ERROR bad-record", typ, e, err)
	}

	// An unknown FIND_VALUE key answers with closer cards and no record.
	freq, _ = dht.MarshalFindValue(6, [32]byte{9})
	typ, body = ask(t, st, dht.TypeFindValue, freq)
	val, _ = dht.UnmarshalValue(body)
	if typ != dht.TypeValue || len(val.Record) != 0 || len(val.Cards) != 1 {
		t.Fatalf("miss: 0x%x %+v", typ, val)
	}
	_ = dhtpb.Nodes{}
}

// TestNodeWithoutServerRoleDoesNotAnswer: roles without dht-server mean the node serves no dht/2
// requests (platforms.md §2), while its own routing table still works.
func TestNodeWithoutServerRoleDoesNotAnswer(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := dhtServerNode(t, []carrier.Listener{queue}) // no roles
	b.start(t)
	a.start(t)

	if b.rt.RoutingTable() == nil || b.rt.DHTServer() == nil {
		t.Fatal("a non-server node must still have a table and a cache")
	}
	st, err := openDHT(t, a, b, queue)
	if err == nil {
		req, _ := dht.MarshalFindNode(1, [32]byte{})
		_ = st.WriteFrame(dht.TypeFindNode, 0, req)
		if f, rerr := st.ReadFrame(); rerr == nil {
			t.Fatalf("a node without the dht-server role answered with frame 0x%x", f.Type)
		}
	}
}

func testSigner(t *testing.T, name string) *keys.Signer {
	t.Helper()
	var seed [32]byte
	copy(seed[:], name)
	sg, err := keys.NewEd25519Signer(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	return sg
}
