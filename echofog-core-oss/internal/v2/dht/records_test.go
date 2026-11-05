package dht

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// F1 (T5.1b return 1): one record is one quota slot, however many day keys point at it.

func allKeys(label string, seed uint64, t *testing.T, now time.Time) [][32]byte {
	sg := seededSigner(t, seed)
	d := objects.DHTDay(now)
	var out [][32]byte
	for _, day := range []uint32{d - 1, d, d + 1} {
		out = append(out, objects.DHTKey(label, signerID(sg), day))
	}
	return out
}

func quotaOf(s *Server, sg [32]byte, sid [16]byte, unit string) (known, unknown, session, units int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc := s.signers[sg]; sc != nil {
		known, unknown = sc.known, sc.unknown
	}
	return known, unknown, s.sessions[sid], s.units[unit]
}

func TestThreeLabelsOfOneSignerAllStoredCountersAreRecords(t *testing.T) {
	sg := seededSigner(t, 4100)
	s := newServer(t, respondAll)
	p := testPeer("192.0.2.0/24")
	at := srvNow
	for _, l := range []string{"echofog/test/a", "echofog/test/b", "echofog/test/c"} {
		at = at.Add(time.Second)
		if r := s.StoreAt(p, unknownRecord(t, l, sg, 1, expiry(time.Hour)), at); r.Status != StatusStored {
			t.Fatalf("%s: %+v (signer limit for unknown labels is 4)", l, r)
		}
	}
	// A known label of the same signer still fits too.
	at = at.Add(time.Second)
	if r := s.StoreAt(p, hintRecord(t, sg, 1, expiry(time.Hour)), at); r.Status != StatusStored {
		t.Fatalf("hint: %+v", r)
	}
	kn, unk, sess, un := quotaOf(s, signerID(sg), p.SID, p.Unit.String())
	if kn != 1 || unk != 3 || sess != 4 || un != 4 {
		t.Fatalf("counters known=%d unknown=%d session=%d unit=%d, want 1/3/4/4", kn, unk, sess, un)
	}
	if s.Len() != 4 {
		t.Fatalf("Len = %d, want 4 records", s.Len())
	}
	for _, k := range allKeys("echofog/test/a", 4100, t, srvNow) {
		if got, _ := s.FindValue(k); got == nil {
			t.Fatal("a day key does not find the record")
		}
	}
	s.checkInvariants(t)
}

func TestSessionLimitCountsRecordsNotKeys(t *testing.T) {
	s := newServer(t, respondAll)
	p := testPeer("192.0.2.0/24")
	at := srvNow
	for i := 0; i < SessionRecords; i++ {
		at = at.Add(time.Second)
		sg := seededSigner(t, uint64(4200+i))
		if r := s.StoreAt(p, unknownRecord(t, "echofog/test/s", sg, 1, expiry(time.Hour)), at); r.Status != StatusStored {
			t.Fatalf("record %d of %d: %+v", i, SessionRecords, r)
		}
	}
	at = at.Add(time.Second)
	if r := s.StoreAt(p, unknownRecord(t, "echofog/test/s", seededSigner(t, 4999), 1, expiry(time.Hour)), at); r.Status != StatusQuota {
		t.Fatalf("record %d: %+v, want quota", SessionRecords+1, r)
	}
	s.checkInvariants(t)
}

func TestExpiryOfThreeKeyRecordDeductsOnce(t *testing.T) {
	now := srvNow
	s := newServer(t, func(c *ServerConfig) { respondAll(c); c.Now = func() time.Time { return now } })
	p := testPeer("192.0.2.0/24")
	var sgs [][32]byte
	for i := 0; i < 3; i++ {
		sg := seededSigner(t, uint64(4300+i))
		sgs = append(sgs, signerID(sg))
		if r := s.StoreAt(p, unknownRecord(t, "echofog/test/x", sg, 1, expiry(time.Hour)), srvNow.Add(time.Duration(i)*time.Second)); r.Status != StatusStored {
			t.Fatal(r)
		}
	}
	now = srvNow.Add(2 * time.Hour)
	s.Expire(now)
	s.Expire(now)
	if s.Len() != 0 {
		t.Fatalf("Len = %d", s.Len())
	}
	if _, _, sess, un := quotaOf(s, sgs[0], p.SID, p.Unit.String()); sess != 0 || un != 0 {
		t.Fatalf("session=%d unit=%d after expiry, want 0/0", sess, un)
	}
	s.checkInvariants(t)
	// The whole quota is available again.
	for i := 0; i < SessionRecords; i++ {
		sg := seededSigner(t, uint64(4400+i))
		if r := s.StoreAt(p, unknownRecord(t, "echofog/test/x", sg, 1, now.Add(time.Hour)), now.Add(time.Duration(i)*time.Second)); r.Status != StatusStored {
			t.Fatalf("after expiry %d: %+v", i, r)
		}
	}
	s.checkInvariants(t)
}

func TestEvictionOfThreeKeyRecordDeductsOnce(t *testing.T) {
	s := newServer(t, func(c *ServerConfig) { respondAll(c); c.Self = DeviceID{}; c.MaxRecords = 2 })
	p := testPeer("")
	p.Server = true
	at := srvNow
	var seeds []uint64
	for i := 0; i < 8; i++ {
		seeds = append(seeds, uint64(4500+i))
		at = at.Add(time.Second)
		s.StoreAt(p, unknownRecord(t, "echofog/test/v", seededSigner(t, seeds[i]), 1, expiry(time.Hour)), at)
		if s.Len() > 2 {
			t.Fatalf("Len = %d, cap 2", s.Len())
		}
		s.checkInvariants(t)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	s.mu.Lock()
	sess := s.sessions[p.SID]
	s.mu.Unlock()
	if sess != 2 {
		t.Fatalf("session count %d, want 2 records", sess)
	}
	// Every key of an evicted record is gone, every key of a kept one still answers.
	found := 0
	for _, seed := range seeds {
		n := 0
		keys := allKeys("echofog/test/v", seed, t, srvNow)
		for _, k := range keys {
			if got, _ := s.FindValue(k); got != nil {
				n++
			}
		}
		if n != 0 && n != 3 {
			t.Fatalf("record %d answers under %d of 3 keys", seed, n)
		}
		if n == 3 {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("%d records answer, want 2", found)
	}
}

func TestReplacingThreeKeyRecordMovesOwnerOnce(t *testing.T) {
	now := srvNow
	sg := seededSigner(t, 4600)
	s := newServer(t, func(c *ServerConfig) { respondAll(c); c.Now = func() time.Time { return now } })
	p, q := testPeer("192.0.2.0/24"), testPeer("198.51.100.0/24")
	if r := s.StoreAt(p, unknownRecord(t, "echofog/test/r", sg, 1, expiry(time.Hour)), now); r.Status != StatusStored {
		t.Fatal(r)
	}
	if r := s.StoreAt(q, unknownRecord(t, "echofog/test/r", sg, 2, expiry(2*time.Hour)), now.Add(time.Second)); r.Status != StatusStored {
		t.Fatal(r)
	}
	_, unk, sp, _ := quotaOf(s, signerID(sg), p.SID, p.Unit.String())
	_, _, sq, uq := quotaOf(s, signerID(sg), q.SID, q.Unit.String())
	if unk != 1 || sp != 0 || sq != 1 || uq != 1 || s.Len() != 1 {
		t.Fatalf("unknown=%d old session=%d new session=%d new unit=%d Len=%d, want 1/0/1/1/1", unk, sp, sq, uq, s.Len())
	}
	s.checkInvariants(t)
	now = srvNow.Add(3 * time.Hour)
	s.Expire(now)
	if s.Len() != 0 {
		t.Fatal("record outlived expiry")
	}
	s.checkInvariants(t)
}
