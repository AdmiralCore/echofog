package session_test

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/session"
)

type obsClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *obsClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *obsClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type obsRig struct {
	o       *session.Observer
	clk     *obsClock
	mu      sync.Mutex
	changes []session.PublicAddrChange
	sus     int
}

func newObsRig(t *testing.T) *obsRig {
	t.Helper()
	r := &obsRig{clk: &obsClock{t: time.Unix(1_800_000_000, 0)}}
	r.o = session.NewObserver(session.ObserverConfig{
		Now: r.clk.Now,
		OnChange: func(c session.PublicAddrChange) {
			r.mu.Lock()
			r.changes = append(r.changes, c)
			r.mu.Unlock()
		},
		OnSuspicious: func([32]byte) {
			r.mu.Lock()
			r.sus++
			r.mu.Unlock()
		},
	})
	return r
}

func (r *obsRig) nChanges() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.changes)
}

func peerID(n byte) [32]byte { return [32]byte{n} }

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// obs builds an observation by peer n at address peerAddr that reports reported on socket.
func obs(n byte, peerAddr, reported, socket string) session.Observation {
	return session.Observation{
		Peer:     peerID(n),
		PeerAddr: netip.MustParseAddr(peerAddr),
		Reported: ap(reported),
		Socket:   socket,
	}
}

func wantPublic(t *testing.T, o *session.Observer, want string) {
	t.Helper()
	got, ok := o.PublicAddr()
	if want == "" {
		if ok {
			t.Fatalf("public address = %v, want none", got)
		}
		return
	}
	if !ok || got != netip.MustParseAddr(want) {
		t.Fatalf("public address = %v ok=%v, want %s", got, ok, want)
	}
}

func TestOneObservationIsNotEnough(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "")
}

func TestTwoPeersDifferentSlash16Decide(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
	if r.nChanges() != 1 {
		t.Fatalf("changes = %d, want 1", r.nChanges())
	}
	c := r.changes[0]
	if c.Old.IsValid() || c.New != netip.MustParseAddr("8.8.8.8") {
		t.Fatalf("change = %+v", c)
	}
}

func TestTwoPeersSameSlash16DoNotDecide(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.1.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "198.51.200.2", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(3, "198.51.77.3", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "")
	if r.nChanges() != 0 {
		t.Fatalf("changes = %d, want 0", r.nChanges())
	}
}

func TestIPv6GroupIsSlash32(t *testing.T) {
	r := newObsRig(t)
	// same /32, different /48: one group
	r.o.Observe(obs(1, "2001:db8:1::1", "[2001:db9::1]:5000", "s"))
	r.o.Observe(obs(2, "2001:db8:2::1", "[2001:db9::1]:5000", "s"))
	wantPublic(t, r.o, "")
	// a different /32 completes the quorum
	r.o.Observe(obs(3, "2001:dba::1", "[2001:db9::1]:5000", "s"))
	wantPublic(t, r.o, "2001:db9::1")
}

func TestOneLiarDoesNotChangeDecision(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
	r.o.Observe(obs(3, "100.64.0.1", "8.8.4.4:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
	if r.nChanges() != 1 {
		t.Fatalf("changes = %d, want 1", r.nChanges())
	}
	if r.sus != 1 {
		t.Fatalf("suspicious = %d, want 1", r.sus)
	}
}

func TestLiarFirstCannotForceAnAddress(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(3, "100.64.0.1", "8.8.4.4:4000", "s"))
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "") // 1 vs 1: no decision, and no tie-break by order
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
}

func TestLiarsInOneSlash16AreOneVote(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(3, "100.64.0.1", "8.8.4.4:4000", "s"))
	r.o.Observe(obs(4, "100.64.9.9", "8.8.4.4:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
	if r.nChanges() != 1 {
		t.Fatalf("changes = %d, want 1", r.nChanges())
	}
}

func TestTieDecidesNothingAndKeepsOldDecision(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
	// 2 peers for a new address in other groups: 2 vs 2 is a tie, the decision stays
	r.o.Observe(obs(3, "100.64.0.1", "9.9.9.9:4000", "s"))
	r.o.Observe(obs(4, "100.65.0.1", "9.9.9.9:4000", "s"))
	wantPublic(t, r.o, "8.8.8.8")
	if r.nChanges() != 1 {
		t.Fatalf("changes = %d, want 1", r.nChanges())
	}
}

func TestStrictMajorityChangesAddress(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	// the node moved: the same peers now report the new address
	r.clk.advance(session.MinChangeInterval + time.Second)
	r.o.Observe(obs(1, "198.51.100.1", "9.9.9.9:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "9.9.9.9:4000", "s"))
	wantPublic(t, r.o, "9.9.9.9")
	if r.nChanges() != 2 {
		t.Fatalf("changes = %d, want 2", r.nChanges())
	}
	if c := r.changes[1]; c.Old != netip.MustParseAddr("8.8.8.8") || c.New != netip.MustParseAddr("9.9.9.9") {
		t.Fatalf("change = %+v", c)
	}
}

func TestSameDecisionRepeatedFiresOnce(t *testing.T) {
	r := newObsRig(t)
	for i := 0; i < 5; i++ {
		r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
		r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	}
	if r.nChanges() != 1 {
		t.Fatalf("changes = %d, want 1", r.nChanges())
	}
}

func TestUnusableObservationsAreIgnored(t *testing.T) {
	r := newObsRig(t)
	bad := []session.Observation{
		obs(1, "198.51.100.1", "10.0.0.5:4000", "s"),    // private
		obs(2, "192.0.2.9", "127.0.0.1:4000", "s"),      // loopback
		obs(3, "100.64.0.1", "0.0.0.0:4000", "s"),       // unspecified
		obs(4, "198.51.101.1", "169.254.1.1:4000", "s"), // link local
		{Peer: peerID(5), Reported: ap("8.8.8.8:1")},    // no peer address (relay circuit)
		{Peer: peerID(6), PeerAddr: netip.MustParseAddr("127.0.0.1"), Reported: ap("8.8.8.8:1")},
		{Peer: peerID(7), PeerAddr: netip.MustParseAddr("192.0.2.77")}, // no reported address
	}
	for _, b := range bad {
		if v := r.o.Observe(b); v != session.ObsIgnored {
			t.Fatalf("verdict for %+v = %v, want ignored", b, v)
		}
	}
	wantPublic(t, r.o, "")
}

func TestObservationsExpire(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.clk.advance(session.ObservationTTL + time.Second)
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	wantPublic(t, r.o, "") // the first one is gone
}

func TestResetForgetsEverything(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Reset()
	wantPublic(t, r.o, "")
	if got := r.o.NAT("s"); got != session.NATUnknown {
		t.Fatalf("NAT after reset = %v", got)
	}
}

func TestObservationCapEvictsOldest(t *testing.T) {
	r := newObsRig(t)
	for i := 0; i < session.MaxObservations+50; i++ {
		r.clk.advance(time.Second)
		r.o.Observe(session.Observation{
			Peer:     [32]byte{byte(i), byte(i >> 8), 1},
			PeerAddr: netip.AddrFrom4([4]byte{byte(11 + i%200), byte(i / 200), 1, 1}),
			Reported: ap("8.8.8.8:4000"),
			Socket:   "s",
		})
	}
	if n := r.o.Len(); n > session.MaxObservations {
		t.Fatalf("held %d observations, cap %d", n, session.MaxObservations)
	}
}

func TestNATNeedsThreeObservations(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	if got := r.o.NAT("s"); got != session.NATUnknown {
		t.Fatalf("NAT with 2 observations = %v, want unknown", got)
	}
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:4000", "s"))
	if got := r.o.NAT("s"); got != session.NATCone {
		t.Fatalf("NAT with 3 equal observations = %v, want cone", got)
	}
}

func TestNATSymmetricNeedsThreeDistinctPorts(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4001", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4002", "s"))
	if got := r.o.NAT("s"); got != session.NATUnknown {
		t.Fatalf("NAT with 2 = %v", got)
	}
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:4003", "s"))
	if got := r.o.NAT("s"); got != session.NATSymmetric {
		t.Fatalf("NAT = %v, want symmetric", got)
	}
}

func TestOneLyingPortIsNotSymmetric(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:7777", "s"))
	if got := r.o.NAT("s"); got == session.NATSymmetric {
		t.Fatalf("one divergent port made the NAT symmetric")
	}
	// a fourth honest observation makes it cone again
	r.o.Observe(obs(4, "100.65.0.1", "8.8.8.8:4000", "s"))
	if got := r.o.NAT("s"); got != session.NATCone {
		t.Fatalf("NAT = %v, want cone (3 of 4)", got)
	}
}

func TestDivergentIPIsNotEvidenceOfSymmetricNAT(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(4, "100.65.0.1", "1.1.1.1:5555", "s"))
	if got := r.o.NAT("s"); got != session.NATCone {
		t.Fatalf("NAT = %v, want cone: the divergent IP is suspicious, not symmetric evidence", got)
	}
}

func TestNATNeedsOneLocalSocket(t *testing.T) {
	r := newObsRig(t)
	// each session on its own socket: a cone NAT shows different ports, that proves nothing
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4001", "a"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4002", "b"))
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:4003", "c"))
	for _, s := range []string{"a", "b", "c", ""} {
		if got := r.o.NAT(s); got != session.NATUnknown {
			t.Fatalf("NAT(%q) = %v, want unknown", s, got)
		}
	}
	if got := r.o.NATClass(); got != session.NATUnknown {
		t.Fatalf("NATClass = %v, want unknown", got)
	}
}

func TestNATSameGroupCountsOnce(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4001", "s"))
	r.o.Observe(obs(2, "198.51.100.2", "8.8.8.8:4002", "s"))
	r.o.Observe(obs(3, "198.51.100.3", "8.8.8.8:4003", "s"))
	if got := r.o.NAT("s"); got != session.NATUnknown {
		t.Fatalf("three peers of one /16 classified the NAT as %v", got)
	}
}

func TestNATClassUsesTheBestSocketOnly(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:4000", "s"))
	if got := r.o.NATClass(); got != session.NATCone {
		t.Fatalf("NATClass = %v, want cone", got)
	}
}

func TestParseObservedHandshakeValue(t *testing.T) {
	good := []byte{4, 203, 0, 113, 5, 0x0f, 0xa0}
	got, ok := session.ParseObserved(good)
	if !ok || got != netip.MustParseAddrPort("203.0.113.5:4000") {
		t.Fatalf("ParseObserved = %v %v", got, ok)
	}
	for _, bad := range [][]byte{nil, {4, 1, 2}, {5, 1, 2, 3, 4, 0, 1}, append(append([]byte(nil), good...), 0)} {
		if _, ok := session.ParseObserved(bad); ok {
			t.Fatalf("ParseObserved(%x) accepted", bad)
		}
	}
}

// TestRandomLiarsNeverOutvoteHonestNetworks: with 3 honest networks and one lying network (any
// number of liars inside it, any order), the decision is the honest address whenever it is made,
// and no divergent address is ever decided.
func TestRandomLiarsNeverOutvoteHonestNetworks(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for iter := 0; iter < 300; iter++ {
		r := newObsRig(t)
		type rep struct {
			n        byte
			addr, ip string
		}
		var reps []rep
		for i := 0; i < 3; i++ { // honest: one network each
			reps = append(reps, rep{byte(10 + i), fmt.Sprintf("198.%d.0.%d", 51+i, i+1), "8.8.8.8:4000"})
		}
		for i := 0; i < 1+rng.IntN(4); i++ { // liars: all inside 100.64/16
			reps = append(reps, rep{byte(50 + i), fmt.Sprintf("100.64.%d.%d", i, i+1),
				fmt.Sprintf("5.5.5.%d:4000", 100+rng.IntN(100))})
		}
		rng.Shuffle(len(reps), func(i, j int) { reps[i], reps[j] = reps[j], reps[i] })
		for _, x := range reps {
			r.o.Observe(obs(x.n, x.addr, x.ip, "s"))
			if got, ok := r.o.PublicAddr(); ok && got != netip.MustParseAddr("8.8.8.8") {
				t.Fatalf("iter %d: decided %v with %d reports", iter, got, len(reps))
			}
		}
		wantPublic(t, r.o, "8.8.8.8")
	}
}

// TestObserverConcurrentUse: Observe, PublicAddr, NAT, NATClass, Len and Reset from many
// goroutines (run under -race).
func TestObserverConcurrentUse(t *testing.T) {
	o := session.NewObserver(session.ObserverConfig{
		OnChange:     func(session.PublicAddrChange) {},
		OnSuspicious: func([32]byte) {},
	})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				o.Observe(session.Observation{
					Peer:     [32]byte{byte(g), byte(i % 7)},
					PeerAddr: netip.AddrFrom4([4]byte{byte(11 + g), byte(i % 5), 1, 1}),
					Reported: netip.AddrPortFrom(netip.AddrFrom4([4]byte{8, 8, 8, byte(8 - i%2*4)}), uint16(4000+i%3)),
					Socket:   "s",
				})
				_, _ = o.PublicAddr()
				_ = o.NAT("s")
				_ = o.NATClass()
				_ = o.Len()
				if i%97 == 0 {
					o.Reset()
				}
			}
		}()
	}
	wg.Wait()
}

func TestNATDetailPortRange(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:41500", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:40200", "s"))
	if d := r.o.NATDetail(); d.Class != session.NATUnknown || d.Lo != 0 || d.Hi != 0 {
		t.Fatalf("detail with 2 reports = %+v", d)
	}
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:40900", "s"))
	d := r.o.NATDetail()
	if d.Class != session.NATSymmetric || d.Lo != 40200 || d.Hi != 41500 || d.Samples != 3 {
		t.Fatalf("detail = %+v, want symmetric 40200..41500 from 3", d)
	}
}

func TestNATDetailConeHasNoRange(t *testing.T) {
	r := newObsRig(t)
	r.o.Observe(obs(1, "198.51.100.1", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(2, "192.0.2.9", "8.8.8.8:4000", "s"))
	r.o.Observe(obs(3, "100.64.0.1", "8.8.8.8:4000", "s"))
	if d := r.o.NATDetail(); d.Class != session.NATCone || d.Lo != 0 || d.Hi != 0 {
		t.Fatalf("detail = %+v", d)
	}
}
