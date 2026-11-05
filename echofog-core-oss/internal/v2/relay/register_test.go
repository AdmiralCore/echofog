package relay

import (
	"errors"
	"testing"
	"time"
)

const testDay = 20_400 // an arbitrary day number

func dayStart(d uint32) time.Time { return time.Unix(int64(d)*86400+12*3600, 0) }

func rid(n byte) []byte {
	b := make([]byte, 32)
	b[0], b[31] = n, 0xAA
	return b
}

// register sends REGISTER on a fresh stream and returns REGISTERED.count, or the error that
// replaced it.
func (s *side) register(day uint32, ids ...[]byte) (uint32, error) {
	st, err := s.open(s.w.ctx)
	if err != nil {
		return 0, err
	}
	st.SetLimiter(limiter())
	if err := st.WriteFrame(TypeRegister, 0, Register{Req: 1, IDs: ids, Day: day}.Append(nil)); err != nil {
		return 0, err
	}
	f, err := readFrameTimeout(st, 3*time.Second)
	if err != nil {
		return 0, err
	}
	if f.Type != TypeRegistered {
		return 0, errors.New("not REGISTERED")
	}
	r, err := ParseRegistered(f.Payload)
	return r.Count, err
}

func (s *side) dialRID(id []byte) error {
	var t [32]byte
	copy(t[:], id)
	c, err := Dial(s.w.ctx, s.open, s.w.rNode, t, ConnectOptions{TargetKind: TargetRendezvous})
	if err == nil {
		_ = c.Close()
	}
	return err
}

// M3 (accept 1): REGISTER carries a day; only the current and the next day are taken.
func TestRegisterAcceptsOnlyTodayAndTomorrow(t *testing.T) {
	clk := newFakeClock(dayStart(testDay))
	w := newWorld(t, ServerConfig{Clock: clk})
	b := w.join(newTNode(t), "10.0.0.2", true)
	for _, day := range []uint32{testDay - 1, testDay + 2, 0} {
		if _, err := b.register(day, rid(1)); err == nil {
			t.Fatalf("day %d accepted on day %d", day, testDay)
		}
	}
	if n, err := b.register(testDay, rid(1)); err != nil || n != 1 {
		t.Fatalf("today: count %d, %v", n, err)
	}
	if n, err := b.register(testDay+1, rid(2)); err != nil || n != 2 {
		t.Fatalf("tomorrow: count %d, %v", n, err)
	}
}

// AD19: an expiry lives in one index. At the start of the next day the old IDs are gone, the
// count drops with them, and the node can register the new day's IDs even if it was full.
func TestRegistrationsExpireByDayAndFreeTheirSlots(t *testing.T) {
	clk := newFakeClock(dayStart(testDay))
	w := newWorld(t, ServerConfig{Clock: clk, Limits: Limits{MaxRendezvous: 4}})
	a := w.join(newTNode(t), "10.0.0.1", true)
	b := w.join(newTNode(t), "10.0.0.2", true)
	if n, err := b.register(testDay, rid(1), rid(2), rid(3), rid(4)); err != nil || n != 4 {
		t.Fatalf("fill: count %d, %v", n, err)
	}
	if err := a.dialRID(rid(1)); err != nil {
		t.Fatalf("rendezvous dial on the day of registration: %v", err)
	}
	// Full: one more is refused, and the node is told (no silent REGISTERED).
	if _, err := b.register(testDay+1, rid(5)); err == nil {
		t.Fatal("a full node got REGISTERED for a new ID")
	}
	// Next day.
	clk.Advance(13 * time.Hour)
	if n, err := b.register(testDay+1, rid(5), rid(6)); err != nil || n != 2 {
		t.Fatalf("after the day change: count %d, %v (the old IDs must be gone and uncounted)", n, err)
	}
	var re *RefusedError
	if err := a.dialRID(rid(1)); !errors.As(err, &re) || re.Reason != ReasonUnavailable {
		t.Fatalf("expired rendezvous ID still served: %v", err)
	}
	if err := a.dialRID(rid(5)); err != nil {
		t.Fatalf("new day's ID not served: %v", err)
	}
}

// A node can hold 256 IDs on one day and 256 on the next, replacing the old after the change.
func TestRegisterLimitIs256PerNode(t *testing.T) {
	clk := newFakeClock(dayStart(testDay))
	w := newWorld(t, ServerConfig{Clock: clk})
	b := w.join(newTNode(t), "10.0.0.2", true)
	ids := make([][]byte, 256)
	for i := range ids {
		ids[i] = append([]byte{byte(i), byte(i >> 8)}, make([]byte, 30)...)
	}
	// One REGISTER frame carries at most 244 IDs (8320 bytes): fill in two frames.
	if _, err := b.register(testDay, ids[:128]...); err != nil {
		t.Fatal(err)
	}
	if n, err := b.register(testDay, ids[128:]...); err != nil || n != 256 {
		t.Fatalf("256 IDs: count %d, %v", n, err)
	}
	if _, err := b.register(testDay+1, rid(9)); err == nil {
		t.Fatal("257th ID accepted")
	}
	clk.Advance(13 * time.Hour)
	if _, err := b.register(testDay+1, ids[:128]...); err != nil {
		t.Fatal(err)
	}
	if n, err := b.register(testDay+1, ids[128:]...); err != nil || n != 256 {
		t.Fatalf("256 IDs of the next day: count %d, %v", n, err)
	}
}
