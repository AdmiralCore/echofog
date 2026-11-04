package mux

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// bindNewPath dials a second carrier from A to B and binds it; it returns the path ID.
func bindNewPath(t testing.TB, pr *pair, sealed bool) (uint64, *carrierEnds) {
	t.Helper()
	e := newCarrier(t, sealed)
	ctx := ctxT(t)
	type res struct {
		id  uint64
		err error
	}
	ch := make(chan res, 1)
	go func() {
		id, err := pr.a.BindPath(ctx, e.ga, PathDirect)
		ch <- res{id, err}
	}()
	s, pend, err := pr.binder.Accept(ctx, e.gb, PathDirect)
	if err != nil || pend != nil || s != pr.b {
		t.Fatalf("bind refused: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.id, e
}

func TestPathSwitchWithoutNewSession(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		pr := sessionPair(t, sealed, func(a, b *Config) { a.Timers = fastTimers(); b.Timers = fastTimers() })
		ctx := ctxT(t)
		sid := pr.a.SID()
		// A stream on path 0.
		st, _ := pr.a.OpenStream(ctx, 3, 2)
		st.WriteFrame(0x40, 0, deliver(1, "on path 0"))
		rs, _ := pr.b.AcceptStream(ctx)
		rs.ReadFrame()

		id, e := bindNewPath(t, pr, sealed)
		if id != 2 {
			t.Fatalf("first path created by the initiator has ID %d, want 2", id)
		}
		// Path 0 dies (its carrier breaks). Its streams are reset with path-failed.
		pr.path0.ca.Close()
		if _, err := st.ReadFrame(); !errors.Is(err, ErrPathFailed) {
			t.Fatalf("stream on the dead path: %v, want ErrPathFailed", err)
		}
		waitFor(t, func() bool { _, act, ok := pr.a.Paths(); return ok && act == 2 })
		waitFor(t, func() bool { _, act, ok := pr.b.Paths(); return ok && act == 2 })
		// Same session, same keys: a new stream works on path 2, both directions, no handshake.
		if pr.a.SID() != sid || pr.b.SID() != sid {
			t.Fatal("session changed")
		}
		st2, err := pr.a.OpenStream(ctx, 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		if st2.PathID() != 2 {
			t.Fatalf("stream on path %d", st2.PathID())
		}
		st2.WriteFrame(0x40, 0, deliver(2, "on path 2"))
		rs2, _ := pr.b.AcceptStream(ctx)
		if f, err := rs2.ReadFrame(); err != nil || !bytes.Contains(f.Payload, []byte("on path 2")) {
			t.Fatalf("%v", err)
		}
		bs, err := pr.b.OpenStream(ctx, 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		bs.WriteFrame(0x40, 0, deliver(3, "from B"))
		as, _ := pr.a.AcceptStream(ctx)
		if f, err := as.ReadFrame(); err != nil || f.Type != 0x40 {
			t.Fatalf("%v", err)
		}
		if _, err := pr.a.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		// Datagrams follow the active path.
		pr.a.SendDatagram(frame.DgramCallAudio, []byte("dg"))
		if d, err := pr.b.ReceiveDatagram(ctx); err != nil || d.Path != 2 {
			t.Fatalf("%+v %v", d, err)
		}
		_ = e
		select {
		case <-pr.a.Done():
			t.Fatal("session ended with the first path")
		default:
		}
	}
}

func TestPathBindReplayedOnAnotherPathRefused(t *testing.T) {
	pr := sessionPair(t, false, nil)
	ctx := ctxT(t)
	// A real bind on carrier X; the PATH_BIND bytes are captured from X's wire.
	_, x := bindNewPath(t, pr, false)
	wire := x.ta.Bytes()
	i := bytes.Index(wire, []byte{byte(frame.TypePathBind), 0, 0x31})
	if i < 0 {
		t.Fatal("PATH_BIND not on the wire")
	}
	captured := append([]byte{}, wire[i:i+3+0x31]...)

	// The attacker replays it as stream 0 of a new carrier Y (another TLS session, other cb).
	y := newCarrier(t, false)
	raw, _ := y.ca.OpenStream(ctx)
	raw.Write(captured)
	if _, _, err := pr.binder.Accept(ctx, y.gb, PathDirect); !errors.Is(err, ErrBindRefused) {
		t.Fatalf("replayed PATH_BIND: %v", err)
	}
	// Refusal is silent: the carrier is closed without any answer.
	select {
	case <-y.cb.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("carrier not closed")
	}
	for _, b := range [][]byte{{byte(frame.TypePathBindOK)}, {byte(frame.TypeReject)}, {byte(frame.TypeClose)}} {
		if bytes.Contains(y.tb.Bytes(), b) && len(y.tb.Bytes()) > 0 {
			t.Fatalf("refusal sent bytes: %x", y.tb.Bytes())
		}
	}
	// Even replayed with Y's own binding, the same path ID is never accepted twice.
	z := newCarrier(t, false)
	ik, _ := vectorKeys(t)
	mac := PathMAC(PathBindKey(ik), z.binding, ik.sid, 2, 0)
	rawZ, _ := z.ca.OpenStream(ctx)
	rawZ.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypePathBind,
		Payload: frame.PathBind{SID: ik.sid, PathID: 2, MAC: mac}.Append(nil)}))
	if _, _, err := pr.binder.Accept(ctx, z.gb, PathDirect); !errors.Is(err, ErrBindRefused) {
		t.Fatalf("reused path ID: %v", err)
	}
	if pr.a.Err() != nil || pr.b.Err() != nil {
		t.Fatal("a refused PATH_BIND hurt the session")
	}
}

func TestPathBindChecks(t *testing.T) {
	pr := sessionPair(t, false, nil)
	ik, _ := vectorKeys(t)
	kp := PathBindKey(ik)
	try := func(name string, sid [16]byte, id uint64, role byte, cbOverride *[32]byte) error {
		e := newCarrier(t, false)
		cb := e.binding
		if cbOverride != nil {
			cb = *cbOverride
		}
		mac := PathMAC(kp, cb, ik.sid, id, role)
		raw, _ := e.ca.OpenStream(ctxT(t))
		raw.Write(frame.AppendFrame(nil, frame.Frame{Type: frame.TypePathBind,
			Payload: frame.PathBind{SID: sid, PathID: id, MAC: mac}.Append(nil)}))
		_, _, err := pr.binder.Accept(ctxT(t), e.gb, PathDirect)
		return err
	}
	var unknown [16]byte
	unknown[0] = 0xee
	if err := try("unknown sid", unknown, 4, 0, nil); !errors.Is(err, ErrBindRefused) {
		t.Fatal("unknown sid accepted")
	}
	if err := try("wrong parity", ik.sid, 3, 0, nil); !errors.Is(err, ErrBindRefused) {
		t.Fatal("odd path ID from the initiator accepted")
	}
	if err := try("path 0", ik.sid, 0, 0, nil); !errors.Is(err, ErrBindRefused) {
		t.Fatal("path 0 accepted")
	}
	if err := try("wrong role", ik.sid, 4, 1, nil); !errors.Is(err, ErrBindRefused) {
		t.Fatal("wrong role accepted")
	}
	var other [32]byte
	if err := try("other cb", ik.sid, 4, 0, &other); !errors.Is(err, ErrBindRefused) {
		t.Fatal("MAC over another channel binding accepted")
	}
	if err := try("valid", ik.sid, 4, 0, nil); err != nil {
		t.Fatalf("valid PATH_BIND refused: %v", err)
	}
	// Without the feature path-migration, PATH_BIND is never accepted.
	pr2 := sessionPair(t, false, func(a, b *Config) { a.Features = nil; b.Features = nil })
	e := newCarrier(t, false)
	if _, err := pr2.a.BindPath(ctxT(t), e.ga, PathDirect); !errors.Is(err, ErrFeatureNotSelected) {
		t.Fatalf("%v", err)
	}
}

func TestPathBindByResponder(t *testing.T) {
	// The session responder may create paths too (odd IDs), dialing the initiator.
	pr := sessionPair(t, false, nil)
	binderA := NewBinder()
	binderA.Add(pr.a)
	e := newCarrier(t, false) // B is the carrier client here
	ch := make(chan error, 1)
	var id uint64
	go func() {
		var err error
		id, err = pr.b.BindPath(ctxT(t), e.ga, PathDirect)
		ch <- err
	}()
	if s, _, err := binderA.Accept(ctxT(t), e.gb, PathDirect); err != nil || s != pr.a {
		t.Fatalf("%v", err)
	}
	if err := <-ch; err != nil || id != 1 {
		t.Fatalf("id %d err %v", id, err)
	}
}

func TestPathAbandon(t *testing.T) {
	pr := sessionPair(t, false, nil)
	id, e := bindNewPath(t, pr, false)
	if err := pr.a.AbandonPath(0, frame.CodePathFailed); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { ids, act, _ := pr.b.Paths(); return len(ids) == 1 && act == id })
	select {
	case <-pr.path0.cb.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned carrier still open")
	}
	if err := pr.a.AbandonPath(id, frame.CodePathFailed); err == nil {
		t.Fatal("abandoned the only path")
	}
	_ = e
}
