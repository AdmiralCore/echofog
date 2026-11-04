package mux

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Sealing (wire.md §8.4):
//
//	sealed = n(u64) ‖ ChaCha20-Poly1305(k_dir, nonce = 0^4 ‖ u64le(n), aad, plaintext)
//	aad    = sid(16) ‖ u64(n) ‖ u8(class) ‖ varint(path_id) ‖ varint(pipe)
//
// One counter per direction per session, shared by every path, pipe, record and datagram.
// Ordered records (class 1) are protected against replay by a strictly growing n per pipe;
// SEALED datagrams (class 2, pipe 0) by a window of 4096 over the datagram counters.

// AAD classes (wire.md §8.4).
const (
	ClassRecord   byte = 1
	ClassDatagram byte = 2
)

// MaxSealCounter: a sender stops at 2^60; the session must then be re-established.
const MaxSealCounter = 1 << 60

// ReplayWindow is the datagram replay window: values behind the highest datagram counter.
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
	win replayWindow // datagrams only
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

// AAD returns sid ‖ u64(n) ‖ u8(class) ‖ varint(path_id) ‖ varint(pipe).
func (s *Sealer) AAD(n uint64, class byte, pathID, pipe uint64) []byte {
	b := make([]byte, 0, 16+8+1+16)
	b = append(b, s.sid[:]...)
	b = binary.BigEndian.AppendUint64(b, n)
	b = append(b, class)
	b = frame.AppendVarint(b, pathID)
	return frame.AppendVarint(b, pipe)
}

func nonceOf(n uint64) (nonce [12]byte) {
	binary.LittleEndian.PutUint64(nonce[4:], n)
	return
}

// Seal appends n ‖ ciphertext to dst, drawing n from the session's send counter.
func (s *Sealer) Seal(dst, plaintext []byte, class byte, pathID, pipe uint64) ([]byte, error) {
	n := s.next.Add(1) - 1
	if n >= MaxSealCounter {
		s.next.Store(MaxSealCounter)
		return nil, ErrCounterExhausted
	}
	nonce := nonceOf(n)
	dst = binary.BigEndian.AppendUint64(dst, n)
	return s.send.Seal(dst, nonce[:], plaintext, s.AAD(n, class, pathID, pipe)), nil
}

func (s *Sealer) open(dst, sealed []byte, class byte, pathID, pipe uint64) (uint64, []byte, error) {
	if len(sealed) < sealOverhead {
		return 0, nil, ErrSealedShort
	}
	n := binary.BigEndian.Uint64(sealed[:8])
	if n >= MaxSealCounter {
		return 0, nil, ErrOpen
	}
	nonce := nonceOf(n)
	pt, err := s.recv.Open(dst, nonce[:], sealed[8:], s.AAD(n, class, pathID, pipe))
	if err != nil {
		return 0, nil, ErrOpen
	}
	return n, pt, nil
}

// OpenDatagram opens a SEALED datagram payload received on path pathID, checking the 4096
// datagram replay window.
func (s *Sealer) OpenDatagram(dst, sealed []byte, pathID uint64) ([]byte, error) {
	if len(sealed) >= 8 {
		s.mu.Lock()
		ok := s.win.check(binary.BigEndian.Uint64(sealed[:8]))
		s.mu.Unlock()
		if !ok {
			return nil, ErrReplay
		}
	}
	n, pt, err := s.open(dst, sealed, ClassDatagram, pathID, 0)
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

// PipeState is the receive state of one ordered pipe: the last accepted counter.
type PipeState struct {
	last    uint64
	hasLast bool
}

// OpenRecord opens an ordered record of pipe (pathID, pipe). Its counter must be larger than
// that of the pipe's previous record; there is no window (wire.md §8.4).
func (s *Sealer) OpenRecord(dst, sealed []byte, pathID, pipe uint64, st *PipeState) ([]byte, error) {
	n, pt, err := s.open(dst, sealed, ClassRecord, pathID, pipe)
	if err != nil {
		return nil, err
	}
	if st.hasLast && n <= st.last {
		return nil, ErrReplay
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
