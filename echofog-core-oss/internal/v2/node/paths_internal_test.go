package node

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/session"
)

func attemptTo(host string) carrier.Attempt {
	return carrier.Attempt{
		Kind:  carrier.AttemptCarrier,
		Suite: objects.SuiteNoise3,
		Addr:  &pb.Addr{Host: host, Port: 443, Suite: objects.SuiteNoise3},
	}
}

// TestReserveAttemptsAppliesH11 pins H11 on the first dial: attempts to a pair that already has
// two carriers are not started at all, while another address is still dialed.
func TestReserveAttemptsAppliesH11(t *testing.T) {
	r := &Runtime{policy: session.NewPathPolicy(session.PathPolicyConfig{})}
	full := session.Pair{Host: "203.0.113.10"}
	for i := 0; i < 2; i++ {
		if err := r.policy.Acquire(full); err != nil {
			t.Fatal(err)
		}
	}
	attempts := []carrier.Attempt{
		attemptTo("203.0.113.10"),
		attemptTo("203.0.113.10"),
		attemptTo("198.51.100.7"),
	}
	got, reserved, err := r.reserveAttempts(attempts)
	if err != nil {
		t.Fatalf("reserveAttempts: %v", err)
	}
	if len(got) != 1 || pairOfAttempt(got[0]).Host != "198.51.100.7" {
		t.Fatalf("allowed attempts %v, want only the other address", got)
	}
	if len(reserved) != 1 || reserved[0].Host != "198.51.100.7" {
		t.Fatalf("reserved %v", reserved)
	}
	r.settleReservations(reserved, reserved[0])
	if n := r.policy.Open(full); n != 2 {
		t.Fatalf("policy still counts %d carriers of the full pair, want 2", n)
	}
}

// TestReserveAttemptsRespectsBackoff pins H15 before the handshake: an attempt to a backed-off
// pair is never started.
func TestReserveAttemptsRespectsBackoff(t *testing.T) {
	r := &Runtime{policy: session.NewPathPolicy(session.PathPolicyConfig{})}
	pair := session.Pair{Host: "203.0.113.10"}
	if err := r.policy.Acquire(pair); err != nil {
		t.Fatal(err)
	}
	r.policy.Penalize(pair)
	_, _, err := r.reserveAttempts([]carrier.Attempt{attemptTo("203.0.113.10")})
	var backoff *session.PathBackoffError
	if !errors.As(err, &backoff) {
		t.Fatalf("reserveAttempts during backoff: %v, want PathBackoffError", err)
	}
}

// TestRecordPairRejectsDeadPath pins the recordPair race fix: a path that failed (or a session
// that closed) before its pair was recorded cannot leave a stale H11/H15 slot.
func TestRecordPairRejectsDeadPath(t *testing.T) {
	r := &Runtime{
		policy:    session.NewPathPolicy(session.PathPolicyConfig{}),
		pathPairs: map[pathKey]session.Pair{},
		deadPaths: map[pathKey]struct{}{},
	}
	var sid [16]byte
	sid[0] = 7
	r.pathFailed(mux.PathFailure{SID: sid, PathID: 2, Cause: mux.ErrPathFailed})
	open := make(chan struct{})
	if r.recordPair(sid, 2, session.Pair{}, open) {
		t.Fatal("recorded a pair of a path that already failed")
	}
	if !r.recordPair(sid, 4, session.Pair{}, open) {
		t.Fatal("refused a live path")
	}
	closed := make(chan struct{})
	close(closed)
	if r.recordPair(sid, 6, session.Pair{}, closed) {
		t.Fatal("recorded a pair of a closed session")
	}
}

// TestPathAttemptsPrefersAnotherFamily pins the T3.3 "other address / IPv6" choice: a v6 address
// is preferred over an unused v4 one, and the address of the current path goes last.
func TestPathAttemptsPrefersAnotherFamily(t *testing.T) {
	var sid [16]byte
	sid[0] = 9
	r := &Runtime{pathPairs: map[pathKey]session.Pair{
		{sid: sid, id: 0}: {Host: "192.0.2.1"},
	}}
	est := &session.Established{SID: sid, Remote: netip.MustParseAddrPort("192.0.2.1:443")}
	attempts := []carrier.Attempt{
		attemptTo("192.0.2.1"),
		attemptTo("2001:db8::1"),
		attemptTo("198.51.100.7"),
	}
	got := r.pathAttempts(est, attempts)
	want := []string{"2001:db8::1", "198.51.100.7", "192.0.2.1"}
	if len(got) != len(want) {
		t.Fatalf("attempts %v, want %v", got, want)
	}
	for i := range want {
		if h := attemptHost(got[i].Addr); h != want[i] {
			t.Fatalf("attempt %d is %q, want %q (order %v)", i, h, want[i], got)
		}
	}
}
