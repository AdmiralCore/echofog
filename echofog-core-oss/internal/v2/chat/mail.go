package chat

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// MailTarget is where a contact receives mail while away: the newest queue it announced.
type MailTarget struct {
	Relay     [32]byte // device ID of the mailbox relay M
	Queue     [16]byte
	NotAfter  time.Time
	RelayCard []byte
}

// MailDirectory remembers the queue each contact announced.
type MailDirectory interface {
	Put(node [32]byte, t MailTarget) error
	Get(node [32]byte) (MailTarget, bool)
	Delete(node [32]byte)
}

var errNotImplemented = errors.New("chat: not implemented")

// MemMailDirectory is a MailDirectory in memory.
type MemMailDirectory struct{}

func NewMemMailDirectory() *MemMailDirectory                      { return &MemMailDirectory{} }
func (d *MemMailDirectory) Put(node [32]byte, t MailTarget) error { return errNotImplemented }
func (d *MemMailDirectory) Get(node [32]byte) (MailTarget, bool)  { return MailTarget{}, false }
func (d *MemMailDirectory) Delete(node [32]byte)                  {}

// KVMailDirectory keeps the queues in the store, under blinded keys (security.md 4.8).
type KVMailDirectory struct {
	KV  store.KV
	IDs *store.IDs
}

func (d *KVMailDirectory) Put(node [32]byte, t MailTarget) error { return errNotImplemented }
func (d *KVMailDirectory) Get(node [32]byte) (MailTarget, bool)  { return MailTarget{}, false }
func (d *KVMailDirectory) Delete(node [32]byte)                  {}

// ErrNoMailbox is a contact whose queue is not known (or ended): the message waits.
var ErrNoMailbox = errors.New("chat: no mailbox queue of the contact")

// Depositor puts a blob into a queue through a forwarding relay; *mailbox.Sender satisfies it.
type Depositor interface {
	Deposit(ctx context.Context, via [32]byte, t mailbox.Target, blob []byte) (uint32, error)
}

// MailboxMail is the chat Mail over mailbox/2 (services.md 5.3): the envelope goes to the queue the
// contact announced, through the first forwarding relay that takes it.
type MailboxMail struct {
	Sender Depositor
	Dir    MailDirectory
	// Via lists the forwarding relays to try, in order.
	Via func() [][32]byte
	// HS returns the handshake key of the mailbox relay, from its card or the relay card given.
	HS  func(relay [32]byte, card []byte) ([]byte, error)
	Now func() time.Time
}

// Deposit hands the envelope for node to its mailbox queue.
func (m *MailboxMail) Deposit(ctx context.Context, node [32]byte, envelope []byte) error {
	return errNotImplemented
}
