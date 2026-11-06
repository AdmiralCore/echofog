package media

import (
	"crypto/ed25519"
	"errors"
	"io"
	"time"
)

// ErrNoEpoch is returned to a sender that has no current epoch secret: it MUST NOT send media.
var ErrNoEpoch = errors.New("media v3: no epoch secret")

// EpochGrace3 is how long the previous epoch secret stays valid after a switch.
const EpochGrace3 = 5 * time.Second

// EpochKeys holds the epoch secrets one participant knows.
type EpochKeys struct{}

// NewEpochKeys returns an empty set on the given clock.
func NewEpochKeys(now func() time.Time) *EpochKeys { return &EpochKeys{} }

// Install stores a secret that becomes the current one at activateAt (zero: at once).
func (e *EpochKeys) Install(epoch byte, secret []byte, activateAt time.Time) {}

// Current returns the epoch to send with.
func (e *EpochKeys) Current() (byte, []byte, bool) { return 0, nil, false }

// Secret returns the secret of an installed, not yet retired epoch.
func (e *EpochKeys) Secret(epoch byte) ([]byte, bool) { return nil, false }

// Rotation is what the coordinator sends after a join, leave or kick.
type Rotation struct {
	Epoch      byte
	Secret     [32]byte
	Delay      time.Duration
	Recipients [][32]byte
}

// EpochCoordinator draws epoch secrets.
type EpochCoordinator struct{}

// NewEpochCoordinator starts at epoch 0; rand supplies the secrets and the delay.
func NewEpochCoordinator(rand io.Reader) *EpochCoordinator { return &EpochCoordinator{} }

// Rotate draws the next epoch for exactly these members: a kicked member is not among them.
func (c *EpochCoordinator) Rotate(members [][32]byte) (Rotation, error) {
	return Rotation{}, ErrNotImplemented
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

// Sender3 seals, fragments and authenticates the media of one participant.
type Sender3 struct{}

// NewSender3 returns a sender.
func NewSender3(cfg Sender3Config) (*Sender3, error) { return nil, ErrNotImplemented }

// Send seals one unit.
func (s *Sender3) Send(kind, stream byte, ts uint32, plain []byte) (Sent, error) {
	return Sent{}, ErrNotImplemented
}

// Tick returns the CALL_AUTH payloads of batches that expired.
func (s *Sender3) Tick() [][]byte { return nil }
