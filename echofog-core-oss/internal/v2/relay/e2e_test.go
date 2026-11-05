package relay

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// tapLog records what R copied, per sender, and can tamper with it.
type tapLog struct {
	mu     sync.Mutex
	chunks [][]byte
	flip   func(sender [32]byte, b []byte)
	byKey  map[tapKey][]byte
}

type tapKey struct {
	cid    uint64
	sender [32]byte
}

func (l *tapLog) tap(cid uint64, sender [32]byte, b []byte) {
	l.mu.Lock()
	if l.byKey == nil {
		l.byKey = map[tapKey][]byte{}
	}
	l.byKey[tapKey{cid, sender}] = append(l.byKey[tapKey{cid, sender}], b...)
	l.chunks = append(l.chunks, append([]byte(nil), b...))
	f := l.flip
	l.mu.Unlock()
	if f != nil {
		f(sender, b)
	}
}

func (l *tapLog) all() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Join(l.chunks, nil)
}

func echoOnce(t *testing.T, from, to *mux.Session, msg []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := from.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if err := st.WriteFrame(0x40, 0, msg); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	rst, err := to.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	rst.SetLimiter(func(uint64) (int, bool) { return 61504, true })
	f, err := rst.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(f.Payload, msg) {
		t.Fatalf("payload changed in transit")
	}
}

// TestRelayedSessionIsOpaqueToR: A and B run an end-to-end session through R; R only ever
// handled records and never saw the plaintext.
func TestRelayedSessionIsOpaqueToR(t *testing.T) {
	log := &tapLog{}
	w := newWorld(t, ServerConfig{})
	w.srv.tap = log.tap
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)

	aEst, bEst, _ := w.relayedSession(a, b)
	if aEst.Kind != mux.PathRelay || bEst.Kind != mux.PathRelay {
		t.Fatalf("session kind %v / %v, want relay", aEst.Kind, bEst.Kind)
	}
	if aEst.Peer.NodeID != b.node || bEst.Peer.NodeID != a.node {
		t.Fatal("end-to-end handshake did not authenticate the other end")
	}
	secret := []byte("plaintext-that-r-must-never-see-0123456789")
	echoOnce(t, aEst.Session, bEst.Session, secret)
	echoOnce(t, bEst.Session, aEst.Session, secret)
	if seen := log.all(); len(seen) == 0 || bytes.Contains(seen, secret) {
		t.Fatalf("R saw %d bytes; plaintext leaked: %v", len(seen), bytes.Contains(seen, secret))
	}
}

// TestRelayTamperingKillsThePath: a relay that flips a bit in sealed records cannot change data;
// the receiver drops the path.
func TestRelayTamperingKillsThePath(t *testing.T) {
	log := &tapLog{}
	w := newWorld(t, ServerConfig{})
	w.srv.tap = log.tap
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	aEst, bEst, _ := w.relayedSession(a, b)
	echoOnce(t, aEst.Session, bEst.Session, []byte("before"))

	log.mu.Lock()
	log.flip = func(sender [32]byte, c []byte) {
		if sender == a.node && len(c) > 20 {
			c[len(c)-1] ^= 1
		}
	}
	log.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := aEst.Session.OpenStream(ctx, 3, 2)
	if err == nil {
		_ = st.WriteFrame(0x40, 0, []byte("tampered"))
	}
	// The session survives without a path (it waits for a PATH_BIND), but the tampered record
	// is never delivered and the relay path is gone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ids, _, _ := bEst.Session.Paths()
		if len(ids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("B kept the relay path although a record failed to authenticate")
		}
		time.Sleep(20 * time.Millisecond)
	}
	actx, acancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer acancel()
	if st, err := bEst.Session.AcceptStream(actx); err == nil {
		t.Fatalf("tampered stream was delivered: %v", st.Open())
	}
}
