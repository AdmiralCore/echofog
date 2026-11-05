package exit

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

func TestPaddingPayloadExactFit(t *testing.T) {
	t.Parallel()

	for n := 3; n <= 200; n++ {
		p, ok := paddingPayload(n)
		if !ok {
			continue // a width jump leaves a one-byte gap (67 is one)
		}
		if got := frame.FrameLen(frame.TypePadding, p); got != n {
			t.Fatalf("paddingPayload(%d) = %d encoded %d", n, p, got)
		}
	}
	if _, ok := paddingPayload(67); ok {
		t.Fatal("67 has no exact PADDING frame; the test's width-jump gap changed")
	}
	for _, n := range []int{0, 1, 2} {
		if _, ok := paddingPayload(n); ok {
			t.Fatalf("paddingPayload(%d) succeeded", n)
		}
	}
}

func TestPadderRoundsFirstRecordsToBuckets(t *testing.T) {
	t.Parallel()

	prof := &shape.Profile{Buckets: []uint32{1024}}
	var slept []time.Duration
	p := newPadder(prof, func(d time.Duration) { slept = append(slept, d) }, rand.New(rand.NewPCG(1, 2)))

	for i := 0; i < padRecords; i++ {
		pad, _ := p.plan(100)
		if pad == 0 {
			t.Fatalf("record %d was not padded", i)
		}
		total := frame.FrameLen(TypeData, 100) + frame.FrameLen(frame.TypePadding, pad)
		if total != 1024 {
			t.Fatalf("record %d total = %d, want 1024", i, total)
		}
	}
	// Only the first 8 records are padded (H1).
	if pad, _ := p.plan(100); pad != 0 {
		t.Fatalf("ninth record padded by %d", pad)
	}
	if len(slept) != padRecords {
		t.Fatalf("slept %d times, want %d", len(slept), padRecords)
	}
	for _, d := range slept {
		if d < 0 || d > maxJitter {
			t.Fatalf("jitter %v outside [0, %v]", d, maxJitter)
		}
	}
}

func TestPadderStopsAfter16KiB(t *testing.T) {
	t.Parallel()

	prof := &shape.Profile{Buckets: []uint32{16384}}
	p := newPadder(prof, func(time.Duration) {}, rand.New(rand.NewPCG(3, 4)))
	// Two 9 KiB records already cross the 16 KiB window after padding to 16 KiB each.
	if pad, _ := p.plan(9000); pad == 0 {
		t.Fatal("first record not padded")
	}
	if pad, _ := p.plan(9000); pad != 0 {
		t.Fatalf("second record padded by %d after the window", pad)
	}
}

func TestPadderWithoutProfile(t *testing.T) {
	t.Parallel()

	p := newPadder(nil, func(time.Duration) {}, rand.New(rand.NewPCG(5, 6)))
	if pad, jitter := p.plan(100); pad != 0 || jitter != 0 {
		t.Fatalf("nil profile padded: %d %v", pad, jitter)
	}
}
