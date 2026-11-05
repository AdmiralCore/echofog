package session_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/session"
)

func abuseLimits(t *testing.T, clk *obsClock) (*session.Limits, *session.AbuseLog) {
	t.Helper()
	log, err := session.NewAbuseLog(newMemKV())
	if err != nil {
		t.Fatal(err)
	}
	return newLimits(t, clk, func(c *session.LimitsConfig) {
		c.Abuse = log
		c.BanAfter = 3
		c.BanWindow = time.Minute
		c.BanFor = time.Hour
	}), log
}

// Abuse of an authenticated device is counted and, past the threshold, bans that device ID.
func TestAuthenticatedAbuseBansTheDevice(t *testing.T) {
	clk := newClock()
	l, log := abuseLimits(t, clk)
	bad, good := peerID(1), peerID(2)
	h := l.AbuseHandler(bad)
	for i := 0; i < 2; i++ {
		h(mux.Abuse{Type: 1})
	}
	if l.Banned(bad) {
		t.Fatal("banned below the threshold")
	}
	h(mux.Abuse{Type: 1})
	if !l.Banned(bad) {
		t.Fatal("not banned at the threshold")
	}
	if l.Banned(good) {
		t.Fatal("an innocent device is banned")
	}
	if n, _ := log.Count(bad); n != 3 {
		t.Fatalf("persisted abuse count = %d, want 3", n)
	}
	clk.advance(2 * time.Hour)
	if l.Banned(bad) {
		t.Fatal("the ban did not expire")
	}
}

// Findings far apart do not add up to a ban: the score is a window, not a lifetime.
func TestAbuseOutsideTheWindowDoesNotBan(t *testing.T) {
	clk := newClock()
	l, _ := abuseLimits(t, clk)
	h := l.AbuseHandler(peerID(1))
	for i := 0; i < 10; i++ {
		h(mux.Abuse{Type: 1})
		clk.advance(40 * time.Second)
	}
	if l.Banned(peerID(1)) {
		t.Fatal("slow, sparse findings must not ban")
	}
}

// A device ID that was never authenticated (all zero) is shared by every unauthenticated caller
// and is not a ban target.
func TestZeroDeviceIsNeverBanned(t *testing.T) {
	clk := newClock()
	l, _ := abuseLimits(t, clk)
	h := l.AbuseHandler([32]byte{})
	for i := 0; i < 50; i++ {
		h(mux.Abuse{Type: 1})
	}
	if l.Banned([32]byte{}) {
		t.Fatal("the zero ID was banned")
	}
}

func TestPuzzleSecretRotatesDaily(t *testing.T) {
	clk := newClock()
	l := newLimits(t, clk, nil)
	p0 := l.Puzzle()
	if len(p0.Secret) != 32 {
		t.Fatalf("R is %d bytes, want 32", len(p0.Secret))
	}
	if p0.PrevSecret != nil {
		t.Fatal("no previous R before the first rotation")
	}
	first := append([]byte(nil), p0.Secret...)
	clk.advance(23*time.Hour + 59*time.Minute)
	if p := l.Puzzle(); !bytes.Equal(p.Secret, first) {
		t.Fatal("R rotated before a day passed")
	}
	clk.advance(2 * time.Minute)
	p1 := l.Puzzle()
	if bytes.Equal(p1.Secret, first) {
		t.Fatal("R did not rotate after a day")
	}
	if !bytes.Equal(p1.PrevSecret, first) {
		t.Fatal("the previous R is not kept")
	}
	if p1.RotatedAt.IsZero() || p1.RotatedAt.After(clk.Now()) || clk.Now().Sub(p1.RotatedAt) > 3*time.Minute {
		t.Fatalf("RotatedAt = %v is not the rotation time (now %v)", p1.RotatedAt, clk.Now())
	}
	// The snapshot must not alias the live secret.
	p1.Secret[0] ^= 0xff
	if p2 := l.Puzzle(); p2.Secret[0] == p1.Secret[0] {
		t.Fatal("Puzzle() leaks the live secret slice")
	}
}
