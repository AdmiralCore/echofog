package servernode

import (
	"bytes"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

type fakeStream struct {
	r bytes.Buffer
	w bytes.Buffer
}

func (f *fakeStream) Read(p []byte) (int, error)       { return f.r.Read(p) }
func (f *fakeStream) Write(p []byte) (int, error)      { return f.w.Write(p) }
func (f *fakeStream) ID() uint64                       { return 1 }
func (f *fakeStream) CloseWrite() error                { return nil }
func (f *fakeStream) Close() error                     { return nil }
func (f *fakeStream) Reset(uint64)                     {}
func (f *fakeStream) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeStream) SetWriteDeadline(time.Time) error { return nil }

func TestTrafficMeterCountsAndRollsTheDay(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	m := newTrafficMeter()
	m.now = func() time.Time { return now }
	m.add(100, 200)
	if got := m.snapshot(); got.In != 100 || got.Out != 200 || got.DayIn != 100 || got.DayOut != 200 {
		t.Fatalf("after the first bytes: %+v", got)
	}
	now = now.Add(24 * time.Hour)
	m.add(1, 2)
	got := m.snapshot()
	if got.In != 101 || got.Out != 202 {
		t.Errorf("the total must not reset: %+v", got)
	}
	if got.DayIn != 1 || got.DayOut != 2 {
		t.Errorf("the day counter did not roll: %+v", got)
	}
}

func TestLimitsGateSessions(t *testing.T) {
	g := newGate(&LimitsSpec{Sessions: 1}, newTrafficMeter())
	if !g.acquire() {
		t.Fatal("the first session was refused")
	}
	if g.acquire() {
		t.Fatal("the second session was allowed over the limit")
	}
	g.release()
	if !g.acquire() {
		t.Fatal("the session slot was not released")
	}
	// No limit at all: everything passes and nothing waits.
	open := newGate(nil, newTrafficMeter())
	for i := 0; i < 100; i++ {
		if !open.acquire() {
			t.Fatal("a gate without a limit refused a session")
		}
	}
	open.wait(1 << 20)
}

func TestCountingStreamCountsBothWays(t *testing.T) {
	m := newTrafficMeter()
	fs := &fakeStream{}
	if _, err := fs.r.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	s := &countingStream{RawStream: fs, gate: newGate(nil, m)}
	if _, err := s.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := s.Read(buf); err != nil {
		t.Fatal(err)
	}
	got := m.snapshot()
	if got.In != 10 || got.Out != 10 {
		t.Fatalf("traffic %+v, want 10 in and 10 out", got)
	}
}

// The counting carrier is a mux.Carrier: the compile-time check catches a forgotten method.
var _ mux.Carrier = (*countingCarrier)(nil)
