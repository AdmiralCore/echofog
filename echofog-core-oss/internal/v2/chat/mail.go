package chat

import (
	"context"
	"encoding/binary"
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

// maxQueueLife is the longest an announced queue may last (services.md 5.2: ttl at most 30 days).
const maxQueueLife = 30 * 24 * time.Hour

// Len is how many targets are kept.
func (d *MemMailDirectory) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.m)
}

// Prune drops the targets that have ended.
func (d *MemMailDirectory) Prune(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.m {
		if !now.Before(t.NotAfter) {
			delete(d.m, k)
		}
	}
}

func (d *MemMailDirectory) max() int {
	if d.Max > 0 {
		return d.Max
	}
	return DefaultMaxMailTargets
}

// NewMemMailDirectory returns an empty directory.
func NewMemMailDirectory() *MemMailDirectory {
	return &MemMailDirectory{m: map[[32]byte]MailTarget{}}
}

func (d *MemMailDirectory) Put(node [32]byte, t MailTarget) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t.RelayCard = append([]byte(nil), t.RelayCard...)
	if _, ok := d.m[node]; !ok {
		for len(d.m) >= d.max() { // the target that ends first goes
			var oldest [32]byte
			var at time.Time
			first := true
			for k, v := range d.m {
				if first || v.NotAfter.Before(at) {
					oldest, at, first = k, v.NotAfter, false
				}
			}
			delete(d.m, oldest)
		}
	}
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

// KVMailDirectory keeps the queues in the store, under blinded keys (security.md 4.8). Each target has
// an entry in a logical index of end times (AD19): it is dropped when it ends, and the oldest goes when
// the directory is full. The counter changes only where an entry that was read and found is deleted.
type KVMailDirectory struct {
	KV  store.KV
	IDs *store.IDs
	// Max bounds the entries; zero means DefaultMaxMailTargets.
	Max int

	mu     sync.Mutex
	loaded bool
	n      int
}

type kvTarget struct {
	Relay     [32]byte `json:"relay"`
	Queue     [16]byte `json:"queue"`
	NotAfter  int64    `json:"not_after"`
	RelayCard []byte   `json:"card,omitempty"`
}

const (
	targetPrefix = "mailtarget:"
	targetIndex  = "mailx:" // mailx:<not_after 8><blinded id hex>
)

func (d *KVMailDirectory) max() int {
	if d.Max > 0 {
		return d.Max
	}
	return DefaultMaxMailTargets
}

func (d *KVMailDirectory) blind(node [32]byte) (string, error) {
	id, err := d.IDs.Blind(store.DomainPeer, node[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func indexKey(notAfter int64, id string) []byte {
	return append(binary.BigEndian.AppendUint64([]byte(targetIndex), uint64(notAfter)), id...)
}

// load counts the stored targets once. d.mu is held.
func (d *KVMailDirectory) load() {
	if d.loaded {
		return
	}
	d.loaded = true
	d.n = 0
	var after []byte
	for {
		var (
			page []store.Entry
			err  error
		)
		if after == nil {
			page, err = d.KV.Scan([]byte(targetIndex), store.MaxScanLimit)
		} else {
			page, err = d.KV.ScanFrom([]byte(targetIndex), after, store.MaxScanLimit)
		}
		if err != nil {
			return
		}
		d.n += len(page)
		if len(page) < store.MaxScanLimit {
			return
		}
		after = page[len(page)-1].Key
	}
}

// Len is how many targets are kept.
func (d *KVMailDirectory) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.load()
	return d.n
}

// removeLocked deletes the target with this blinded id if it is found, with its index entry.
func (d *KVMailDirectory) removeLocked(id string) {
	raw, err := d.KV.Get([]byte(targetPrefix + id))
	if err != nil {
		return
	}
	var kt kvTarget
	if json.Unmarshal(raw, &kt) == nil {
		_ = d.KV.Delete(indexKey(kt.NotAfter, id))
	}
	_ = d.KV.Delete([]byte(targetPrefix + id))
	if d.n > 0 {
		d.n--
	}
}

func (d *KVMailDirectory) Put(node [32]byte, t MailTarget) error {
	id, err := d.blind(node)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(kvTarget{t.Relay, t.Queue, t.NotAfter.Unix(), t.RelayCard})
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.load()
	if ok, _ := d.KV.Has([]byte(targetPrefix + id)); ok {
		d.removeLocked(id) // replaced: the old entry and its index go first
	}
	for d.n >= d.max() { // the target that ends first goes
		page, err := d.KV.Scan([]byte(targetIndex), 1)
		if err != nil || len(page) == 0 {
			break
		}
		k := page[0].Key
		before := d.n
		d.removeLocked(string(k[len(targetIndex)+8:]))
		_ = d.KV.Delete(k)
		if d.n == before {
			break // an index entry without a target: gone now
		}
	}
	if err := d.KV.Put([]byte(targetPrefix+id), raw); err != nil {
		return err
	}
	if err := d.KV.Put(indexKey(t.NotAfter.Unix(), id), nil); err != nil {
		return err
	}
	d.n++
	return nil
}

func (d *KVMailDirectory) Get(node [32]byte) (MailTarget, bool) {
	id, err := d.blind(node)
	if err != nil {
		return MailTarget{}, false
	}
	raw, err := d.KV.Get([]byte(targetPrefix + id))
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
	id, err := d.blind(node)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.load()
	d.removeLocked(id)
}

// Prune drops the targets that have ended.
func (d *KVMailDirectory) Prune(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.load()
	for {
		page, err := d.KV.Scan([]byte(targetIndex), 64)
		if err != nil || len(page) == 0 {
			return
		}
		for _, e := range page {
			k := e.Key
			if len(k) <= len(targetIndex)+8 {
				_ = d.KV.Delete(k)
				continue
			}
			if int64(binary.BigEndian.Uint64(k[len(targetIndex):len(targetIndex)+8])) > now.Unix() {
				return
			}
			d.removeLocked(string(k[len(targetIndex)+8:]))
			_ = d.KV.Delete(k)
		}
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
