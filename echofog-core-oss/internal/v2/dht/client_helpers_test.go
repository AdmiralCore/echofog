package dht

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// mode is how a node of the fake network behaves when it is asked.
type mode int

const (
	honest    mode = iota
	liarStale      // FIND_VALUE answers with an older record of the same signer
	liarDeny       // FIND_VALUE says "no record" and gives closer cards
	dead           // every request fails
	badCards       // answers carry a card that does not verify
	forger         // FIND_VALUE answers with a record of another signer / a tampered one
)

type fakeNode struct {
	card  *objects.Card
	srv   *Server
	mode  mode
	known []*objects.Card
	stale []byte // answer of a liarStale node, set per test
	forge []byte // answer of a forger node
}

// fakeNet is a network of real storing servers behind an in-memory transport. The clock is shared,
// so a test can move the whole network to another time of day.
type fakeNet struct {
	t     *testing.T
	mu    sync.Mutex
	clock time.Time
	nodes []*fakeNode
	byID  map[DeviceID]*fakeNode
	// sharedUnit makes pairs of nodes share one /24, to exercise the diversity rule.
	queries     int
	valueAnswer int // number of FIND_VALUE answers that carried a record
	afterFirst  int // queries sent after the first record answer
	stores      map[DeviceID]int
	storeLog    []DeviceID
	lures       []*objects.Card // hostile cards every node adds to its answers
	lureHits    int
}

func (n *fakeNet) Now() time.Time {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.clock
}

func (n *fakeNet) setClock(t time.Time) {
	n.mu.Lock()
	n.clock = t
	n.mu.Unlock()
}

// newFakeNet builds size nodes. Every node knows `knows` random others (the cards its FIND_NODE
// hands out); liarPct percent of the nodes are liars of the given kinds, picked from rnd.
func newFakeNet(t *testing.T, size, knows int, pairs bool, rnd *rand.Rand) *fakeNet {
	t.Helper()
	n := &fakeNet{t: t, clock: srvNow, byID: map[DeviceID]*fakeNode{}, stores: map[DeviceID]int{}}
	for i := 0; i < size; i++ {
		unit := i
		if pairs {
			unit = i / 2
		}
		host := fmt.Sprintf("%d.%d.1.%d", 20+unit/250, unit%250, 1+i%2) // every unit in its own /16
		card := buildCard(t, uint64(1000+i), 1, srvNow, objects.ScopePublic, 0, testAddr(host, 4430))
		fn := &fakeNode{card: card}
		n.nodes = append(n.nodes, fn)
		n.byID[DeviceID(card.DeviceID)] = fn
	}
	for i, fn := range n.nodes {
		fn := fn
		for _, j := range rnd.Perm(size) {
			if j == i {
				continue
			}
			fn.known = append(fn.known, n.nodes[j].card)
			if len(fn.known) >= knows {
				break
			}
		}
		srv, err := NewServer(ServerConfig{
			Self:        DeviceID(fn.card.DeviceID),
			Responsible: func([32]byte) bool { return true },
			Reached:     func(time.Time) []*objects.Card { return fn.known },
			Now:         n.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		fn.srv = srv
	}
	return n
}

// setModes turns pct percent of the nodes into liars of the given modes (round robin).
func (n *fakeNet) setModes(rnd *rand.Rand, pct int, modes ...mode) {
	cnt := len(n.nodes) * pct / 100
	for k, i := range rnd.Perm(len(n.nodes))[:cnt] {
		n.nodes[i].mode = modes[k%len(modes)]
	}
}

// ranked returns the nodes ordered by distance to key.
func (n *fakeNet) ranked(key [32]byte) []*fakeNode {
	out := append([]*fakeNode(nil), n.nodes...)
	sort.Slice(out, func(a, b int) bool {
		return lessDistance(DeviceID(out[a].card.DeviceID), DeviceID(out[b].card.DeviceID), DeviceID(key))
	})
	return out
}

func unitOfCard(c *objects.Card) netip.Prefix {
	ip, _ := netip.ParseAddr(c.Addrs()[0].GetHost())
	return diversityUnit(ip)
}

// transport is the Transport one client sees.
type fakeTransport struct {
	net  *fakeNet
	self DeviceID
	unit netip.Prefix
}

func (n *fakeNet) transport(self DeviceID, unit string) *fakeTransport {
	ft := &fakeTransport{net: n, self: self}
	if unit != "" {
		ft.unit = netip.MustParsePrefix(unit)
	}
	return ft
}

var errFakeDown = errors.New("fake: node is down")

func (f *fakeTransport) node(ctx context.Context, to *objects.Card) (*fakeNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.net.mu.Lock()
	f.net.queries++
	if f.net.valueAnswer > 0 {
		f.net.afterFirst++
	}
	f.net.mu.Unlock()
	fn := f.net.byID[DeviceID(to.DeviceID)]
	if fn == nil {
		f.net.mu.Lock()
		f.net.lureHits++
		f.net.mu.Unlock()
	}
	if fn == nil || fn.mode == dead {
		return nil, errFakeDown
	}
	return fn, nil
}

func (f *fakeTransport) FindNode(ctx context.Context, to *objects.Card, target [32]byte) ([][]byte, error) {
	fn, err := f.node(ctx, to)
	if err != nil {
		return nil, err
	}
	if fn.mode == badCards {
		return [][]byte{[]byte("not a card")}, nil
	}
	return f.net.withLures(fn.srv.FindNodeAt(target, f.net.Now())), nil
}

func (f *fakeTransport) FindValue(ctx context.Context, to *objects.Card, key [32]byte) ([]byte, [][]byte, error) {
	fn, err := f.node(ctx, to)
	if err != nil {
		return nil, nil, err
	}
	now := f.net.Now()
	switch fn.mode {
	case badCards:
		return nil, [][]byte{[]byte("not a card")}, nil
	case liarDeny:
		return nil, fn.srv.FindNodeAt(key, now), nil
	case liarStale:
		if fn.stale != nil {
			f.net.recordAnswer()
			return fn.stale, nil, nil
		}
		return nil, fn.srv.FindNodeAt(key, now), nil
	case forger:
		f.net.recordAnswer()
		return fn.forge, nil, nil
	}
	rec, cards := fn.srv.FindValueAt(key, now)
	if rec != nil {
		f.net.recordAnswer()
	}
	if rec == nil {
		cards = f.net.withLures(cards)
	}
	return rec, cards, nil
}

func (n *fakeNet) recordAnswer() {
	n.mu.Lock()
	n.valueAnswer++
	n.mu.Unlock()
}

func (f *fakeTransport) Store(ctx context.Context, to *objects.Card, rec []byte) (uint32, error) {
	fn, err := f.node(ctx, to)
	if err != nil {
		return 0, err
	}
	f.net.mu.Lock()
	f.net.stores[DeviceID(to.DeviceID)]++
	f.net.storeLog = append(f.net.storeLog, DeviceID(to.DeviceID))
	f.net.mu.Unlock()
	h := sha256.Sum256(append(f.self[:], to.DeviceID[:]...))
	var sid [16]byte
	copy(sid[:], h[:])
	res := fn.srv.StoreAt(Peer{Device: f.self, SID: sid, Unit: f.unit}, rec, f.net.Now())
	if res.Err != 0 {
		return 0, &RemoteError{Code: res.Err}
	}
	return res.Status, nil
}

// testClient is one reader or publisher over a fake network.
type testClient struct {
	*Client
	table  *Table
	memory *memKV
	self   *objects.Card
	paths  map[int][]DeviceID
	mu     sync.Mutex
}

func newTestClient(t *testing.T, net *fakeNet, seed uint64, rnd *rand.Rand, mutate func(*ClientConfig)) *testClient {
	t.Helper()
	self := buildCard(t, seed, 1, srvNow, objects.ScopePublic, 0, testAddr(fmt.Sprintf("192.0.2.%d", seed%200+1), 4430))
	tbl, err := Open(Config{Self: DeviceID(self.DeviceID), Now: net.Now, Rand: rand.New(rand.NewSource(int64(seed)))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tbl.Stop)
	for _, i := range rnd.Perm(len(net.nodes))[:min(25, len(net.nodes))] {
		tbl.AddFromCard(net.nodes[i].card)
	}
	tc := &testClient{table: tbl, memory: newMemKV(), self: self, paths: map[int][]DeviceID{}}
	cfg := ClientConfig{
		Self:      DeviceID(self.DeviceID),
		Table:     tbl,
		Transport: net.transport(DeviceID(self.DeviceID), "192.0.2.0/24"),
		Memory:    tc.memory,
		Now:       net.Now,
		Rand:      rand.New(rand.NewSource(int64(seed) + 99)),
		OnQuery: func(path int, to DeviceID) {
			tc.mu.Lock()
			tc.paths[path] = append(tc.paths[path], to)
			tc.mu.Unlock()
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tc.Client = c
	return tc
}

// restart builds a new client over the same reader memory, as after a process restart.
func (tc *testClient) restart(t *testing.T, net *fakeNet, rnd *rand.Rand) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{
		Self:      DeviceID(tc.self.DeviceID),
		Table:     tc.table,
		Transport: net.transport(DeviceID(tc.self.DeviceID), "192.0.2.0/24"),
		Memory:    tc.memory,
		Now:       net.Now,
		Rand:      rnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// hint seals a mailbox hint (a signer-keyed label that needs three disjoint paths).
func hint(t testing.TB, sg *keys.Signer, seq uint64, notAfter time.Time) []byte {
	return hintRecord(t, sg, seq, notAfter)
}

// deviceSignerOf rebuilds the device key buildCard derives from a seed.
func deviceSignerOf(t testing.TB, seed uint64) *keys.Signer {
	t.Helper()
	var s [32]byte
	binary.BigEndian.PutUint64(s[0:8], seed)
	binary.BigEndian.PutUint64(s[8:16], seed^0x9e3779b97f4a7c15)
	sg, err := keys.NewEd25519Signer(s[:])
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

// netipFrom makes a distinct public-looking IPv4 address (own /24) for a counter.
func netipFrom(n uint64) string {
	return fmt.Sprintf("%d.%d.%d.9", 30+(n>>16)&0x3f, (n>>8)&0xff, n&0xff)
}

// withLures puts the hostile cards at the front of an answer (up to the 20 cards a NODES allows).
func (n *fakeNet) withLures(cards [][]byte) [][]byte {
	if len(n.lures) == 0 {
		return cards
	}
	var out [][]byte
	for _, l := range n.lures {
		out = append(out, l.Env.Raw)
	}
	out = append(out, cards...)
	if len(out) > K {
		out = out[:K]
	}
	return out
}

// releaseRecord seals a release/2 object with an empty body (the DHT's generic checks only).
func releaseRecord(t testing.TB, sg *keys.Signer, seq uint64, notAfter time.Time) []byte {
	t.Helper()
	b, err := objects.Seal(objects.LabelRelease, sg, seq, uint64(notAfter.Unix()), nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// storeEverywhere puts a record straight into the servers picked by pick (a test's way to make
// nodes "hold" something, however they got it).
func (n *fakeNet) storeOn(t testing.TB, rec []byte, pick func(i int) bool) {
	t.Helper()
	for i, fn := range n.nodes {
		if !pick(i) {
			continue
		}
		var sid [16]byte
		sid[0], sid[1] = byte(i), byte(i>>8)
		res := fn.srv.StoreAt(Peer{SID: sid, Device: DeviceID{byte(i + 1)}}, rec, n.Now())
		if res.Err != 0 || res.Status != StatusStored {
			t.Fatalf("node %d did not store: %+v", i, res)
		}
	}
}
