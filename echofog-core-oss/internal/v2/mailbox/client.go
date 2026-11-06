package mailbox

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// Conn is one mailbox/2 session as a client uses it: one request at a time per call, errors for
// ERROR frames and closed streams. StreamConn is the implementation over a stream.
type Conn interface {
	// SID is the session id FETCH and ACK auth is bound to (services.md 5.2).
	SID() [16]byte
	CreateQueue(ctx context.Context, m QueueCreate) (expires uint64, err error)
	Deposit(ctx context.Context, m Deposit) (Deposited, error)
	Forward(ctx context.Context, m Forward) (Deposited, error)
	Fetch(ctx context.Context, m Fetch) (Items, error)
	Ack(ctx context.Context, m Ack) error
	Close() error
}

// Dialer opens a mailbox/2 session with a device (a mailbox relay M or a forwarding relay F).
type Dialer interface {
	Dial(ctx context.Context, device [32]byte) (Conn, error)
}

var errNotImplemented = errors.New("mailbox: not implemented")

// Target is where a sender puts a message: a queue on a mailbox relay M.
type Target struct {
	// Relay is M's device ID; HS is M's handshake static key from its card (the deposit is sealed to it).
	Relay [32]byte
	HS    []byte
	Queue []byte
	// Public queues need proof of work of PowBits (services.md 5.2).
	Public  bool
	PowBits uint32
	// RelayCard is M's card when F cannot know it (an unlisted relay); optional.
	RelayCard []byte
}

// ErrTooLarge is a blob above 64 KiB.
var ErrTooLarge = errors.New("mailbox: blob above 64 KiB")

// Sender deposits blobs through a forwarding relay F (services.md 5.3).
type Sender struct {
	Dial Dialer
	Rand io.Reader
}

// Deposit seals a DEPOSIT for M, sends FORWARD to F and returns M's DEPOSITED status. For a public
// queue it first finds the proof of work; ctx cancels the search.
func (s *Sender) Deposit(ctx context.Context, via [32]byte, t Target, blob []byte) (uint32, error) {
	return 0, errNotImplemented
}

// QueueRef names a queue of the recipient on a mailbox relay.
type QueueRef struct {
	Relay   [32]byte
	Queue   [QueueSize]byte
	Expires time.Time
}

// ReceiverConfig configures a Receiver.
type ReceiverConfig struct {
	Store store.KV
	Dial  Dialer
	// Relays are the mailbox relays the recipient may use, in order of preference.
	Relays func() [][32]byte
	Now    func() time.Time
	Rand   io.Reader
	// Handle processes one blob and says whether it may be acknowledged: true once it is handled (or
	// can never be), false to see it again at the next poll.
	Handle func(ctx context.Context, blob []byte) bool
	// Announce is called with each new current queue, so that contacts can be told (services.md 5.2).
	Announce func(QueueRef)
	// Limited says the node is in background=limited mode: no long-poll, no polling on its own.
	Limited func() bool
	// Wait is the long-poll time of a FETCH; zero means 30 s.
	Wait time.Duration
}

// Receiver owns the recipient's queues: it creates them, rotates them, fetches and acknowledges.
type Receiver struct{}

// NewReceiver builds a Receiver over its stored queues.
func NewReceiver(cfg ReceiverConfig) (*Receiver, error) { return nil, errNotImplemented }

// Tick creates the first queue or the next one when it is due, then fetches every queue once.
func (r *Receiver) Tick(ctx context.Context) error { return errNotImplemented }

// Queues returns the current queue of each relay in use.
func (r *Receiver) Queues() []QueueRef { return nil }
