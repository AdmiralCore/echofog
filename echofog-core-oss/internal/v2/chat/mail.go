package chat

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
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

// MemMailDirectory is a MailDirectory in memory.
type MemMailDirectory struct {
	mu sync.Mutex
	m  map[[32]byte]MailTarget
	// Max bounds the entries; zero means DefaultMaxMailTargets.
	Max int
}

// DefaultMaxMailTargets is the most queues a directory keeps.
const DefaultMaxMailTargets = 4096

// Len is how many targets are kept.
func (d *MemMailDirectory) Len() int { return 0 }

// Prune drops the targets that have ended.
func (d *MemMailDirectory) Prune(now time.Time) {}

// NewMemMailDirectory returns an empty directory.
func NewMemMailDirectory() *MemMailDirectory {
	return &MemMailDirectory{m: map[[32]byte]MailTarget{}}
}

func (d *MemMailDirectory) Put(node [32]byte, t MailTarget) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t.RelayCard = append([]byte(nil), t.RelayCard...)
	d.m[node] = t
	return nil
}

func (d *MemMailDirectory) Get(node [32]byte) (MailTarget, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.m[node]
	return t, ok
}

func (d *MemMailDirectory) Delete(node [32]byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.m, node)
}

// KVMailDirectory keeps the queues in the store, under blinded keys (security.md 4.8).
type KVMailDirectory struct {
	KV  store.KV
	IDs *store.IDs
	// Max bounds the entries; zero means DefaultMaxMailTargets.
	Max int
}

// Len is how many targets are kept.
func (d *KVMailDirectory) Len() int { return 0 }

// Prune drops the targets that have ended.
func (d *KVMailDirectory) Prune(now time.Time) {}

type kvTarget struct {
	Relay     [32]byte `json:"relay"`
	Queue     [16]byte `json:"queue"`
	NotAfter  int64    `json:"not_after"`
	RelayCard []byte   `json:"card,omitempty"`
}

func (d *KVMailDirectory) key(node [32]byte) ([]byte, error) {
	id, err := d.IDs.Blind(store.DomainPeer, node[:])
	if err != nil {
		return nil, err
	}
	return []byte("mailtarget:" + hex.EncodeToString(id[:])), nil
}

func (d *KVMailDirectory) Put(node [32]byte, t MailTarget) error {
	k, err := d.key(node)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(kvTarget{t.Relay, t.Queue, t.NotAfter.Unix(), t.RelayCard})
	if err != nil {
		return err
	}
	return d.KV.Put(k, raw)
}

func (d *KVMailDirectory) Get(node [32]byte) (MailTarget, bool) {
	k, err := d.key(node)
	if err != nil {
		return MailTarget{}, false
	}
	raw, err := d.KV.Get(k)
	if err != nil {
		return MailTarget{}, false
	}
	var kt kvTarget
	if json.Unmarshal(raw, &kt) != nil {
		return MailTarget{}, false
	}
	return MailTarget{kt.Relay, kt.Queue, time.Unix(kt.NotAfter, 0), kt.RelayCard}, true
}

func (d *KVMailDirectory) Delete(node [32]byte) {
	if k, err := d.key(node); err == nil {
		_ = d.KV.Delete(k)
	}
}

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

// Deposit hands the envelope for node to its mailbox queue. A queue that ended or that M does not
// know any more is forgotten (ErrNoMailbox: the message waits for a new announcement); a full or
// refusing queue is an error and stays.
func (m *MailboxMail) Deposit(ctx context.Context, node [32]byte, envelope []byte) error {
	t, ok := m.Dir.Get(node)
	if !ok {
		return ErrNoMailbox
	}
	if !m.Now().Before(t.NotAfter) {
		m.Dir.Delete(node)
		return ErrNoMailbox
	}
	if len(envelope) > chatpb.MaxEnvelope {
		return mailbox.ErrTooLarge
	}
	hs, err := m.HS(t.Relay, t.RelayCard)
	if err != nil {
		return fmt.Errorf("chat: key of the mailbox relay: %w", err)
	}
	last := errors.New("chat: no forwarding relay")
	for _, via := range m.Via() {
		st, err := m.Sender.Deposit(ctx, via, mailbox.Target{Relay: t.Relay, HS: hs, Queue: t.Queue[:], RelayCard: t.RelayCard}, envelope)
		if err != nil {
			last = err
			continue
		}
		switch st {
		case mailbox.StatusStored:
			return nil
		case mailbox.StatusUnknownQueue:
			m.Dir.Delete(node)
			return ErrNoMailbox
		case mailbox.StatusRelayUnreachable:
			last = fmt.Errorf("chat: relay unreachable through %x", via[:4])
		default:
			return fmt.Errorf("chat: the mailbox answered %d", st)
		}
	}
	return last
}
