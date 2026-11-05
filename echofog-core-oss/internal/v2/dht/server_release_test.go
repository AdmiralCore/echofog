package dht

import (
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

func releaseCheckFor(trusted [32]byte) func([]byte, time.Time) error {
	return func(raw []byte, now time.Time) error {
		env, err := objects.Verify(raw, now)
		if err != nil {
			return err
		}
		if env.SignerID() != trusted {
			return errors.New("not a release key")
		}
		return nil
	}
}

// TestStoreRefusesReleaseFromUntrustedSigner (T5.2 G1): the day key of a release does not depend on
// the signer. A storing node that accepted any signer's object with a huge seq would then refuse the
// genuine manifest as stale. With the check, the forgery is not stored and the genuine one is.
func TestStoreRefusesReleaseFromUntrustedSigner(t *testing.T) {
	trusted, attacker := seededSigner(t, 1200), seededSigner(t, 1201)
	s := newServer(t, func(c *ServerConfig) {
		respondAll(c)
		c.ReleaseCheck = releaseCheckFor(signerID(trusted))
	})
	fake := releaseRecord(t, attacker, 1<<40, srvNow.Add(time.Hour))
	real := releaseRecord(t, trusted, 5, srvNow.Add(time.Hour))

	if res := s.Store(testPeer("10.1.1.0/24"), fake); res.Status != StatusInvalid || res.Err != 0 {
		t.Fatalf("forged release: %+v, want invalid", res)
	}
	if s.Len() != 0 {
		t.Fatal("a forged release occupies a slot")
	}
	if res := s.Store(testPeer("10.2.2.0/24"), real); res.Status != StatusStored {
		t.Fatalf("genuine release after the forgery: %+v", res)
	}
	got, _ := s.FindValue(objects.ReleaseDHTKey(objects.DHTDay(srvNow)))
	if string(got) != string(real) {
		t.Fatal("the genuine release is not served")
	}
	s.checkInvariants(t)
}

// TestStoreRefusesReleaseWithoutACheck: a node that was given no release keys cannot judge, so it
// refuses release/2 objects (the strictest reading) and caches none either.
func TestStoreRefusesReleaseWithoutACheck(t *testing.T) {
	trusted := seededSigner(t, 1210)
	s := newServer(t, respondAll)
	rec := releaseRecord(t, trusted, 5, srvNow.Add(time.Hour))
	if res := s.Store(testPeer("10.1.1.0/24"), rec); res.Status != StatusRefused {
		t.Fatalf("release without a check: %+v, want refused", res)
	}
	if s.Cache(rec) {
		t.Fatal("a release was cached without a check")
	}
	if s.Len() != 0 || s.CacheLen() != 0 {
		t.Fatal("something was kept")
	}
}

func TestCacheRefusesForgedRelease(t *testing.T) {
	trusted, attacker := seededSigner(t, 1220), seededSigner(t, 1221)
	s := newServer(t, func(c *ServerConfig) {
		respondAll(c)
		c.ReleaseCheck = releaseCheckFor(signerID(trusted))
	})
	if s.Cache(releaseRecord(t, attacker, 1<<40, srvNow.Add(time.Hour))) {
		t.Fatal("a forged release was cached")
	}
	if !s.Cache(releaseRecord(t, trusted, 3, srvNow.Add(time.Hour))) {
		t.Fatal("a genuine release was not cached")
	}
}

// TestPutOfAForgedReleaseStoresNowhere: through the client, against honest servers.
func TestPutOfAForgedReleaseStoresNowhere(t *testing.T) {
	rnd := newRand(141)
	net := newFakeNet(t, 60, 30, false, rnd)
	trusted, attacker := seededSigner(t, 1230), seededSigner(t, 1231)
	net.relCheck = releaseCheckFor(signerID(trusted))
	pub := newTestClient(t, net, 8700, rnd, nil)
	if res, err := pub.Put(ctxT(t), releaseRecord(t, attacker, 1<<40, srvNow.Add(time.Hour))); err == nil || res.Stored != 0 {
		t.Fatalf("forged release stored: %+v %v", res, err)
	}
	if res, err := pub.Put(ctxT(t), releaseRecord(t, trusted, 5, srvNow.Add(time.Hour))); err != nil || res.Stored != Replicas(objects.LabelRelease) {
		t.Fatalf("genuine release: %+v %v", res, err)
	}
	reader := newTestClient(t, net, 8701, rnd, nil)
	env, err := reader.GetRelease(ctxT(t), releaseCheckFor(signerID(trusted)))
	if err != nil || env.Seq != 5 {
		t.Fatalf("reader: %v %v", env, err)
	}
}
