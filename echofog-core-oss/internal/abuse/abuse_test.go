package abuse

import (
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestScorerBansPeerAboveThreshold(t *testing.T) {
	s := NewScorer(Config{})
	now := time.Unix(1000, 0)
	s.SetClock(func() time.Time { return now })
	peer := types.PeerID{1}
	ip := netip.MustParseAddr("203.0.113.5")

	for i := 0; i < 3; i++ {
		if v := s.Report(peer, ip, BadSignature); v.BanPeer {
			t.Fatalf("banned after %d reports", i+1)
		}
	}
	v := s.Report(peer, ip, BadSignature)
	if !v.BanPeer || v.PeerFor != time.Hour {
		t.Fatalf("4 bad signatures (100 points) should ban the peer for an hour, got %+v", v)
	}
	if v.BanAddr {
		t.Fatal("one peer alone must not ban its address")
	}
	if s.PeerScore(peer) != 0 {
		t.Fatal("a ban resets the score")
	}
}

func TestScorerDecays(t *testing.T) {
	s := NewScorer(Config{HalfLife: time.Minute})
	now := time.Unix(1000, 0)
	s.SetClock(func() time.Time { return now })
	peer := types.PeerID{2}
	for i := 0; i < 3; i++ {
		s.Report(peer, netip.Addr{}, BadSignature)
	}
	now = now.Add(time.Minute)
	if got := s.PeerScore(peer); got < 37 || got > 38 {
		t.Fatalf("75 points after one half-life should be 37.5, got %v", got)
	}
	now = now.Add(time.Hour)
	if v := s.Report(peer, netip.Addr{}, BadSignature); v.BanPeer {
		t.Fatal("old reports faded away; one more must not ban")
	}
}

func TestScorerAddrAddsUpIdentities(t *testing.T) {
	s := NewScorer(Config{})
	ip := netip.MustParseAddr("198.51.100.7")
	banned := false
	// a fresh identity per report: no single peer ever reaches its threshold
	for i := 0; i < 8; i++ {
		v := s.Report(types.PeerID{byte(10 + i)}, ip, BadSignature)
		if v.BanPeer {
			t.Fatal("no identity reported enough to be banned alone")
		}
		if v.BanAddr {
			banned = true
			if v.AddrFor != 15*time.Minute {
				t.Fatalf("address ban for %v", v.AddrFor)
			}
			break
		}
	}
	if !banned {
		t.Fatal("eight bad identities from one address should ban it")
	}
}

func TestScorerLeavesLoopbackAlone(t *testing.T) {
	s := NewScorer(Config{})
	lo := netip.MustParseAddr("127.0.0.1")
	for i := 0; i < 20; i++ {
		if v := s.Report(types.PeerID{byte(i)}, lo, BadSignature); v.BanAddr {
			t.Fatal("loopback banned")
		}
	}
}

func TestScorerCountsAFloodOncePerSecond(t *testing.T) {
	s := NewScorer(Config{})
	now := time.Unix(1000, 0)
	s.SetClock(func() time.Time { return now })
	peer := types.PeerID{3}
	for i := 0; i < 1000; i++ {
		if v := s.Report(peer, netip.Addr{}, Flood); v.BanPeer {
			t.Fatal("one burst of a thousand drops must not ban")
		}
	}
	if got := s.PeerScore(peer); got != Flood.Weight() {
		t.Fatalf("a burst scores once, got %v", got)
	}
	banned := false
	for i := 0; i < 60 && !banned; i++ {
		now = now.Add(time.Second)
		banned = s.Report(peer, netip.Addr{}, Flood).BanPeer
	}
	if !banned {
		t.Fatal("a flood that goes on for a minute should ban")
	}
}
