package dht

import (
	"math/rand"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// TestRepublishDelayJitter (dht.md §3, AD13): the republication delay is half of the remaining
// lifetime with ± 20 % jitter.
func TestRepublishDelayJitter(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	remaining := 10 * time.Hour
	lo, hi := time.Duration(1<<62), time.Duration(0)
	for i := 0; i < 2000; i++ {
		d := halfLifeDelay(remaining, rnd)
		if d < time.Duration(float64(remaining/2)*0.8)-time.Second || d > time.Duration(float64(remaining/2)*1.2)+time.Second {
			t.Fatalf("delay %v outside half ± 20 %%", d)
		}
		if d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	if hi-lo < remaining/10 {
		t.Fatalf("delays do not spread: %v..%v", lo, hi)
	}
}

// TestRepublishKeepsBothDayKeysFilled: over the life of a 7-day card the publisher stores it in a
// window around every midnight (so that the next day's key is filled in the last 2 hours or the
// previous day's key still serves the first 2 hours), never twice within a minute, and stops at
// expiry.
func TestRepublishKeepsBothDayKeysFilled(t *testing.T) {
	rnd := rand.New(rand.NewSource(3))
	net := newFakeNet(t, 60, 30, false, rnd)
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	net.setClock(start)
	card := buildCard(t, 5151, 1, start, objects.ScopePublic, 0, testAddr("203.0.113.51", 4430))
	pub := newTestClient(t, net, 8300, rnd, nil)
	if err := pub.Publish(card.Env.Raw); err != nil {
		t.Fatal(err)
	}

	var times []time.Time
	end := time.Unix(int64(card.Env.NotAfter), 0)
	for now := start; now.Before(end.Add(36 * time.Hour)); now = now.Add(time.Minute) {
		net.setClock(now)
		before := len(net.storeLog)
		pub.Step(ctxT(t))
		if len(net.storeLog) != before {
			times = append(times, now)
		}
	}
	if len(times) < 6 {
		t.Fatalf("only %d publications over 6 days", len(times))
	}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < time.Minute {
			t.Fatalf("publications %v and %v are too close", times[i-1], times[i])
		}
	}
	// Every midnight inside the lifetime has a publication within [-2h, +2h].
	for b := start.Truncate(24 * time.Hour).Add(24 * time.Hour); b.Before(end); b = b.Add(24 * time.Hour) {
		ok := false
		for _, p := range times {
			if !p.Before(b.Add(-2*time.Hour)) && p.Before(b.Add(2*time.Hour)) {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("no publication within 2 h of %v; got %v", b, times)
		}
	}
	for _, p := range times {
		if !p.Before(end) {
			t.Fatalf("published at %v after the record expired at %v", p, end)
		}
	}
}
