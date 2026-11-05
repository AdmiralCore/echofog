package dht

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestGetFindsTheCurrentRecordAmongLiars pins the T5.2 acceptance: in a network where 30 % of the
// nodes lie (an old seq, or a false "no"), a reader finds the newest record.
func TestGetFindsTheCurrentRecordAmongLiars(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4} {
		rnd := rand.New(rand.NewSource(seed))
		net := newFakeNet(t, 80, 40, false, rnd)
		sg := seededSigner(t, 900+uint64(seed))
		old := hint(t, sg, 2, srvNow.Add(time.Hour))
		rec := hint(t, sg, 5, srvNow.Add(time.Hour))
		for _, fn := range net.nodes {
			fn.stale = old
		}
		net.setModes(rnd, 30, liarStale, liarDeny)

		pub := newTestClient(t, net, 7001, rnd, nil)
		res, err := pub.Put(ctxT(t), rec)
		if err != nil {
			t.Fatalf("seed %d: Put: %v", seed, err)
		}
		if res.Stored == 0 {
			t.Fatalf("seed %d: record stored nowhere: %+v", seed, res)
		}
		for r := 0; r < 4; r++ {
			reader := newTestClient(t, net, uint64(7100+r), rnd, nil)
			env, err := reader.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg))
			if err != nil {
				t.Fatalf("seed %d reader %d: %v", seed, r, err)
			}
			if env.Seq != 5 {
				t.Fatalf("seed %d reader %d: got seq %d, want 5", seed, r, env.Seq)
			}
		}
	}
}

// TestGetUsesThreeDisjointPaths: the queried sets of the three paths share no node and no
// diversity unit (dht.md §4.2), for a label that requires it and for one that only recommends it.
func TestGetUsesThreeDisjointPaths(t *testing.T) {
	for _, label := range []string{objects.LabelMailboxHint, objects.LabelPrekeys, objects.LabelCard, objects.LabelRelease, "echofog/test/other"} {
		rnd := rand.New(rand.NewSource(11))
		net := newFakeNet(t, 80, 40, true, rnd) // pairs of nodes share a /24
		sg := seededSigner(t, 950)
		reader := newTestClient(t, net, 7200, rnd, nil)
		_, _ = reader.Get(ctxT(t), label, signerID(sg)) // nothing is stored: only the queries matter

		seenDev := map[DeviceID]int{}
		seenUnit := map[string]int{}
		used := 0
		for path, devs := range reader.paths {
			if len(devs) > 0 {
				used++
			}
			for _, d := range devs {
				if p, ok := seenDev[d]; ok && p != path {
					t.Fatalf("%s: node %x queried by paths %d and %d", label, d[:4], p, path)
				}
				seenDev[d] = path
				u := unitOfCard(net.byID[d].card).String()
				if p, ok := seenUnit[u]; ok && p != path {
					t.Fatalf("%s: unit %s queried by paths %d and %d", label, u, p, path)
				}
				seenUnit[u] = path
			}
		}
		if used != 3 {
			t.Fatalf("%s: %d paths used, want 3", label, used)
		}
	}
}

// TestPathsAreRequiredForThePrivateLabels: with only two reachable disjoint paths the lookup of a
// required label does not conclude "not found".
func TestNotFoundNeedsEveryPath(t *testing.T) {
	rnd := rand.New(rand.NewSource(5))
	net := newFakeNet(t, 60, 30, false, rnd)
	sg := seededSigner(t, 960)
	reader := newTestClient(t, net, 7300, rnd, nil)

	// Everybody honest and nobody holds the record: a clear "not found".
	if _, err := reader.Get(ctxT(t), objects.LabelPrekeys, signerID(sg)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty network: %v, want ErrNotFound", err)
	}

	// All nodes down: no answers, so no conclusion at all.
	for _, fn := range net.nodes {
		fn.mode = dead
	}
	if _, err := reader.Get(ctxT(t), objects.LabelPrekeys, signerID(sg)); !errors.Is(err, ErrInconclusive) {
		t.Fatalf("all down: %v, want ErrInconclusive", err)
	}

	// Two nodes answer, which cannot make three disjoint paths: still no "not found".
	net.nodes[0].mode, net.nodes[1].mode = honest, honest
	reader2 := newTestClient(t, net, 7301, rnd, func(c *ClientConfig) {})
	reader2.table.AddFromCard(net.nodes[0].card)
	reader2.table.AddFromCard(net.nodes[1].card)
	if _, err := reader2.Get(ctxT(t), objects.LabelPrekeys, signerID(sg)); errors.Is(err, ErrNotFound) {
		t.Fatalf("two reachable nodes concluded ErrNotFound")
	}
}

// TestGetDoesNotStopAtTheFirstAnswer (dht.md §4.2): after a node returned the record the lookup
// continues until the closest nodes have answered.
func TestGetDoesNotStopAtTheFirstAnswer(t *testing.T) {
	rnd := rand.New(rand.NewSource(21))
	net := newFakeNet(t, 80, 40, false, rnd)
	sg := seededSigner(t, 970)
	rec := hint(t, sg, 3, srvNow.Add(time.Hour))
	pub := newTestClient(t, net, 7400, rnd, nil)
	if _, err := pub.Put(ctxT(t), rec); err != nil {
		t.Fatal(err)
	}
	net.mu.Lock()
	net.queries, net.valueAnswer, net.afterFirst = 0, 0, 0
	net.mu.Unlock()
	reader := newTestClient(t, net, 7401, rnd, nil)
	if _, err := reader.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg)); err != nil {
		t.Fatal(err)
	}
	net.mu.Lock()
	after := net.afterFirst
	net.mu.Unlock()
	if after < K {
		t.Fatalf("only %d queries after the first record answer; the lookup stopped early", after)
	}
}

// TestLookupBudget: an endless supply of fresh cards cannot make a lookup send more than 64
// queries per path (dht.md §4.1) or run for ever.
func TestLookupBudget(t *testing.T) {
	rnd := rand.New(rand.NewSource(31))
	net := newFakeNet(t, 20, 10, false, rnd)
	sg := seededSigner(t, 980)
	counts := map[int]int{}
	var countsMu sync.Mutex
	reader := newTestClient(t, net, 7500, rnd, func(c *ClientConfig) {
		c.Transport = &endlessTransport{t: t}
		prev := c.OnQuery
		c.OnQuery = func(path int, to DeviceID) {
			countsMu.Lock()
			counts[path]++
			countsMu.Unlock()
			prev(path, to)
		}
	})
	for _, fn := range net.nodes {
		reader.table.AddFromCard(fn.card)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := reader.Get(ctx, objects.LabelCard, signerID(sg))
	if err == nil {
		t.Fatal("an endless network produced a record")
	}
	total := 0
	for p, n := range counts {
		if n > MaxQueries {
			t.Fatalf("path %d sent %d queries, budget %d", p, n, MaxQueries)
		}
		total += n
	}
	if total == 0 || total > 3*MaxQueries {
		t.Fatalf("total queries %d", total)
	}
	if _, err := reader.FindNode(ctx, [32]byte{9}); err != nil && !errors.Is(err, ErrInconclusive) {
		t.Fatal(err)
	}
}

// endlessTransport answers every query with 20 cards nobody has seen before.
type endlessTransport struct {
	mu   sync.Mutex
	t    *testing.T
	next uint64
	pool [][]byte
}

func (e *endlessTransport) fresh() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out [][]byte
	for i := 0; i < K; i++ {
		e.next++
		c := buildCard(e.t, 50000+e.next, 1, srvNow, objects.ScopePublic, 0,
			testAddr(addrFor(e.next), 4430))
		out = append(out, c.Env.Raw)
	}
	return out
}

func addrFor(n uint64) string {
	return netipFrom(n)
}

func (e *endlessTransport) FindNode(context.Context, *objects.Card, [32]byte) ([][]byte, error) {
	return e.fresh(), nil
}
func (e *endlessTransport) FindValue(context.Context, *objects.Card, [32]byte) ([]byte, [][]byte, error) {
	return nil, e.fresh(), nil
}
func (e *endlessTransport) Store(context.Context, *objects.Card, []byte) (uint32, error) {
	return StatusStored, nil
}

// TestReaderMemoryRefusesRollbackAcrossRestart (dht.md §4.2): a reader that has accepted seq 5
// never accepts a lower one, also after a restart.
func TestReaderMemoryRefusesRollbackAcrossRestart(t *testing.T) {
	rnd := rand.New(rand.NewSource(41))
	net := newFakeNet(t, 60, 30, false, rnd)
	sg := seededSigner(t, 990)
	rec5 := hint(t, sg, 5, srvNow.Add(time.Hour))
	rec3 := hint(t, sg, 3, srvNow.Add(time.Hour))
	pub := newTestClient(t, net, 7600, rnd, nil)
	if _, err := pub.Put(ctxT(t), rec5); err != nil {
		t.Fatal(err)
	}
	reader := newTestClient(t, net, 7601, rnd, nil)
	if env, err := reader.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg)); err != nil || env.Seq != 5 {
		t.Fatalf("first read: %v %v", env, err)
	}

	// The network now shows only seq 3 (every node lies with an old record).
	for _, fn := range net.nodes {
		fn.mode, fn.stale = liarStale, rec3
	}
	if env, err := reader.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg)); !errors.Is(err, ErrRollback) {
		t.Fatalf("rollback accepted: %v %v", env, err)
	}
	restarted := reader.restart(t, net, rnd)
	if env, err := restarted.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg)); !errors.Is(err, ErrRollback) {
		t.Fatalf("rollback accepted after restart: %v %v", env, err)
	}
	// A new reader without memory is the documented exception: it takes what it sees.
	fresh := newTestClient(t, net, 7602, rnd, nil)
	if env, err := fresh.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg)); err != nil || env.Seq != 3 {
		t.Fatalf("a reader without memory: %v %v", env, err)
	}
	// A higher seq is accepted and raises the memory.
	rec7 := hint(t, sg, 7, srvNow.Add(time.Hour))
	for _, fn := range net.nodes {
		fn.stale = rec7
	}
	if env, err := restarted.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg)); err != nil || env.Seq != 7 {
		t.Fatalf("higher seq: %v %v", env, err)
	}
	for _, fn := range net.nodes {
		fn.stale = rec5
	}
	if _, err := restarted.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg)); !errors.Is(err, ErrRollback) {
		t.Fatalf("memory did not rise to 7: %v", err)
	}
}

// TestGetIgnoresForgedAnswers: a record of another signer, a tampered one, an expired one and one
// with another label never come out of Get, and the nodes that sent them are not counted as
// having answered.
func TestGetIgnoresForgedAnswers(t *testing.T) {
	rnd := rand.New(rand.NewSource(51))
	net := newFakeNet(t, 60, 30, false, rnd)
	sg := seededSigner(t, 1010)
	other := seededSigner(t, 1011)

	good := hint(t, sg, 2, srvNow.Add(time.Hour))
	tampered := append([]byte(nil), good...)
	tampered[len(tampered)-1] ^= 0xff
	forgeries := map[string][]byte{
		"other signer": hint(t, other, 9, srvNow.Add(time.Hour)),
		"tampered":     tampered,
		"expired":      hint(t, sg, 9, srvNow.Add(-time.Hour)),
		"other label":  unknownRecord(t, "echofog/test/x", sg, 9, srvNow.Add(time.Hour)),
		"garbage":      []byte("garbage"),
	}
	for name, bad := range forgeries {
		for _, fn := range net.nodes {
			fn.mode, fn.forge = forger, bad
		}
		reader := newTestClient(t, net, 7700, rnd, nil)
		env, err := reader.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg))
		if err == nil || env != nil {
			t.Fatalf("%s: a forged record came out: %+v", name, env)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: forged answers counted as a majority saying not found", name)
		}
	}

	// With a quarter of the nodes forging and the rest honest, the real record wins.
	for _, fn := range net.nodes {
		fn.mode = honest
	}
	pub := newTestClient(t, net, 7701, rnd, nil)
	if _, err := pub.Put(ctxT(t), good); err != nil {
		t.Fatal(err)
	}
	net.setModes(rnd, 25, forger)
	for _, fn := range net.nodes {
		fn.forge = forgeries["other signer"]
	}
	reader := newTestClient(t, net, 7702, rnd, nil)
	env, err := reader.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg))
	if err != nil || env.Seq != 2 {
		t.Fatalf("with forgers: %v %v", env, err)
	}
}

// TestInvalidCardsAreDroppedAndCounted (dht.md §4.1).
func TestInvalidCardsAreDroppedAndCounted(t *testing.T) {
	rnd := rand.New(rand.NewSource(61))
	net := newFakeNet(t, 60, 30, false, rnd)
	net.setModes(rnd, 30, badCards)
	reader := newTestClient(t, net, 7800, rnd, nil)
	cards, err := reader.FindNode(ctxT(t), [32]byte{0x42})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if net.byID[DeviceID(c.DeviceID)].mode == badCards {
			t.Fatalf("node %x sent an invalid card and is still in the result", c.DeviceID[:4])
		}
	}
}

// TestFindNodeResultRules (dht.md §4.1): up to 20 responders, closest first, at most one per
// diversity unit.
func TestFindNodeResultRules(t *testing.T) {
	rnd := rand.New(rand.NewSource(71))
	net := newFakeNet(t, 80, 40, true, rnd)
	reader := newTestClient(t, net, 7900, rnd, nil)
	target := [32]byte{0x99}
	cards, err := reader.FindNode(ctxT(t), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) == 0 || len(cards) > K {
		t.Fatalf("%d cards", len(cards))
	}
	units := map[string]bool{}
	for i, c := range cards {
		u := unitOfCard(c).String()
		if units[u] {
			t.Fatalf("two results in unit %s", u)
		}
		units[u] = true
		if i > 0 && lessDistance(DeviceID(c.DeviceID), DeviceID(cards[i-1].DeviceID), DeviceID(target)) {
			t.Fatalf("result %d is closer than %d", i, i-1)
		}
	}
	// What the lookup learned goes into the routing table as cards (dht.md §5).
	if reader.table.Len() <= 25 {
		t.Fatalf("table did not learn from the answers: %d entries", reader.table.Len())
	}
}

// TestPutChoosesOneReplicaPerUnit (dht.md §3): replicas are the closest nodes, at most one per
// diversity unit, and the number depends on the label (registries.md §9).
func TestPutChoosesOneReplicaPerUnit(t *testing.T) {
	cases := []struct {
		label string
		want  int
		rec   func(*keys.Signer) []byte
	}{
		{objects.LabelMailboxHint, 5, func(sg *keys.Signer) []byte { return hint(t, sg, 1, srvNow.Add(time.Hour)) }},
		{"echofog/test/other", 5, func(sg *keys.Signer) []byte {
			return unknownRecord(t, "echofog/test/other", sg, 1, srvNow.Add(time.Hour))
		}},
	}
	for _, tc := range cases {
		rnd := rand.New(rand.NewSource(81))
		net := newFakeNet(t, 80, 40, true, rnd)
		sg := seededSigner(t, 1020)
		pub := newTestClient(t, net, 8000, rnd, nil)
		res, err := pub.Put(ctxT(t), tc.rec(sg))
		if err != nil {
			t.Fatal(err)
		}
		if res.Stored != tc.want {
			t.Fatalf("%s: stored on %d, want %d (%+v)", tc.label, res.Stored, tc.want, res)
		}
		units := map[string]int{}
		for _, d := range net.storeLog {
			units[unitOfCard(net.byID[d].card).String()]++
		}
		for u, n := range units {
			if n > 1 {
				t.Fatalf("%s: %d STOREs into unit %s", tc.label, n, u)
			}
		}
		// Every recipient is among the closest nodes by the true ranking.
		key := objects.DHTKey(tc.label, signerID(sg), objects.DHTDay(srvNow))
		rank := net.ranked(key)
		pos := map[DeviceID]int{}
		for i, fn := range rank {
			pos[DeviceID(fn.card.DeviceID)] = i
		}
		for _, d := range net.storeLog {
			if pos[d] >= 3*K {
				t.Fatalf("%s: stored on rank %d", tc.label, pos[d])
			}
		}
	}
	if Replicas(objects.LabelCard) != 8 || Replicas(objects.LabelPrekeys) != 8 || Replicas(objects.LabelDevices) != 8 ||
		Replicas(objects.LabelMailboxHint) != 5 || Replicas(objects.LabelRelease) != 20 || Replicas("echofog/x") != 5 {
		t.Fatal("replica counts differ from registries.md §9")
	}
	for _, l := range []string{objects.LabelPrekeys, objects.LabelCard, objects.LabelMailboxHint, objects.LabelRelease} {
		if !RequiresDisjointPaths(l) {
			t.Fatalf("%s must require disjoint paths", l)
		}
	}
	if RequiresDisjointPaths("echofog/other") || RequiresDisjointPaths(objects.LabelDevices) {
		t.Fatal("recommended labels are not required")
	}
}

// TestPutAndGetAroundMidnight (dht.md §2): in the last 2 hours of a day the record is stored
// under the next day's key too; in the first 2 hours readers also look under the previous day's.
func TestPutAndGetAroundMidnight(t *testing.T) {
	rnd := rand.New(rand.NewSource(91))
	net := newFakeNet(t, 80, 40, false, rnd)
	sg := seededSigner(t, 1030)
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)

	// Noon: only today's key is served by the replicas chosen.
	net.setClock(day.Add(12 * time.Hour))
	pub := newTestClient(t, net, 8100, rnd, nil)
	rec := hint(t, sg, 1, day.Add(17*time.Hour))
	if _, err := pub.Put(ctxT(t), rec); err != nil {
		t.Fatal(err)
	}
	noon := len(net.storeLog)

	// 23:30: also the next day's key, so the replica set is the union of both.
	net.setClock(day.Add(23*time.Hour + 30*time.Minute))
	net.storeLog = nil
	rec2 := hint(t, sg, 2, day.Add(28*time.Hour))
	if _, err := pub.Put(ctxT(t), rec2); err != nil {
		t.Fatal(err)
	}
	late := map[DeviceID]bool{}
	for _, d := range net.storeLog {
		late[d] = true
	}
	today := objects.DHTKey(objects.LabelMailboxHint, signerID(sg), objects.DHTDay(net.Now()))
	tomorrow := objects.DHTKey(objects.LabelMailboxHint, signerID(sg), objects.DHTDay(net.Now())+1)
	hit := func(key [32]byte) int {
		n := 0
		for i, fn := range net.ranked(key) {
			if i < 5 && late[DeviceID(fn.card.DeviceID)] {
				n++
			}
		}
		return n
	}
	if hit(today) < 3 || hit(tomorrow) < 3 {
		t.Fatalf("23:30 publication reached %d of today's and %d of tomorrow's closest", hit(today), hit(tomorrow))
	}
	if len(net.storeLog) <= noon-1 && len(late) <= noon/2 {
		t.Fatalf("23:30 publication used no more nodes than noon (%d vs %d)", len(late), noon)
	}

	// 00:30 next day: a reader looks under the previous day's key as well and finds seq 2.
	net.setClock(day.Add(24*time.Hour + 30*time.Minute))
	reader := newTestClient(t, net, 8101, rnd, nil)
	env, err := reader.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg))
	if err != nil || env.Seq != 2 {
		t.Fatalf("00:30 read: %v %v", env, err)
	}
	// 05:00: only the new day's key is read; the record published late is still under it.
	net.setClock(day.Add(24*time.Hour + 3*time.Hour))
	reader2 := newTestClient(t, net, 8102, rnd, nil)
	if env, err := reader2.Get(ctxT(t), objects.LabelMailboxHint, signerID(sg)); err != nil || env.Seq != 2 {
		t.Fatalf("03:00 read: %v %v", env, err)
	}
}

// TestResolveNodeToCardsAndPrekeys: node ID -> devices/2 -> the devices' cards and prekeys.
func TestResolveNodeToCardsAndPrekeys(t *testing.T) {
	rnd := rand.New(rand.NewSource(101))
	net := newFakeNet(t, 60, 30, false, rnd)
	card := buildCard(t, 4242, 1, srvNow, objects.ScopePublic, 0, testAddr("203.0.113.9", 4430))
	devSigner := deviceSignerOf(t, 4242)
	list := card.Body.GetDevices()
	node := card.Account.ID()

	prekeys, err := objects.Seal(objects.LabelPrekeys, devSigner, 1, uint64(srvNow.Add(24*time.Hour).Unix()), mustMarshalPrekeys(t, devSigner))
	if err != nil {
		t.Fatal(err)
	}
	pub := newTestClient(t, net, 8200, rnd, nil)
	for _, rec := range [][]byte{list, card.Env.Raw, prekeys} {
		if res, err := pub.Put(ctxT(t), rec); err != nil || res.Stored == 0 {
			t.Fatalf("put: %v %+v", err, res)
		}
	}
	reader := newTestClient(t, net, 8201, rnd, nil)
	got, err := reader.Resolve(ctxT(t), node)
	if err != nil {
		t.Fatal(err)
	}
	if got.Devices == nil || got.Devices.NodeID != node {
		t.Fatalf("device list: %+v", got.Devices)
	}
	if len(got.Cards) != 1 || got.Cards[0].DeviceID != card.DeviceID {
		t.Fatalf("cards: %+v", got.Cards)
	}
	if len(got.Prekeys) != 1 || !got.Prekeys[0].VouchedBy(got.Devices) {
		t.Fatalf("prekeys: %+v", got.Prekeys)
	}
	// An account nobody published is an error, not an empty answer.
	if _, err := reader.Resolve(ctxT(t), [32]byte{1, 2, 3}); err == nil {
		t.Fatal("unknown account resolved")
	}
	_ = bytes.Equal
}

func mustMarshalPrekeys(t *testing.T, dev *keys.Signer) []byte {
	t.Helper()
	b, err := objects.Marshal(&pb.Prekeys{Ik: keys.New(keys.X25519, make([]byte, 32)), Spk: make([]byte, 32),
		SpkCreated: uint64(srvNow.Unix()), Kem: make([]byte, 1184), KemCreated: uint64(srvNow.Unix())})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGetReleaseIgnoresRecordsTheCallerDoesNotTrust (T5.2 F1): any signer can put a release/2
// object with a huge seq under the day key. The caller's predicate (the compiled release keys)
// decides which records take part; a forgery with a higher seq neither wins nor is remembered.
func TestGetReleaseIgnoresRecordsTheCallerDoesNotTrust(t *testing.T) {
	rnd := rand.New(rand.NewSource(121))
	net := newFakeNet(t, 60, 30, false, rnd)
	trusted := seededSigner(t, 1100)
	attacker := seededSigner(t, 1101)
	real := releaseRecord(t, trusted, 5, srvNow.Add(time.Hour))
	fake := releaseRecord(t, attacker, 1<<40, srvNow.Add(time.Hour))
	net.storeOn(t, real, func(i int) bool { return i%10 >= 3 }) // 70 % hold the real manifest
	net.storeOn(t, fake, func(i int) bool { return i%10 < 3 })  // 30 % hold the forgery
	trustedID := signerID(trusted)
	valid := func(raw []byte, now time.Time) error {
		env, err := objects.Verify(raw, now)
		if err != nil {
			return err
		}
		if env.SignerID() != trustedID {
			return errors.New("not a release key")
		}
		return nil
	}

	for r := 0; r < 4; r++ {
		reader := newTestClient(t, net, uint64(8400+r), rnd, nil)
		env, err := reader.GetRelease(ctxT(t), valid)
		if err != nil || env.Seq != 5 || env.SignerID() != trustedID {
			t.Fatalf("reader %d: %v %v", r, env, err)
		}
		if _, ok, _ := reader.mem.Seq(objects.LabelRelease, signerID(attacker)); ok {
			t.Fatalf("reader %d remembered a seq of an untrusted signer", r)
		}
	}
	// Without a predicate nothing is accepted: the DHT cannot judge a release by itself.
	reader := newTestClient(t, net, 8410, rnd, nil)
	if env, err := reader.GetRelease(ctxT(t), nil); err == nil {
		t.Fatalf("a release was accepted without a check: %v", env)
	}
}

// TestCardsWithUnroutableAddressesAreNotDialled (T5.2 F2): a hostile node hands out signed cards
// that point at loopback, private, link-local and metadata addresses. The client neither queries
// them nor returns them.
func TestCardsWithUnroutableAddressesAreNotDialled(t *testing.T) {
	rnd := rand.New(rand.NewSource(131))
	net := newFakeNet(t, 60, 30, false, rnd)
	for i, host := range []string{"127.0.0.1", "10.9.9.9", "169.254.169.254", "192.168.1.5", "100.64.0.9", "::1"} {
		net.lures = append(net.lures, buildCard(t, uint64(3000+i), 1, srvNow, objects.ScopePublic, 0, testAddr(host, 4430)))
	}
	reader := newTestClient(t, net, 8500, rnd, nil)
	cards, err := reader.FindNode(ctxT(t), [32]byte{0x31})
	if err != nil {
		t.Fatal(err)
	}
	lure := map[[32]byte]bool{}
	for _, l := range net.lures {
		lure[l.DeviceID] = true
	}
	for _, c := range cards {
		if lure[c.DeviceID] {
			t.Fatalf("an unroutable card is in the result")
		}
	}
	if _, err := reader.Get(ctxT(t), objects.LabelCard, [32]byte{4}); err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInconclusive) {
		t.Fatal(err)
	}
	net.mu.Lock()
	hits := net.lureHits
	net.mu.Unlock()
	if hits != 0 {
		t.Fatalf("%d queries went to unroutable cards", hits)
	}
	for _, l := range net.lures {
		if reader.table.Has(DeviceID(l.DeviceID)) {
			t.Fatal("an unroutable card entered the routing table")
		}
	}
}
