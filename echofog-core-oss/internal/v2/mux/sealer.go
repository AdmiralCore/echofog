package mux

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
)

// Sealing (wire.md §8.4):
//
//	sealed = n(u64) ‖ ChaCha20-Poly1305(k_dir, nonce = 0^4 ‖ u64le(n), aad = sid(16) ‖ n(8), plaintext)
//
// One counter per direction per session, shared by every path, record and datagram.

// MaxSealCounter: a sender stops at 2^60; the session must then be re-established.
const MaxSealCounter = 1 << 60

// ReplayWindow is the number of counter values behind the highest one that a receiver tracks.
const ReplayWindow = 4096

const sealOverhead = 8 + chacha20poly1305.Overhead

var (
	ErrCounterExhausted = errors.New("mux: sealing counter exhausted (2^60): re-establish the session")
	ErrReplay           = errors.New("mux: replayed or too old sealed counter")
	ErrSealedShort      = errors.New("mux: sealed packet too short")
	ErrOpen             = errors.New("mux: sealed packet does not authenticate")
)

// Sealer seals and opens with a session's transport keys. It is shared by all paths of a
// session. Safe for concurrent use.
type Sealer struct {
	sid  [16]byte
	send cipher.AEAD
	recv cipher.AEAD
	next atomic.Uint64

	mu  sync.Mutex
	win replayWindow
}

// NewSealer builds the sealer of this side of a session: it seals with k_i2r if this side is the
// initiator and opens with the other key.
func NewSealer(k Keys) (*Sealer, error) {
	i2r, r2i := k.TransportKeys()
	sk, rk := i2r, r2i
	if !k.Initiator() {
		sk, rk = r2i, i2r
	}
	send, err := chacha20poly1305.New(sk)
	if err != nil {
		return nil, err
	}
	recv, err := chacha20poly1305.New(rk)
	if err != nil {
		return nil, err
	}
	return &Sealer{sid: k.SID(), send: send, recv: recv}, nil
}

func (s *Sealer) nonceAAD(n uint64) (nonce [12]byte, aad [24]byte) {
	binary.LittleEndian.PutUint64(nonce[4:], n)
	copy(aad[:16], s.sid[:])
	binary.BigEndian.PutUint64(aad[16:], n)
	return
}

// Seal appends n ‖ ciphertext to dst, drawing n from the session's send counter.
func (s *Sealer) Seal(dst, plaintext []byte) ([]byte, error) {
	n := s.next.Add(1) - 1
	if n >= MaxSealCounter {
		s.next.Store(MaxSealCounter)
		return nil, ErrCounterExhausted
	}
	nonce, aad := s.nonceAAD(n)
	dst = binary.BigEndian.AppendUint64(dst, n)
	return s.send.Seal(dst, nonce[:], plaintext, aad[:]), nil
}

func (s *Sealer) open(dst, sealed []byte) (uint64, []byte, error) {
	if len(sealed) < sealOverhead {
		return 0, nil, ErrSealedShort
	}
	n := binary.BigEndian.Uint64(sealed[:8])
	if n >= MaxSealCounter {
		return 0, nil, ErrOpen
	}
	nonce, aad := s.nonceAAD(n)
	pt, err := s.recv.Open(dst, nonce[:], sealed[8:], aad[:])
	if err != nil {
		return 0, nil, ErrOpen
	}
	return n, pt, nil
}

// Open authenticates a sealed packet and checks the session's replay window (4096 values per
// direction). It is used for datagrams and for relay circuit records.
func (s *Sealer) Open(dst, sealed []byte) ([]byte, error) {
	if len(sealed) >= 8 {
		n := binary.BigEndian.Uint64(sealed[:8])
		s.mu.Lock()
		ok := s.win.check(n)
		s.mu.Unlock()
		if !ok {
			return nil, ErrReplay
		}
	}
	n, pt, err := s.open(dst, sealed)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.win.check(n) {
		return nil, ErrReplay
	}
	s.win.mark(n)
	return pt, nil
}

// orderedState is the per-stream state of openOrdered.
type orderedState struct {
	last    uint64
	hasLast bool
}

// openOrdered opens a record of an ordered record stream (a stream of a direct path in sealed
// mode). The counter must grow along the stream; inside the session window the shared replay
// window applies too. A record whose counter fell below the window while it waited unread in a
// stream buffer is still accepted if it is above the stream's previous record (spec issue S7:
// the session-wide window would otherwise reject records of a slow reader).
func (s *Sealer) openOrdered(dst, sealed []byte, st *orderedState) ([]byte, error) {
	n, pt, err := s.open(dst, sealed)
	if err != nil {
		return nil, err
	}
	if st.hasLast && n <= st.last {
		return nil, ErrReplay
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.win.inWindow(n) {
		if !s.win.check(n) {
			return nil, ErrReplay
		}
		s.win.mark(n)
	}
	st.last, st.hasLast = n, true
	return pt, nil
}

// replayWindow tracks the highest counter seen and a bitmap of the ReplayWindow values below it.
type replayWindow struct {
	any     bool
	highest uint64
	bits    [ReplayWindow / 64]uint64
}

// inWindow reports whether n is above the highest value or within the window below it.
func (w *replayWindow) inWindow(n uint64) bool {
	return !w.any || n > w.highest || w.highest-n < ReplayWindow
}

func (w *replayWindow) check(n uint64) bool {
	if !w.any || n > w.highest {
		return true
	}
	d := w.highest - n
	if d >= ReplayWindow {
		return false
	}
	return w.bits[(n%ReplayWindow)/64]&(1<<(n%64)) == 0
}

func (w *replayWindow) mark(n uint64) {
	if !w.any || n > w.highest {
		var shift uint64 = ReplayWindow
		if w.any {
			shift = n - w.highest
		}
		if shift >= ReplayWindow {
			w.bits = [ReplayWindow / 64]uint64{}
		} else {
			for i := w.highest + 1; i <= n; i++ {
				w.bits[(i%ReplayWindow)/64] &^= 1 << (i % 64)
			}
		}
		w.highest = n
		w.any = true
	}
	w.bits[(n%ReplayWindow)/64] |= 1 << (n % 64)
}
