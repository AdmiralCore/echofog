package media

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrNoEpoch is returned to a sender that has no current epoch secret: it MUST NOT send media.
var ErrNoEpoch = errors.New("media v3: no epoch secret")

// ErrSeqExhausted is returned when the next seq would wrap: the nonce would repeat under one key.
var ErrSeqExhausted = errors.New("media v3: sequence numbers exhausted, rejoin")

// EpochGrace3 is how long the previous epoch secret stays valid after a switch.
const EpochGrace3 = 5 * time.Second

type epochEntry struct {
	secret   []byte
	activate time.Time
	retire   time.Time // zero: not retired
}

// EpochKeys holds the epoch secrets one participant knows. Safe for concurrent use.
type EpochKeys struct {
	now     func() time.Time
	mu      sync.Mutex
	entries map[byte]*epochEntry
	cur     byte
	haveCur bool
}

// NewEpochKeys returns an empty set on the given clock.
func NewEpochKeys(now func() time.Time) *EpochKeys {
	return &EpochKeys{now: now, entries: map[byte]*epochEntry{}}
}

// Install stores a secret that becomes the current one at activateAt (zero: at once). A secret
// installed ahead of its switch lets this participant open media of senders that switched first.
func (e *EpochKeys) Install(epoch byte, secret []byte, activateAt time.Time) {
	now := e.now()
	if activateAt.IsZero() {
		activateAt = now
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.entries[epoch] = &epochEntry{secret: append([]byte(nil), secret...), activate: activateAt}
	e.rollLocked(now)
}

// rollLocked makes the latest due entry current and retires the one it replaces.
func (e *EpochKeys) rollLocked(now time.Time) {
	best, found := byte(0), false
	var bestAt time.Time
	for ep, en := range e.entries {
		if en.activate.After(now) {
			continue
		}
		if !found || en.activate.After(bestAt) {
			best, bestAt, found = ep, en.activate, true
		}
	}
	if found && (!e.haveCur || best != e.cur) {
		if old, ok := e.entries[e.cur]; ok && e.haveCur && e.cur != best && old.retire.IsZero() {
			old.retire = now.Add(EpochGrace3)
		}
		e.cur, e.haveCur = best, true
	}
	for ep, en := range e.entries {
		if !en.retire.IsZero() && !now.Before(en.retire) {
			delete(e.entries, ep)
		}
	}
}

// Current returns the epoch to send with.
func (e *EpochKeys) Current() (byte, []byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollLocked(e.now())
	if !e.haveCur {
		return 0, nil, false
	}
	return e.cur, e.entries[e.cur].secret, true
}

// Secret returns the secret of an installed, not yet retired epoch.
func (e *EpochKeys) Secret(epoch byte) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollLocked(e.now())
	en, ok := e.entries[epoch]
	if !ok {
		return nil, false
	}
	return en.secret, true
}

// Rotation is what the coordinator sends after a join, leave or kick.
type Rotation struct {
	Epoch      byte
	Secret     [32]byte
	Delay      time.Duration
	Recipients [][32]byte
}

// EpochCoordinator draws epoch secrets. Safe for concurrent use.
type EpochCoordinator struct {
	mu    sync.Mutex
	rand  io.Reader
	epoch byte
}

// NewEpochCoordinator starts at epoch 0; rand supplies the secrets and the delay.
func NewEpochCoordinator(rand io.Reader) *EpochCoordinator {
	return &EpochCoordinator{rand: rand}
}

// Rotate draws the next epoch for exactly these members: a kicked member is not among them and
// never receives the new secret. The switch happens uniform(1.5, 2.5) s after the secrets are
// sent (services.md 9.3).
func (c *EpochCoordinator) Rotate(members [][32]byte) (Rotation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var r Rotation
	var buf [8]byte
	if _, err := io.ReadFull(c.rand, r.Secret[:]); err != nil {
		return Rotation{}, err
	}
	if _, err := io.ReadFull(c.rand, buf[:]); err != nil {
		return Rotation{}, err
	}
	frac := float64(binary.BigEndian.Uint64(buf[:])>>11) / (1 << 53) // [0, 1)
	r.Delay = 1500*time.Millisecond + time.Duration(frac*float64(time.Second))
	c.epoch++
	r.Epoch = c.epoch
	r.Recipients = append([][32]byte(nil), members...)
	return r, nil
}

// Sender3Config configures the media sender of one join.
type Sender3Config struct {
	Index     byte
	Device    [32]byte
	JoinSalt  []byte
	Gen       byte
	MediaPriv ed25519.PrivateKey
	Epochs    *EpochKeys
	Now       func() time.Time
}

// Sent is the datagram payloads one Send produced.
type Sent struct {
	Fragments [][]byte
	Auths     [][]byte
}

type streamKind struct{ stream, kind byte }

// Sender3 seals, fragments and authenticates the media of one participant (one join). It is not
// safe for concurrent use: media is produced by one goroutine per participant.
type Sender3 struct {
	cfg      Sender3Config
	keys     map[byte][]byte // epoch -> k_sender
	seq      map[streamKind]uint32
	batchers map[streamKind]*Batcher
}

// NewSender3 returns a sender. The join_salt is the one announced in the signed join.
func NewSender3(cfg Sender3Config) (*Sender3, error) {
	if len(cfg.JoinSalt) != JoinSalt3Len || len(cfg.MediaPriv) != ed25519.PrivateKeySize ||
		cfg.Epochs == nil || cfg.Now == nil {
		return nil, ErrBadKey3
	}
	return &Sender3{cfg: cfg, keys: map[byte][]byte{}, seq: map[streamKind]uint32{},
		batchers: map[streamKind]*Batcher{}}, nil
}

func (s *Sender3) key(epoch byte, secret []byte) ([]byte, error) {
	if k, ok := s.keys[epoch]; ok {
		return k, nil
	}
	k, err := SenderKey3(secret, s.cfg.Device, s.cfg.JoinSalt)
	if err != nil {
		return nil, err
	}
	if len(s.keys) > 8 {
		s.keys = map[byte][]byte{}
	}
	s.keys[epoch] = k
	return k, nil
}

// Send seals one unit. It returns ErrNoEpoch until the participant holds a current epoch secret:
// media MUST NOT be sent before (services.md 9.2).
func (s *Sender3) Send(kind, stream byte, ts uint32, plain []byte) (Sent, error) {
	epoch, secret, ok := s.cfg.Epochs.Current()
	if !ok {
		return Sent{}, ErrNoEpoch
	}
	k, err := s.key(epoch, secret)
	if err != nil {
		return Sent{}, err
	}
	sk := streamKind{stream, kind}
	h := Header3{Kind: kind, Epoch: epoch, Sender: s.cfg.Index, Stream: stream, Gen: s.cfg.Gen, Seq: s.seq[sk], TS: ts}
	ct, err := Seal3(h, k, plain)
	if err != nil {
		return Sent{}, err
	}
	frs, err := Split3(h, ct)
	if err != nil {
		return Sent{}, err
	}
	s.seq[sk]++ // only a unit that was sealed and fits uses up a number
	out := Sent{Fragments: make([][]byte, len(frs))}
	for i, f := range frs {
		out.Fragments[i] = f.Encode()
	}
	b := s.batchers[sk]
	if b == nil {
		b = NewBatcher(s.cfg.MediaPriv, s.cfg.Now)
		s.batchers[sk] = b
	}
	out.Auths = b.Add(h, ct)
	return out, nil
}

// Tick returns the CALL_AUTH payloads of batches that expired.
func (s *Sender3) Tick() [][]byte {
	var out [][]byte
	for _, b := range s.batchers {
		out = append(out, b.Tick()...)
	}
	return out
}
