package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awnumar/memguard"
	chatcrypto "github.com/echofog/echofog-core/internal/v2/chat/crypto"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/secret"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ---- a small world: users, a directory, direct links and a mailbox --------------------------

type world struct {
	t     *testing.T
	clock *clock
	mu    sync.Mutex
	users map[[32]byte]*peer // by node ID
	byDev map[[32]byte]*peer
	down  map[[32]byte]bool // devices that cannot be reached directly
	mail  map[[32]byte][][]byte
	// mailDown makes Deposit fail
	mailDown bool
	// tune changes the configuration of every service built after it is set
	tune func(*ServiceConfig)
	// resolves counts Directory.Resolve calls by node
	resolves map[[32]byte]int
	sent     []sentEnv // every envelope given to Link.Deliver or Mail.Deposit
}

type sentEnv struct {
	to     [32]byte
	env    []byte
	viaMBX bool
}

type peer struct {
	w      *world
	tw     *identity.Throwaway
	key    *secret.Value
	dir    string
	st     *store.Store
	ids    *store.IDs
	cr     *chatcrypto.Crypto
	svc    *Service
	dl     *objects.DeviceList
	self   Self
	inbox  []Incoming
	status map[[16]byte]State
	ctrl   []*chatpb.Control
	evMu   sync.Mutex // the event callbacks run on the goroutines of other peers
}

func newWorld(t *testing.T) *world {
	t.Helper()
	t.Cleanup(memguard.Purge)
	return &world{t: t, clock: &clock{t: time.Unix(1_800_000_000, 0)}, users: map[[32]byte]*peer{}, byDev: map[[32]byte]*peer{},
		down: map[[32]byte]bool{}, mail: map[[32]byte][][]byte{}, resolves: map[[32]byte]int{}}
}

func (w *world) newPeer() *peer {
	t := w.t
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Destroy)
	rawDL, err := tw.DeviceList(1, w.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	dl, err := objects.VerifyDeviceList(rawDL, w.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	p := &peer{w: w, tw: tw, key: secret.New(k), dir: t.TempDir(), dl: dl, status: map[[16]byte]State{},
		self: Self{Node: tw.Account.NodeID(), Device: tw.Device.ID(), Account: tw.Account.Public(), DeviceKey: tw.Device.Public()}}
	t.Cleanup(func() { p.close(); p.key.Destroy() })
	p.open()
	w.users[p.self.Node], w.byDev[p.self.Device] = p, p
	return p
}

func (p *peer) open() {
	t := p.w.t
	st, err := store.Open(store.Options{Dir: filepath.Join(p.dir, "v2"), Key: p.key})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := store.NewIDs(p.key)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := chatcrypto.New(chatcrypto.Config{Device: p.tw.Device, Store: st.Store, IDs: ids, Now: p.w.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	p.st, p.ids, p.cr = st, ids, cr
	sc := ServiceConfig{
		Self: p.self, Crypto: cr, Dir: (*dirFake)(p.w), Link: &linkFake{w: p.w, from: p}, Mail: &mailFake{w: p.w},
		Store: st.Store("v2/chat/outbox"), Now: p.w.clock.Now, NoJitter: true,
		Events: Events{
			Message: func(in Incoming) { p.evMu.Lock(); p.inbox = append(p.inbox, in); p.evMu.Unlock() },
			Status:  func(id [16]byte, s State) { p.evMu.Lock(); p.status[id] = s; p.evMu.Unlock() },
			Control: func(from [32]byte, c *chatpb.Control) { p.evMu.Lock(); p.ctrl = append(p.ctrl, c); p.evMu.Unlock() },
		},
	}
	if p.w.tune != nil {
		p.w.tune(&sc)
	}
	p.svc, err = NewService(sc)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
}

func (p *peer) close() {
	if p.cr != nil {
		p.cr.Close()
		p.cr = nil
	}
	if p.ids != nil {
		p.ids.Close()
		p.ids = nil
	}
	if p.st != nil {
		_ = p.st.Close()
		p.st = nil
	}
}

func (p *peer) restart() { p.close(); p.open() }

func (p *peer) prekeys() *objects.Prekeys {
	raw, err := p.cr.Prekeys()
	if err != nil {
		p.w.t.Fatal(err)
	}
	pk, err := objects.VerifyPrekeys(raw, p.w.clock.Now())
	if err != nil {
		p.w.t.Fatal(err)
	}
	return pk
}

type dirFake world

func (d *dirFake) Resolve(ctx context.Context, node [32]byte) (*dht.Resolved, error) {
	w := (*world)(d)
	w.mu.Lock()
	p := w.users[node]
	w.resolves[node]++
	w.mu.Unlock()
	if p == nil {
		return nil, errors.New("not found")
	}
	return &dht.Resolved{Devices: p.dl, Prekeys: []*objects.Prekeys{p.prekeys()}}, nil
}

type linkFake struct {
	w    *world
	from *peer
}

func (l *linkFake) Deliver(ctx context.Context, device [32]byte, env []byte) (uint32, error) {
	w := l.w
	w.mu.Lock()
	to := w.byDev[device]
	down := w.down[device]
	w.sent = append(w.sent, sentEnv{to: to.self.Node, env: env})
	w.mu.Unlock()
	if to == nil || down {
		return 0, errors.New("unreachable")
	}
	return to.svc.Receive(ctx, env), nil
}

type mailFake struct{ w *world }

func (m *mailFake) Deposit(ctx context.Context, node [32]byte, env []byte) error {
	m.w.mu.Lock()
	defer m.w.mu.Unlock()
	if m.w.mailDown {
		return errors.New("mailbox down")
	}
	m.w.mail[node] = append(m.w.mail[node], env)
	m.w.sent = append(m.w.sent, sentEnv{to: node, env: env, viaMBX: true})
	return nil
}

// drain hands every envelope waiting for p to p, as a mailbox fetch would, and returns the statuses.
func (w *world) drain(p *peer) []uint32 {
	w.mu.Lock()
	envs := w.mail[p.self.Node]
	w.mail[p.self.Node] = nil
	w.mu.Unlock()
	var out []uint32
	for _, e := range envs {
		out = append(out, p.svc.Receive(context.Background(), e))
	}
	return out
}

func text(s string) *chatpb.PersonalMessageProto {
	return &chatpb.PersonalMessageProto{Text: s, Timestamp: 1_800_000_000}
}

func (w *world) send(from, to *peer, s string) [16]byte {
	w.t.Helper()
	id, err := from.svc.Send(context.Background(), to.self.Node, text(s))
	if err != nil {
		w.t.Fatalf("Send: %v", err)
	}
	return id
}

func (w *world) tick(ps ...*peer) {
	w.t.Helper()
	for _, p := range ps {
		if err := p.svc.Tick(context.Background()); err != nil {
			w.t.Fatalf("Tick: %v", err)
		}
	}
}

func (p *peer) texts() []string {
	var out []string
	for _, in := range p.inbox {
		out = append(out, in.Message.Text)
	}
	return out
}

// ---- tests -------------------------------------------------------------------------------

func TestSendDirectIsDeliveredOnlyAfterTheRecipientConfirms(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	id := w.send(a, b, "hello")
	if st, ok := a.svc.Status(id); !ok || st != StateWaiting {
		t.Fatalf("before the first attempt: %v %v", st, ok)
	}
	w.tick(a)
	if got := b.texts(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("recipient got %v", got)
	}
	if b.inbox[0].From != a.self.Node || b.inbox[0].Device != a.self.Device || b.inbox[0].ID != id {
		t.Fatalf("incoming: %+v", b.inbox[0])
	}
	if st, _ := a.svc.Status(id); st != StateDelivered {
		t.Fatalf("status %v after a confirmed DELIVER", st)
	}
	if a.status[id] != StateDelivered {
		t.Fatal("no status event")
	}
	// the next message uses the ratchet
	id2 := w.send(a, b, "second")
	w.tick(a)
	if got := b.texts(); len(got) != 2 || got[1] != "second" {
		t.Fatalf("recipient got %v", got)
	}
	if st, _ := a.svc.Status(id2); st != StateDelivered {
		t.Fatalf("status %v", st)
	}
}

func TestBodyKindFirstInitThenRatchet(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "one")
	w.send(a, b, "two")
	w.tick(a)
	var kinds []string
	for _, s := range w.sent {
		inner, err := b.cr.OpenEnvelope(b.self.Node, s.env)
		if err != nil {
			t.Fatal(err)
		}
		in, err := chatpb.ParseInner(inner)
		if err != nil {
			t.Fatal(err)
		}
		if len(in.PqxdhInit) > 0 {
			kinds = append(kinds, "init")
		} else {
			kinds = append(kinds, "ratchet")
		}
		if !bytes.Equal(in.Sender, a.self.Account) || !bytes.Equal(in.Device, a.self.DeviceKey) {
			t.Fatal("Inner names another sender or device")
		}
	}
	if strings.Join(kinds, ",") != "init,ratchet" {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestOfflineRecipientGoesToMailboxNotDelivered(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.down[b.self.Device] = true
	id := w.send(a, b, "while you were away")
	w.tick(a)
	if st, _ := a.svc.Status(id); st != StateMailbox {
		t.Fatalf("status %v, want in-mailbox", st)
	}
	if len(w.mail[b.self.Node]) != 1 {
		t.Fatal("nothing was deposited")
	}
	// b comes back, fetches the mailbox and reads it; that alone does not make it delivered for a
	if got := w.drain(b); len(got) != 1 || got[0] != chatpb.StatusOK {
		t.Fatalf("mailbox fetch: %v", got)
	}
	if got := b.texts(); len(got) != 1 || got[0] != "while you were away" {
		t.Fatalf("b got %v", got)
	}
	if st, _ := a.svc.Status(id); st != StateMailbox {
		t.Fatalf("delivered without a confirmation: %v", st)
	}
	// b's delivery receipt travels back and confirms
	w.tick(b)
	if st, _ := a.svc.Status(id); st != StateDelivered {
		t.Fatalf("after the receipt: %v", st)
	}
}

func TestNothingReachableStaysWaiting(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.down[b.self.Device] = true
	w.mailDown = true
	id := w.send(a, b, "x")
	w.tick(a)
	if st, _ := a.svc.Status(id); st != StateWaiting {
		t.Fatalf("status %v", st)
	}
}

func TestLostStateIsRepairedByANewInit(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "first")
	w.tick(a)
	// a message of a for b, still queued when b loses its sessions (a new store)
	id := w.send(a, b, "lost")
	b.dir = t.TempDir()
	b.restart()
	w.tick(a) // b cannot open it and answers the same refusal as for a stranger
	if st, _ := a.svc.Status(id); st == StateDelivered {
		t.Fatal("delivered although b could not read it")
	}
	// b has no session, so the first message it sends is a fresh PQXDH init; a accepts it and
	// rebuilds what waited for the old session
	w.send(b, a, "i am here")
	w.tick(b)
	for i := 0; i < 3; i++ {
		w.clock.Add(time.Minute)
		w.tick(a, b)
	}
	if st, _ := a.svc.Status(id); st != StateDelivered {
		t.Fatalf("not delivered after b's fresh init: %v", st)
	}
	if got := b.texts(); len(got) < 2 || got[len(got)-1] != "lost" {
		t.Fatalf("b got %v", got)
	}
}

func TestOutboxSurvivesRestart(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.down[b.self.Device] = true
	w.mailDown = true
	id := w.send(a, b, "kept")
	w.tick(a)
	a.restart()
	if st, ok := a.svc.Status(id); !ok || st != StateWaiting {
		t.Fatalf("after a restart: %v %v", st, ok)
	}
	w.down[b.self.Device] = false
	w.clock.Add(time.Minute)
	w.tick(a)
	if got := b.texts(); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("b got %v", got)
	}
	if st, _ := a.svc.Status(id); st != StateDelivered {
		t.Fatalf("status %v", st)
	}
}

func TestRetryPausesFollowTheClock(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.down[b.self.Device] = true
	w.mailDown = true
	w.send(a, b, "x")
	attempts := func() int { return len(w.sent) }
	w.tick(a)
	n := attempts()
	if n != 1 {
		t.Fatalf("first attempt made %d sends", n)
	}
	// 2 s, 5 s, 15 s, 45 s, then every minute (v1's pace)
	for i, pause := range []time.Duration{2, 5, 15, 45, 60, 60} {
		pause *= time.Second
		w.clock.Add(pause - time.Second)
		w.tick(a)
		if attempts() != n {
			t.Fatalf("retry %d came before its pause", i)
		}
		w.clock.Add(time.Second)
		w.tick(a)
		n++
		if attempts() != n {
			t.Fatalf("retry %d did not come after %v", i, pause)
		}
	}
}

func TestReceiveDropsRepeatsAndMismatches(t *testing.T) {
	w := newWorld(t)
	a, b, c := w.newPeer(), w.newPeer(), w.newPeer()
	w.send(a, b, "hi")
	w.tick(a)
	first := w.sent[0].env

	if st := b.svc.Receive(context.Background(), first); st != chatpb.StatusDuplicate {
		t.Fatalf("replayed envelope: %d", st)
	}
	if len(b.texts()) != 1 {
		t.Fatal("a replay was delivered twice")
	}
	if st := b.svc.Receive(context.Background(), []byte{1, 2, 3}); st != chatpb.StatusRefused {
		t.Fatalf("garbage: %d", st)
	}
	if st := b.svc.Receive(context.Background(), bytes.Repeat([]byte{1}, chatpb.MaxEnvelope+1)); st != chatpb.StatusTooLarge {
		t.Fatalf("huge: %d", st)
	}
	// an envelope for somebody else that was sealed to b's key but names c as recipient
	if st := b.svc.Receive(context.Background(), sealTo(t, c, b, a, 1)); st != chatpb.StatusRefused {
		t.Fatalf("misaddressed: %d", st)
	}
	// a ratchet message from a device without a session
	if st := b.svc.Receive(context.Background(), craft(t, b, c, c.self.Account, c.self.DeviceKey, []byte("m"), false)); st != chatpb.StatusRefused {
		t.Fatalf("no session: %d (an unauthenticated sender learns nothing about sessions, AD20)", st)
	}
	// a ratchet message that names another account than the session is bound to is dropped
	other := craft(t, b, a, c.self.Account, a.self.DeviceKey, mustEnc(t, a, b, "forged sender"), false)
	if st := b.svc.Receive(context.Background(), other); st != chatpb.StatusRefused {
		t.Fatalf("sender not matching the session: %d", st)
	}
	// ... and so is one that names another device
	other = craft(t, b, a, a.self.Account, c.self.DeviceKey, mustEnc(t, a, b, "forged device"), false)
	if st := b.svc.Receive(context.Background(), other); st != chatpb.StatusRefused {
		t.Fatalf("device not matching the session: %d", st)
	}
	for _, s := range b.texts() {
		if strings.HasPrefix(s, "forged") {
			t.Fatalf("a forged message was shown: %q", s)
		}
	}
}

// craft builds a sealed envelope from sender (as named by account/device) to b with the given
// ratchet bytes; withInit makes the body an init instead.
func craft(t *testing.T, to, from *peer, account, device keys.AKey, body []byte, withInit bool) []byte {
	t.Helper()
	in := &chatpb.Inner{Sender: account, Device: device, Id: randID(), Time: 1_800_000_000}
	if withInit {
		in.PqxdhInit = body
	} else {
		in.Ratchet = body
	}
	raw, err := objects.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return sealRaw(t, to, to.self.Node, raw)
}

func sealRaw(t *testing.T, to *peer, node [32]byte, inner []byte) []byte {
	t.Helper()
	pk := to.prekeys()
	ik, err := keys.ParseKind(pk.Body.GetIk(), keys.X25519)
	if err != nil {
		t.Fatal(err)
	}
	env, err := chatcrypto.SealEnvelope(node, [32]byte(ik.Key()), inner)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// sealTo makes an envelope readable by recipient but addressed to the node ID of addressed.
func sealTo(t *testing.T, addressed, recipient, from *peer, _ int) []byte {
	t.Helper()
	in := &chatpb.Inner{Sender: from.self.Account, Device: from.self.DeviceKey, Id: randID(), Time: 1, Ratchet: []byte("x")}
	raw, _ := objects.Marshal(in)
	return sealRaw(t, recipient, addressed.self.Node, raw)
}

func mustEnc(t *testing.T, from, to *peer, s string) []byte {
	t.Helper()
	pt, err := chatpb.PadPersonal(&chatpb.PersonalMessageProto{Text: s})
	if err != nil {
		t.Fatal(err)
	}
	m, err := from.cr.Encrypt(to.self.Device, pt)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func randID() []byte { b := make([]byte, 16); _, _ = rand.Read(b); return b }

// Every envelope is its bucket plus fixed overheads: two messages in different buckets differ by
// exactly the bucket difference, and two in one bucket have the same size (services.md 4.3). At most
// four devices per test: memguard locks pages and a Windows process has little quota.
func TestEnvelopeSizesAreBucketPlusOverhead(t *testing.T) {
	w := newWorld(t)
	a, b, c, d := w.newPeer(), w.newPeer(), w.newPeer(), w.newPeer()
	one := func(to *peer, n int) int {
		before := len(w.sent)
		w.send(a, to, strings.Repeat("x", n))
		w.tick(a)
		if len(w.sent)-before != 1 {
			t.Fatalf("%d sends", len(w.sent)-before)
		}
		return len(w.sent[before].env)
	}
	// the first message to b starts the session and is an init; the rest are ratchet messages
	i10 := one(b, 10)
	r10, r100, r600, r3000 := one(b, 10), one(b, 100), one(b, 600), one(b, 3000)
	if r10 != r100 {
		t.Fatalf("two messages of one bucket differ: %d and %d", r10, r100)
	}
	if r600-r10 != 1024-256 || r3000-r10 != 4096-256 {
		t.Fatalf("ratchet sizes %d %d %d: bucket differences not kept", r10, r600, r3000)
	}
	// init messages to fresh recipients
	i3000 := one(c, 3000)
	if i3000-i10 != 4096-256 {
		t.Fatalf("init sizes %d and %d: bucket difference not kept", i10, i3000)
	}
	if i10 <= r10 {
		t.Fatalf("an init (%d) is not larger than a ratchet message (%d)", i10, r10)
	}
	// the largest message still fits one mailbox item
	iMax := one(d, 57300)
	if iMax > chatpb.MaxEnvelope {
		t.Fatalf("the largest init envelope is %d bytes", iMax)
	}
}

func TestMessageAboveTheLastBucketIsRefusedAtSend(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	if _, err := a.svc.Send(context.Background(), b.self.Node, text(strings.Repeat("x", 57400))); !errors.Is(err, chatpb.ErrTooLarge) {
		t.Fatalf("%v", err)
	}
}

// A reset that a device sends inside a message the receiver can open is authenticated: it makes
// the receiver start a fresh PQXDH session with that device (services.md 4.4, ЧМ-1).
func TestAuthenticatedResetStartsAFreshSession(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a)
	reset := &chatpb.PersonalMessageProto{Control: &chatpb.Control{Kind: &chatpb.Control_Reset_{Reset_: &chatpb.Reset{}}}}
	if _, err := b.svc.Send(context.Background(), a.self.Node, reset); err != nil {
		t.Fatal(err)
	}
	w.tick(b)
	// a has opened the reset; its next message starts a new session instead of using the old one
	w.send(a, b, "after the reset")
	w.tick(a)
	last := w.sent[len(w.sent)-1]
	inner, err := b.cr.OpenEnvelope(b.self.Node, last.env)
	if err != nil {
		t.Fatal(err)
	}
	in, err := chatpb.ParseInner(inner)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.PqxdhInit) == 0 {
		t.Fatal("the reset did not start a fresh PQXDH: the message went on the old session")
	}
	if got := b.texts(); got[len(got)-1] != "after the reset" {
		t.Fatalf("b got %v", got)
	}
	// and the new session works the other way too
	w.send(b, a, "back")
	w.tick(b)
	if got := a.texts(); got[len(got)-1] != "back" {
		t.Fatalf("a got %v", got)
	}
}

func TestFailuresOutsideTenMinutesDoNotAdd(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a)
	for i := 0; i < 3; i++ {
		m := mustEnc(t, a, b, "bent")
		m[len(m)-3] ^= 0x55
		b.svc.Receive(context.Background(), craft(t, b, a, a.self.Account, a.self.DeviceKey, m, false))
		w.clock.Add(6 * time.Minute)
	}
	w.tick(b)
	if len(a.ctrl) != 0 {
		t.Fatal("three failures spread over 18 minutes started a recovery")
	}
}

func TestSameMessageFailingRepeatedlyCountsOnce(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a)
	m := mustEnc(t, a, b, "bent")
	m[len(m)-3] ^= 0x55
	env := craft(t, b, a, a.self.Account, a.self.DeviceKey, m, false)
	for i := 0; i < 5; i++ {
		b.svc.Receive(context.Background(), env)
	}
	w.tick(b)
	if len(a.ctrl) != 0 {
		t.Fatal("one message repeated five times counted as five")
	}
}

func TestControlMessagesAreEvents(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a)
	knock := &chatpb.PersonalMessageProto{Control: &chatpb.Control{Kind: &chatpb.Control_Knock{Knock: &chatpb.Knock{Ask: true, Card: []byte{1}}}}}
	if _, err := a.svc.Send(context.Background(), b.self.Node, knock); err != nil {
		t.Fatal(err)
	}
	w.tick(a)
	if len(b.ctrl) != 1 || b.ctrl[0].GetKnock() == nil {
		t.Fatalf("b control events: %v", b.ctrl)
	}
	if len(b.inbox) != 1 {
		t.Fatalf("a control message was shown as a chat message: %d", len(b.inbox))
	}
	// a delivery receipt is never answered with a receipt
	w.tick(b)
	w.tick(a)
	if len(a.ctrl) != 0 {
		t.Fatalf("a delivery confirmation surfaced as a control event: %v", a.ctrl)
	}
}
