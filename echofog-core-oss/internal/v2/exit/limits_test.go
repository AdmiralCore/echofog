package exit

import (
	"testing"
	"time"
)

func TestTCPLimiterConnectionRate(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	l := newTCPLimiter(20, 256)
	node := [32]byte{1}
	for i := 0; i < 20; i++ {
		if !l.allow(now, node, "1.1.1.1:443") {
			t.Fatalf("connection %d refused", i+1)
		}
	}
	if l.allow(now, node, "1.1.1.1:443") {
		t.Fatal("21st connection in one second allowed")
	}
	// A second later the bucket has one token again.
	now = now.Add(time.Second)
	if !l.allow(now, node, "1.1.1.1:443") {
		t.Fatal("connection after refill refused")
	}
	// Another identity has its own bucket.
	if !l.allow(now, [32]byte{2}, "1.1.1.1:443") {
		t.Fatal("second identity refused")
	}
}

func TestTCPLimiterDistinctTargets(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	l := newTCPLimiter(20, 256)
	node := [32]byte{1}
	target := func(i int) string { return "10.0.0." + itoa(i) + ":443" }
	for i := 0; i < 256; i++ {
		if i > 0 && i%20 == 0 {
			now = now.Add(time.Second)
		}
		if !l.allow(now, node, target(i)) {
			t.Fatalf("distinct target %d refused", i)
		}
	}
	if l.allow(now, node, target(256)) {
		t.Fatal("257th distinct target within a minute allowed")
	}
	// A repeated target is not distinct and stays allowed once the connection rate refills.
	now = now.Add(time.Second)
	if !l.allow(now, node, target(0)) {
		t.Fatal("repeated target refused")
	}
	// Once the window passes, new targets are allowed again.
	now = now.Add(time.Minute)
	if !l.allow(now, node, target(256)) {
		t.Fatal("target refused after the window")
	}
}

func TestPacketLimiter(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	l := newPacketLimiter(2000)
	node := [32]byte{1}
	for i := 0; i < 2000; i++ {
		if !l.allow(now, node) {
			t.Fatalf("packet %d refused", i+1)
		}
	}
	if l.allow(now, node) {
		t.Fatal("2001st packet in one second allowed")
	}
	if !l.allow(now, [32]byte{2}) {
		t.Fatal("second identity refused")
	}
	now = now.Add(time.Second)
	if !l.allow(now, node) {
		t.Fatal("packet after refill refused")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
