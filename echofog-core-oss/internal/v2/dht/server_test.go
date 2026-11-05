package dht

import (
	"bytes"
	"sort"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

func expiry(d time.Duration) time.Time { return srvNow.Add(d) }

func idLess(a, b [32]byte) bool { return bytes.Compare(a[:], b[:]) < 0 }

// oneKeyServer is a server responsible for the current-day key of one signer and label.
func oneKeyServer(t *testing.T, label string, seed uint64, mutate func(*ServerConfig)) (*Server, *keySet, [32]byte) {
	t.Helper()
	ks := newKeySet()
	key := ks.add(dayKey(label, seededSigner(t, seed), srvNow))
	s := newServer(t, func(c *ServerConfig) {
		ks.config(c)
		if mutate != nil {
			mutate(c)
		}
	})
	return s, ks, key
}

func TestStoreThenFindValue(t *testing.T) {
	s, _, key := oneKeyServer(t, objects.LabelMailboxHint, 1, nil)
	sg := seededSigner(t, 1)
	rec := hintRecord(t, sg, 1, expiry(time.Hour))
	if r := s.Store(testPeer("192.0.2.0/24"), rec); r.Status != StatusStored || r.Err != 0 {
		t.Fatalf("Store = %+v", r)
	}
	got, _ := s.FindValue(key)
	if !bytes.Equal(got, rec) {
		t.Fatalf("FindValue returned %d bytes, want the stored record", len(got))
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d", s.Len())
	}
	s.checkInvariants(t)
}

func TestReplacementBySeq(t *testing.T) {
	s, _, key := oneKeyServer(t, objects.LabelMailboxHint, 2, nil)
	sg := seededSigner(t, 2)
	p := testPeer("192.0.2.0/24")
	r2 := hintRecord(t, sg, 2, expiry(time.Hour))
	r1 := hintRecord(t, sg, 1, expiry(time.Hour))
	r3 := hintRecord(t, sg, 3, expiry(2*time.Hour))
	if r := s.StoreAt(p, r2, srvNow); r.Status != StatusStored {
		t.Fatalf("seq 2: %+v", r)
	}
	if r := s.StoreAt(p, r1, srvNow.Add(time.Second)); r.Status != StatusStale {
		t.Fatalf("lower seq: %+v, want stale", r)
	}
	if r := s.StoreAt(p, r3, srvNow.Add(2*time.Second)); r.Status != StatusStored {
		t.Fatalf("higher seq: %+v", r)
	}
	if got, _ := s.FindValue(key); !bytes.Equal(got, r3) {
		t.Fatal("higher seq did not replace the record")
	}
	if s.Len() != 1 {
		t.Fatalf("a replacement must not add a record: Len = %d", s.Len())
	}
	s.checkInvariants(t)
}

func TestEqualSeqKeepsSmallerObjectID(t *testing.T) {
	s, _, key := oneKeyServer(t, objects.LabelMailboxHint, 3, nil)
	sg := seededSigner(t, 3)
	a := hintRecord(t, sg, 5, expiry(time.Hour))
	b := hintRecord(t, sg, 5, expiry(2*time.Hour))
	ea, _ := objects.Parse(a)
	eb, _ := objects.Parse(b)
	small, large := a, b
	if idLess(eb.ID(), ea.ID()) {
		small, large = b, a
	}
	p := testPeer("192.0.2.0/24")
	if r := s.StoreAt(p, large, srvNow); r.Status != StatusStored {
		t.Fatal(r)
	}
	if r := s.StoreAt(p, small, srvNow.Add(time.Second)); r.Status != StatusStored {
		t.Fatalf("smaller object_id must replace: %+v", r)
	}
	if r := s.StoreAt(p, large, srvNow.Add(2*time.Second)); r.Status != StatusStale {
		t.Fatalf("larger object_id must lose: %+v", r)
	}
	if got, _ := s.FindValue(key); !bytes.Equal(got, small) {
		t.Fatal("the smaller object_id is not the stored one")
	}
	s.checkInvariants(t)
}

func TestRefusedAndInvalid(t *testing.T) {
	s := newServer(t, respondAll)
	sg := seededSigner(t, 4)
	p := testPeer("192.0.2.0/24")
	at := srvNow
	step := func() time.Time { at = at.Add(time.Second); return at }

	name := unknownRecord(t, objects.LabelName, sg, 1, expiry(time.Hour))
	if r := s.StoreAt(p, name, step()); r.Status != StatusRefused {
		t.Fatalf("echofog/name/2: %+v, want refused", r)
	}
	other := unknownRecord(t, "other/thing", sg, 1, expiry(time.Hour))
	if r := s.StoreAt(p, other, step()); r.Status != StatusRefused {
		t.Fatalf("label outside echofog/: %+v, want refused", r)
	}
	peerCard := publicCard(t, 41, 1, objects.ScopePeer, 0)
	if r := s.StoreAt(p, peerCard.Env.Raw, step()); r.Status != StatusRefused {
		t.Fatalf("peer-scope card: %+v, want refused", r)
	}
	expired := unknownRecord(t, "echofog/test/1", sg, 1, srvNow.Add(-time.Hour))
	if r := s.StoreAt(p, expired, step()); r.Status != StatusInvalid {
		t.Fatalf("expired: %+v, want invalid", r)
	}
	bad := hintRecord(t, sg, 1, expiry(time.Hour))
	bad[len(bad)-1] ^= 0xff
	if r := s.StoreAt(p, bad, step()); r.Status != StatusInvalid {
		t.Fatalf("bad signature: %+v, want invalid", r)
	}
	if r := s.StoreAt(p, []byte{1, 2, 3}, step()); r.Err != CodeBadRecord {
		t.Fatalf("garbage: %+v, want bad-record", r)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d after refusals", s.Len())
	}
	s.checkInvariants(t)
}

func TestPublicCardIsStoredUnderDeviceKey(t *testing.T) {
	c := publicCard(t, 43, 1, objects.ScopePublic, 0)
	key := objects.DHTKey(objects.LabelCard, c.DeviceID, objects.DHTDay(srvNow))
	ks := newKeySet()
	ks.add(key)
	s := newServer(t, ks.config)
	if r := s.Store(testPeer("192.0.2.0/24"), c.Env.Raw); r.Status != StatusStored {
		t.Fatalf("Store = %+v", r)
	}
	if got, _ := s.FindValue(key); !bytes.Equal(got, c.Env.Raw) {
		t.Fatal("card is not under the signer-keyed key (dht.md §2: signer_id is the device ID)")
	}
}

func TestLifetimeClippedForUnknownLabel(t *testing.T) {
	sg := seededSigner(t, 5)
	ks := newKeySet()
	key := ks.add(dayKey("echofog/test/1", sg, srvNow))
	now := srvNow
	s := newServer(t, func(c *ServerConfig) { ks.config(c); c.Now = func() time.Time { return now } })
	rec := unknownRecord(t, "echofog/test/1", sg, 1, expiry(72*time.Hour))
	if r := s.Store(testPeer("192.0.2.0/24"), rec); r.Status != StatusStored {
		t.Fatal(r)
	}
	now = srvNow.Add(23 * time.Hour)
	if got, _ := s.FindValue(key); got == nil {
		t.Fatal("record gone before the 24-hour clip")
	}
	now = srvNow.Add(25 * time.Hour)
	if got, _ := s.FindValue(key); got != nil {
		t.Fatal("unknown-label record outlived the 24-hour clip")
	}
	s.checkInvariants(t)
}

func TestWrongKeyWhenNotAmongClosest(t *testing.T) {
	sg := seededSigner(t, 6)
	rec := hintRecord(t, sg, 1, expiry(time.Hour))
	d := objects.DHTDay(srvNow)
	var ks [3][32]byte
	for i := range ks {
		ks[i] = objects.DHTKey(objects.LabelMailboxHint, signerID(sg), d+uint32(i)-1)
	}
	// A far-away self: the complement of the first key's bits.
	var self DeviceID
	for i := range self {
		self[i] = ^ks[0][i]
	}
	tbl, err := Open(Config{Self: self, Now: func() time.Time { return srvNow }})
	if err != nil {
		t.Fatal(err)
	}
	// Fill the table so that for each of the three keys at least K known cards are closer than self.
	var closer [3]int
	for seed := uint64(1000); seed < 60000; seed++ {
		if closer[0] >= K && closer[1] >= K && closer[2] >= K {
			break
		}
		c := publicCard(t, seed, 1, objects.ScopePublic, 0)
		useful := false
		for i, k := range ks {
			if closer[i] < K && lessDistance(DeviceID(c.DeviceID), self, DeviceID(k)) {
				useful = true
			}
		}
		if !useful || !tbl.AddFromSession(c) {
			continue
		}
		for i, k := range ks {
			if lessDistance(DeviceID(c.DeviceID), self, DeviceID(k)) {
				closer[i]++
			}
		}
	}
	if closer[0] < K || closer[1] < K || closer[2] < K {
		t.Fatalf("test setup: closer cards per key %v", closer)
	}
	s := newServer(t, func(c *ServerConfig) { c.Self = self; c.Table = tbl })
	if r := s.Store(testPeer("192.0.2.0/24"), rec); r.Err != CodeWrongKey {
		t.Fatalf("Store on a node that is not a replica = %+v, want ERROR wrong-key", r)
	}
	if s.Len() != 0 {
		t.Fatal("a non-replica stored the record")
	}
}

func TestSignerQuotaUnknownLabels(t *testing.T) {
	sg := seededSigner(t, 7)
	ks := newKeySet()
	s := newServer(t, ks.config)
	labels := []string{"echofog/test/a", "echofog/test/b", "echofog/test/c", "echofog/test/d", "echofog/test/e"}
	for _, l := range labels {
		ks.add(dayKey(l, sg, srvNow))
	}
	stored := 0
	at := srvNow
	for _, l := range labels {
		at = at.Add(time.Second)
		r := s.StoreAt(testPeer("192.0.2.0/24"), unknownRecord(t, l, sg, 1, expiry(time.Hour)), at)
		switch r.Status {
		case StatusStored:
			stored++
		case StatusQuota:
		default:
			t.Fatalf("%s: %+v", l, r)
		}
	}
	if stored != 4 {
		t.Fatalf("unknown labels per signer: stored %d, want 4", stored)
	}
	s.checkInvariants(t)
}

func TestSessionAndUnitQuotas(t *testing.T) {
	fill := func(t *testing.T, s *Server, ks *keySet, p Peer, n int, seed0 uint64, at *time.Time) Result {
		var r Result
		for i := 0; i < n; i++ {
			sg := seededSigner(t, seed0+uint64(i))
			ks.add(dayKey("echofog/test/q", sg, srvNow))
			*at = at.Add(time.Second)
			r = s.StoreAt(p, unknownRecord(t, "echofog/test/q", sg, 1, expiry(time.Hour)), *at)
			if r.Status != StatusStored {
				t.Fatalf("record %d: %+v", i, r)
			}
		}
		return r
	}
	t.Run("session 64", func(t *testing.T) {
		ks := newKeySet()
		s := newServer(t, ks.config)
		p := testPeer("192.0.2.0/24")
		at := srvNow
		fill(t, s, ks, p, 64, 100, &at)
		sg := seededSigner(t, 999)
		ks.add(dayKey("echofog/test/q", sg, srvNow))
		if r := s.StoreAt(p, unknownRecord(t, "echofog/test/q", sg, 1, expiry(time.Hour)), at.Add(time.Second)); r.Status != StatusQuota {
			t.Fatalf("65th record of a session: %+v, want quota", r)
		}
		s.checkInvariants(t)
	})
	t.Run("server session 1024", func(t *testing.T) {
		ks := newKeySet()
		s := newServer(t, ks.config)
		p := testPeer("192.0.2.0/24")
		p.Server = true
		at := srvNow
		fill(t, s, ks, p, 70, 2000, &at)
	})
	t.Run("unit 3x", func(t *testing.T) {
		ks := newKeySet()
		s := newServer(t, ks.config)
		var peers [4]Peer
		for i := range peers {
			peers[i] = testPeer("203.0.113.0/24")
		}
		at := srvNow
		for i := 0; i < 3; i++ {
			fill(t, s, ks, peers[i], 64, uint64(5000+i*100), &at)
		}
		sg := seededSigner(t, 9000)
		ks.add(dayKey("echofog/test/q", sg, srvNow))
		if r := s.StoreAt(peers[3], unknownRecord(t, "echofog/test/q", sg, 1, expiry(time.Hour)), at.Add(time.Second)); r.Status != StatusQuota {
			t.Fatalf("a 4th session of the same unit past 3x64: %+v, want quota", r)
		}
		s.checkInvariants(t)
	})
}

func TestStoreRateLimits(t *testing.T) {
	ks := newKeySet()
	s := newServer(t, ks.config)
	p := testPeer("192.0.2.0/24")
	at := srvNow
	var recs [][]byte
	for i := 0; i < 3; i++ {
		sg := seededSigner(t, uint64(300+i))
		ks.add(dayKey("echofog/test/r", sg, srvNow))
		recs = append(recs, unknownRecord(t, "echofog/test/r", sg, 1, expiry(time.Hour)))
	}
	if r := s.StoreAt(p, recs[0], at); r.Status != StatusStored {
		t.Fatal(r)
	}
	if r := s.StoreAt(p, recs[1], at); r.Status != StatusStored {
		t.Fatal(r)
	}
	if r := s.StoreAt(p, recs[2], at); r.Status != StatusQuota {
		t.Fatalf("third STORE in one second: %+v, want quota", r)
	}
	if r := s.StoreAt(p, recs[2], at.Add(time.Second)); r.Status != StatusStored {
		t.Fatalf("after a second: %+v", r)
	}
}

func TestStoreHourlyLimit(t *testing.T) {
	s := newServer(t, respondAll)
	p := testPeer("192.0.2.0/24")
	p.Server = true // 1024 per session, so the hourly 300 is what binds
	sg := seededSigner(t, 400)
	at := srvNow
	for i := 0; i < 300; i++ {
		at = at.Add(600 * time.Millisecond) // under 2/s
		r := s.StoreAt(p, unknownRecord(t, "echofog/test/h", sg, uint64(i+1), expiry(2*time.Hour)), at)
		if r.Status == StatusQuota {
			t.Fatalf("STORE %d refused below the hourly limit", i)
		}
	}
	at = at.Add(600 * time.Millisecond)
	if r := s.StoreAt(p, unknownRecord(t, "echofog/test/h", sg, 301, expiry(2*time.Hour)), at); r.Status != StatusQuota {
		t.Fatalf("301st STORE inside an hour: %+v, want quota", r)
	}
}

func TestRequestRate(t *testing.T) {
	s := newServer(t, respondAll)
	sid := testPeer("").SID
	ok := 0
	for i := 0; i < 100; i++ {
		if s.Allow(sid, srvNow) {
			ok++
		}
	}
	if ok != 60 {
		t.Fatalf("burst allowed %d requests, want 60", ok)
	}
	at := srvNow.Add(100 * time.Millisecond)
	if !s.Allow(sid, at) || !s.Allow(sid, at) {
		t.Fatal("20 requests per second should refill 2 tokens in 100 ms")
	}
	if s.Allow(sid, at) {
		t.Fatal("third request after 100 ms should be limited")
	}
}

func TestFullStoreEvictsFarthestAndRefusesFarther(t *testing.T) {
	self := DeviceID{}
	ks := newKeySet()
	s := newServer(t, func(c *ServerConfig) { ks.config(c); c.Self = self; c.MaxRecords = 20 })
	type item struct {
		key [32]byte
		rec []byte
	}
	var items []item
	for i := 0; i < 40; i++ {
		sg := seededSigner(t, uint64(7000+i))
		items = append(items, item{dayKey("echofog/test/f", sg, srvNow), unknownRecord(t, "echofog/test/f", sg, 1, expiry(time.Hour))})
	}
	// Self is the zero ID, so distance is the key itself: sort ascending = nearest first.
	sort.Slice(items, func(i, j int) bool { return idLess(items[i].key, items[j].key) })
	for _, it := range items {
		ks.add(it.key)
	}
	at := srvNow
	put := func(rec []byte) Result {
		at = at.Add(time.Second)
		p := testPeer("")
		p.Server = true
		return s.StoreAt(p, rec, at)
	}
	for _, it := range items[20:] { // the 20 farthest fill the store
		if r := put(it.rec); r.Status != StatusStored {
			t.Fatalf("fill: %+v", r)
		}
	}
	if s.Len() != 20 {
		t.Fatalf("Len = %d", s.Len())
	}
	for _, it := range items[:20] { // nearer records displace the farthest
		if r := put(it.rec); r.Status != StatusStored {
			t.Fatalf("nearer record into a full store: %+v", r)
		}
	}
	for _, it := range items[:20] {
		if got, _ := s.FindValue(it.key); got == nil {
			t.Fatal("a near record was displaced")
		}
	}
	// Far spam (farther than every stored key) neither enters nor displaces.
	refused := 0
	for i := 0; i < 400 && refused < 10; i++ {
		sg := seededSigner(t, uint64(8000+i))
		key := dayKey("echofog/test/f", sg, srvNow)
		if !idLess(items[19].key, key) {
			continue
		}
		ks.add(key)
		if r := put(unknownRecord(t, "echofog/test/f", sg, 1, expiry(time.Hour))); r.Status != StatusQuota {
			t.Fatalf("far spam into a full store: %+v, want quota", r)
		}
		refused++
	}
	if refused == 0 {
		t.Fatal("test setup produced no far keys")
	}
	for _, it := range items[:20] {
		if got, _ := s.FindValue(it.key); got == nil {
			t.Fatal("far spam displaced a near record")
		}
	}
	if s.Len() != 20 {
		t.Fatalf("Len = %d, want 20", s.Len())
	}
	s.checkInvariants(t)
}

// AD19: one logical expiry index, accounting changed once. Records that expire in the same instant
// and a second Expire pass must not deduct twice.
func TestExpiryAccountingOnce(t *testing.T) {
	now := srvNow
	ks := newKeySet()
	s := newServer(t, func(c *ServerConfig) { ks.config(c); c.Now = func() time.Time { return now } })
	p := testPeer("192.0.2.0/24")
	for i := 0; i < 10; i++ {
		sgi := seededSigner(t, uint64(600+i))
		ks.add(dayKey("echofog/test/e", sgi, srvNow))
		at := srvNow.Add(time.Duration(i) * time.Second)
		if r := s.StoreAt(p, unknownRecord(t, "echofog/test/e", sgi, 1, expiry(time.Hour)), at); r.Status != StatusStored {
			t.Fatalf("%d: %+v", i, r)
		}
	}
	now = srvNow.Add(2 * time.Hour)
	s.Expire(now)
	s.Expire(now) // a second pass is a no-op
	if s.Len() != 0 {
		t.Fatalf("Len = %d after expiry", s.Len())
	}
	s.checkInvariants(t)
	// The session count is back at zero: the full 64 fit again.
	for i := 0; i < 64; i++ {
		sgi := seededSigner(t, uint64(700+i))
		ks.add(dayKey("echofog/test/e", sgi, now))
		if r := s.StoreAt(p, unknownRecord(t, "echofog/test/e", sgi, 1, now.Add(time.Hour)), now.Add(time.Duration(i)*time.Second)); r.Status != StatusStored {
			t.Fatalf("after expiry %d: %+v", i, r)
		}
	}
	s.checkInvariants(t)
}

// A replacement by another session moves ownership, and the old owner is deducted once; a later
// expiry deducts the new owner once.
func TestReplacementAndExpiryDoNotDoubleDeduct(t *testing.T) {
	now := srvNow
	sg := seededSigner(t, 77)
	ks := newKeySet()
	key := ks.add(dayKey("echofog/test/d", sg, srvNow))
	s := newServer(t, func(c *ServerConfig) { ks.config(c); c.Now = func() time.Time { return now } })
	p := testPeer("192.0.2.0/24")
	q := testPeer("192.0.2.0/24")
	if r := s.StoreAt(p, unknownRecord(t, "echofog/test/d", sg, 1, expiry(time.Hour)), now); r.Status != StatusStored {
		t.Fatal(r)
	}
	if r := s.StoreAt(q, unknownRecord(t, "echofog/test/d", sg, 2, expiry(2*time.Hour)), now.Add(time.Second)); r.Status != StatusStored {
		t.Fatal(r)
	}
	s.checkInvariants(t)
	now = srvNow.Add(3 * time.Hour)
	s.Expire(now)
	if got, _ := s.FindValue(key); got != nil || s.Len() != 0 {
		t.Fatal("record outlived its expiry")
	}
	s.checkInvariants(t)
}

// --- FIND_NODE / FIND_VALUE answers ---

func TestFindNodeAnswersOnlyReachedCards(t *testing.T) {
	reached := publicCard(t, 11, 1, objects.ScopePublic, 0)
	victim := publicCard(t, 12, 1, objects.ScopePublic, 0) // learned from DHT/PEX, never reached
	peer := publicCard(t, 14, 1, objects.ScopePeer, 0)
	s := newServer(t, func(c *ServerConfig) {
		c.Reached = func(time.Time) []*objects.Card { return []*objects.Card{reached, peer} }
	})
	got := s.FindNode([32]byte{1})
	if len(got) != 1 || !bytes.Equal(got[0], reached.Env.Raw) {
		t.Fatalf("FIND_NODE answered %d cards, want only the reached public one", len(got))
	}
	for _, raw := range got {
		if bytes.Equal(raw, victim.Env.Raw) {
			t.Fatal("a card nobody reached was passed on")
		}
	}
}

func TestFindNodeIsClosestFirstAndCapped(t *testing.T) {
	var all []*objects.Card
	for i := 0; i < 30; i++ {
		all = append(all, publicCard(t, uint64(20+i), 1, objects.ScopePublic, 0))
	}
	s := newServer(t, func(c *ServerConfig) { c.Reached = func(time.Time) []*objects.Card { return all } })
	target := [32]byte{0xab, 0xcd}
	got := s.FindNode(target)
	if len(got) != K {
		t.Fatalf("answered %d cards, want %d", len(got), K)
	}
	ids := make([]DeviceID, 0, len(all))
	byID := map[DeviceID][]byte{}
	for _, c := range all {
		ids = append(ids, DeviceID(c.DeviceID))
		byID[DeviceID(c.DeviceID)] = c.Env.Raw
	}
	sort.Slice(ids, func(i, j int) bool { return lessDistance(ids[i], ids[j], DeviceID(target)) })
	for i := 0; i < K; i++ {
		if !bytes.Equal(got[i], byID[ids[i]]) {
			t.Fatalf("card %d is not the %dth closest", i, i)
		}
	}
}

func TestNodesPayloadTrimDropsFarthest(t *testing.T) {
	var cards [][]byte
	for i := 0; i < 20; i++ {
		cards = append(cards, bytes.Repeat([]byte{byte(i)}, 4000))
	}
	kept := trimCards(cards, MaxNodesPayload)
	if len(kept) == 0 || len(kept) >= 20 {
		t.Fatalf("kept %d of 20 cards of 4000 bytes", len(kept))
	}
	for i := range kept {
		if !bytes.Equal(kept[i], cards[i]) {
			t.Fatal("trim must drop from the far end, keeping the closest in order")
		}
	}
	if n := nodesSize(kept); n > MaxNodesPayload {
		t.Fatalf("payload %d exceeds the frame maximum", n)
	}
}

// --- popular-record cache ---

func TestCacheIsServedButNotCounted(t *testing.T) {
	sg := seededSigner(t, 900)
	rec := hintRecord(t, sg, 1, expiry(time.Hour))
	s := newServer(t, respondAll)
	if !s.Cache(rec) {
		t.Fatal("a valid record was not cached")
	}
	key := dayKey(objects.LabelMailboxHint, sg, srvNow)
	if got, _ := s.FindValue(key); !bytes.Equal(got, rec) {
		t.Fatal("cached record not served")
	}
	if s.Len() != 0 {
		t.Fatalf("cached records count as replicas: Len = %d", s.Len())
	}
	if s.CacheLen() == 0 {
		t.Fatal("CacheLen = 0")
	}
}

func TestCacheLimits(t *testing.T) {
	check := func(t *testing.T, memCap, want int) {
		s := newServer(t, func(c *ServerConfig) { c.MemCapMB = memCap })
		for i := 0; i < want+50; i++ {
			s.Cache(unknownRecord(t, "echofog/test/c", seededSigner(t, uint64(10000+i)), 1, expiry(time.Hour)))
		}
		// Each record is cached under the keys of three days.
		if n := s.CacheLen(); n > want {
			t.Fatalf("cache holds %d, want at most %d", n, want)
		}
		s.checkInvariants(t)
	}
	t.Run("default 1000", func(t *testing.T) { check(t, 0, 1000) })
	t.Run("mem_cap 200", func(t *testing.T) { check(t, 64, 200) })
}

func TestCacheNeverShadowsStored(t *testing.T) {
	sg := seededSigner(t, 901)
	ks := newKeySet()
	key := ks.add(dayKey("echofog/test/s", sg, srvNow))
	s := newServer(t, ks.config)
	cached := unknownRecord(t, "echofog/test/s", sg, 1, expiry(time.Hour))
	s.Cache(cached)
	newer := unknownRecord(t, "echofog/test/s", sg, 2, expiry(time.Hour))
	if r := s.Store(testPeer("192.0.2.0/24"), newer); r.Status != StatusStored {
		t.Fatal(r)
	}
	if got, _ := s.FindValue(key); !bytes.Equal(got, newer) {
		t.Fatal("a stale cached copy shadows the stored record")
	}
	s.checkInvariants(t)
}
