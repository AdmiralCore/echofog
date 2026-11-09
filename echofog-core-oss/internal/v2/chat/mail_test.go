package chat

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ---- directories ------------------------------------------------------------------------------------

func target(b byte, notAfter time.Time) MailTarget {
	return MailTarget{Relay: [32]byte{b}, Queue: [16]byte{b, 1}, NotAfter: notAfter, RelayCard: []byte{b}}
}

func TestMemMailDirectory(t *testing.T) {
	d := NewMemMailDirectory()
	node := [32]byte{9}
	if _, ok := d.Get(node); ok {
		t.Fatal("found a target in an empty directory")
	}
	want := target(1, time.Unix(2_000_000_000, 0))
	if err := d.Put(node, want); err != nil {
		t.Fatal(err)
	}
	got, ok := d.Get(node)
	if !ok || got.Relay != want.Relay || got.Queue != want.Queue || !got.NotAfter.Equal(want.NotAfter) || !bytes.Equal(got.RelayCard, want.RelayCard) {
		t.Fatalf("%+v %v", got, ok)
	}
	d.Delete(node)
	if _, ok := d.Get(node); ok {
		t.Fatal("still there after Delete")
	}
}

func TestKVMailDirectoryKeepsTargetsUnderBlindedKeys(t *testing.T) {
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Destroy)
	dir := t.TempDir()
	open := func() (*store.Store, *store.IDs) {
		st, err := store.Open(store.Options{Dir: filepath.Join(dir, "v2"), Key: tw.Account.StorageKey()})
		if err != nil {
			t.Fatal(err)
		}
		ids, err := store.NewIDs(tw.Account.StorageKey())
		if err != nil {
			t.Fatal(err)
		}
		return st, ids
	}
	st, ids := open()
	d := &KVMailDirectory{KV: st.Store("v2/chat/mail"), IDs: ids}
	node := [32]byte{0x42, 0x42}
	want := target(3, time.Unix(2_000_000_000, 0))
	if err := d.Put(node, want); err != nil {
		t.Fatal(err)
	}
	entries, _ := st.Store("v2/chat/mail").Scan(nil, 100)
	if len(entries) == 0 {
		t.Fatal("nothing stored")
	}
	for _, e := range entries {
		if bytes.Contains(e.Key, node[:]) {
			t.Fatal("the key shows the contact")
		}
	}
	ids.Close()
	_ = st.Close()

	// after a restart
	st, ids = open()
	defer func() { ids.Close(); _ = st.Close() }()
	d = &KVMailDirectory{KV: st.Store("v2/chat/mail"), IDs: ids}
	got, ok := d.Get(node)
	if !ok || got.Queue != want.Queue || got.Relay != want.Relay {
		t.Fatalf("after a restart: %+v %v", got, ok)
	}
	d.Delete(node)
	if _, ok := d.Get(node); ok {
		t.Fatal("still there after Delete")
	}
}

// ---- announcing a queue ------------------------------------------------------------------------------

func TestAnnouncedQueueIsKeptByTheContact(t *testing.T) {
	w := newWorld(t)
	dirB := NewMemMailDirectory()
	w.tune = func(c *ServiceConfig) { c.MailDir = dirB }
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello") // a session first
	w.tick(a)

	ref := mailbox.QueueRef{Relay: [32]byte{7}, Queue: [16]byte{8}, Expires: w.clock.Now().Add(9 * 24 * time.Hour)}
	if err := a.svc.AnnounceQueue(b.self.Node, ref, []byte("relay card")); err != nil {
		t.Fatal(err)
	}
	w.tick(a)
	got, ok := dirB.Get(a.self.Node)
	if !ok || got.Relay != ref.Relay || got.Queue != ref.Queue || string(got.RelayCard) != "relay card" {
		t.Fatalf("b keeps %+v %v", got, ok)
	}
	if len(b.ctrl) != 0 {
		t.Fatalf("an announcement surfaced as an event: %v", b.ctrl)
	}
	// the newer announcement replaces the older one
	ref2 := mailbox.QueueRef{Relay: [32]byte{7}, Queue: [16]byte{9}, Expires: w.clock.Now().Add(9 * 24 * time.Hour)}
	_ = a.svc.AnnounceQueue(b.self.Node, ref2, nil)
	w.tick(a)
	if got, _ := dirB.Get(a.self.Node); got.Queue != ref2.Queue {
		t.Fatalf("b keeps the old queue: %+v", got)
	}
	// one that has ended is not kept
	old := mailbox.QueueRef{Relay: [32]byte{7}, Queue: [16]byte{10}, Expires: w.clock.Now().Add(-time.Hour)}
	_ = a.svc.AnnounceQueue(b.self.Node, old, nil)
	w.tick(a)
	if got, _ := dirB.Get(a.self.Node); got.Queue != ref2.Queue {
		t.Fatalf("an ended queue replaced the current one: %+v", got)
	}
}

// ---- items taken from a queue -----------------------------------------------------------------------------

func TestReceiveItemAcknowledgesWhatIsFinal(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hi")
	w.tick(a)
	env := w.sent[0].env
	// a repeat of something handled is final
	if !b.svc.ReceiveItem(context.Background(), env) {
		t.Fatal("a handled message was not acknowledged")
	}
	// garbage can never be handled: acknowledged, so that it does not stay for 7 days
	if !b.svc.ReceiveItem(context.Background(), []byte{1, 2, 3}) {
		t.Fatal("garbage was kept")
	}
	// a fresh message
	w.send(a, b, "second")
	w.tick(a)
	if !b.svc.ReceiveItem(context.Background(), w.sent[len(w.sent)-1].env) {
		t.Fatal("a repeat of the second was not acknowledged")
	}
	// an init from an account that cannot be looked up now: the item is kept, to be seen again
	ghost := fakeAccount()
	init := craft(t, b, b, ghost, fakeDevice(), []byte("init bytes"), true)
	if b.svc.ReceiveItem(context.Background(), init) {
		t.Fatal("an item whose sender could not be looked up was acknowledged")
	}
}

// ---- the term of a message -------------------------------------------------------------------------------

func TestExpiresShowsTheTerm(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.down[b.self.Device] = true
	start := w.clock.Now()
	id := w.send(a, b, "x")
	exp, ok := a.svc.Expires(id)
	if !ok || !exp.Equal(start.Add(7*24*time.Hour)) {
		t.Fatalf("waiting: %v %v", exp, ok)
	}
	w.clock.Add(time.Hour)
	w.tick(a) // goes to the mailbox an hour later
	if st, _ := a.svc.Status(id); st != StateMailbox {
		t.Fatalf("status %v", st)
	}
	exp, _ = a.svc.Expires(id)
	if !exp.Equal(start.Add(time.Hour).Add(7 * 24 * time.Hour)) {
		t.Fatalf("in the mailbox: %v, want 7 days after the deposit", exp)
	}
	if _, ok := a.svc.Expires([16]byte{1}); ok {
		t.Fatal("a term for a message that does not exist")
	}
}

// ---- the Mail over mailbox/2 --------------------------------------------------------------------------------

type fakeDepositor struct {
	calls  []depositCall
	status map[[32]byte]uint32 // by forwarding relay
	err    map[[32]byte]error
}

type depositCall struct {
	via  [32]byte
	t    mailbox.Target
	blob []byte
}

func (f *fakeDepositor) Deposit(ctx context.Context, via [32]byte, t mailbox.Target, blob []byte) (uint32, error) {
	f.calls = append(f.calls, depositCall{via, t, append([]byte(nil), blob...)})
	if err := f.err[via]; err != nil {
		return 0, err
	}
	return f.status[via], nil
}

func newMail(t *testing.T) (*MailboxMail, *fakeDepositor, *MemMailDirectory, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	d := NewMemMailDirectory()
	f := &fakeDepositor{status: map[[32]byte]uint32{}, err: map[[32]byte]error{}}
	m := &MailboxMail{Sender: f, Dir: d, Now: c.Now,
		Via: func() [][32]byte { return [][32]byte{{1}, {2}} },
		HS:  func(relay [32]byte, card []byte) ([]byte, error) { return bytes.Repeat([]byte{relay[0]}, 32), nil }}
	return m, f, d, c
}

func TestMailboxMailDeposits(t *testing.T) {
	m, f, d, c := newMail(t)
	node := [32]byte{5}
	_ = d.Put(node, target(7, c.Now().Add(time.Hour)))
	if err := m.Deposit(context.Background(), node, []byte("envelope")); err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0].via != ([32]byte{1}) || string(f.calls[0].blob) != "envelope" {
		t.Fatalf("calls %+v", f.calls)
	}
	tg := f.calls[0].t
	if tg.Relay != ([32]byte{7}) || !bytes.Equal(tg.Queue, []byte{7, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) || !bytes.Equal(tg.HS, bytes.Repeat([]byte{7}, 32)) || tg.Public {
		t.Fatalf("target %+v", tg)
	}
}

func TestMailboxMailWithoutAQueue(t *testing.T) {
	m, f, d, c := newMail(t)
	if err := m.Deposit(context.Background(), [32]byte{5}, []byte("e")); !errors.Is(err, ErrNoMailbox) {
		t.Fatalf("unknown contact: %v", err)
	}
	_ = d.Put([32]byte{5}, target(7, c.Now().Add(-time.Second)))
	if err := m.Deposit(context.Background(), [32]byte{5}, []byte("e")); !errors.Is(err, ErrNoMailbox) {
		t.Fatalf("a queue that ended: %v", err)
	}
	if _, ok := d.Get([32]byte{5}); ok {
		t.Fatal("an ended queue was kept")
	}
	if len(f.calls) != 0 {
		t.Fatal("something was deposited")
	}
}

func TestMailboxMailTriesTheNextForwardingRelay(t *testing.T) {
	m, f, d, c := newMail(t)
	_ = d.Put([32]byte{5}, target(7, c.Now().Add(time.Hour)))
	f.err[[32]byte{1}] = errors.New("F unreachable")
	if err := m.Deposit(context.Background(), [32]byte{5}, []byte("e")); err != nil {
		t.Fatalf("the second relay should take it: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("%d attempts", len(f.calls))
	}
	// F could not reach M: also the next one
	f.calls = nil
	f.err = map[[32]byte]error{}
	f.status[[32]byte{1}] = mailbox.StatusRelayUnreachable
	if err := m.Deposit(context.Background(), [32]byte{5}, []byte("e")); err != nil || len(f.calls) != 2 {
		t.Fatalf("relay-unreachable: %v, %d attempts", err, len(f.calls))
	}
}

func TestMailboxMailAnswersOfM(t *testing.T) {
	m, f, d, c := newMail(t)
	node := [32]byte{5}
	put := func() { _ = d.Put(node, target(7, c.Now().Add(time.Hour))) }

	put()
	f.status[[32]byte{1}] = mailbox.StatusUnknownQueue
	if err := m.Deposit(context.Background(), node, []byte("e")); !errors.Is(err, ErrNoMailbox) {
		t.Fatalf("unknown queue: %v", err)
	}
	if _, ok := d.Get(node); ok {
		t.Fatal("a queue M does not know was kept")
	}

	for _, st := range []uint32{mailbox.StatusFull, mailbox.StatusRefused, mailbox.StatusTooLarge, mailbox.StatusPowRequired} {
		put()
		f.status[[32]byte{1}], f.status[[32]byte{2}] = st, st
		err := m.Deposit(context.Background(), node, []byte("e"))
		if err == nil || errors.Is(err, ErrNoMailbox) {
			t.Fatalf("status %d: %v", st, err)
		}
		if _, ok := d.Get(node); !ok {
			t.Fatalf("status %d dropped the queue", st)
		}
	}
}

func TestMailboxMailKeyAndSize(t *testing.T) {
	m, f, d, c := newMail(t)
	_ = d.Put([32]byte{5}, target(7, c.Now().Add(time.Hour)))
	m.HS = func([32]byte, []byte) ([]byte, error) { return nil, errors.New("no card") }
	if err := m.Deposit(context.Background(), [32]byte{5}, []byte("e")); err == nil || errors.Is(err, ErrNoMailbox) {
		t.Fatalf("no key: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("deposited without the relay's key")
	}
	m.HS = func(relay [32]byte, card []byte) ([]byte, error) { return bytes.Repeat([]byte{1}, 32), nil }
	if err := m.Deposit(context.Background(), [32]byte{5}, make([]byte, chatpb.MaxEnvelope+1)); err == nil {
		t.Fatal("an envelope above the largest was deposited")
	}
}
