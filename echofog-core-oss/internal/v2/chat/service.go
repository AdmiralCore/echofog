package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	mrand "math/rand/v2"
	"sort"
	"sync"
	"time"

	chatcrypto "github.com/echofog/echofog-core/internal/v2/chat/crypto"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
	"google.golang.org/protobuf/proto"
)

// Crypto is the key agreement and ratchet of chat/2; *chatcrypto.Crypto satisfies it.
type Crypto interface {
	Start(peer *objects.Prekeys, dl *objects.DeviceList, first []byte) ([]byte, error)
	Accept(sender *objects.Prekeys, dl *objects.DeviceList, init []byte) ([]byte, error)
	Encrypt(device [32]byte, plain []byte) ([]byte, error)
	Decrypt(device [32]byte, msg []byte) ([]byte, error)
	Peer(device [32]byte) ([32]byte, bool)
	OpenEnvelope(self [32]byte, env []byte) ([]byte, error)
}

// Directory finds the devices and prekeys of an account; *dht.Client satisfies it.
type Directory interface {
	Resolve(ctx context.Context, node [32]byte) (*dht.Resolved, error)
}

// Link sends a DELIVER to a device over a session and returns the DELIVERED status. An error means
// the device could not be reached (no session, timeout): the message then goes to a mailbox.
type Link interface {
	Deliver(ctx context.Context, device [32]byte, envelope []byte) (uint32, error)
}

// Mail deposits an envelope for an account that is away (mailbox/2, T7.4). Until T7.4 a fake.
type Mail interface {
	Deposit(ctx context.Context, node [32]byte, envelope []byte) error
}

// State is where an outgoing message is (H17).
type State uint8

const (
	// StateWaiting: waiting for a connection.
	StateWaiting State = iota + 1
	// StateMailbox: handed to a mailbox; not yet confirmed by the recipient.
	StateMailbox
	// StateDelivered: the recipient confirmed it.
	StateDelivered
	// StateFailed: it cannot be sent (too large).
	StateFailed
)

// Incoming is a received message.
type Incoming struct {
	ID      [16]byte
	From    [32]byte // account node ID
	Device  [32]byte
	Message *chatpb.PersonalMessageProto
}

// Events receives what happens; any field may be nil.
type Events struct {
	Message func(Incoming)
	Status  func(id [16]byte, st State)
	// Control is a knock, a reset or a read receipt (a delivery confirmation updates the status of the
	// message instead).
	Control func(from [32]byte, c *chatpb.Control)
}

// Self is the local identity.
type Self struct {
	Node      [32]byte
	Device    [32]byte
	Account   keys.AKey // akey of the account key (Inner.sender)
	DeviceKey keys.AKey // akey of the device key (Inner.device)
}

// ServiceConfig configures a Service.
type ServiceConfig struct {
	Self   Self
	Crypto Crypto
	Dir    Directory
	Link   Link
	Mail   Mail
	Store  store.KV
	Now    func() time.Time
	Events Events
	// MailDir remembers the queues contacts announce (Control.mailbox); nil ignores announcements.
	MailDir MailDirectory
	// IsContact says whether an account may announce a queue; nil accepts every account that got through
	// the authentication of a session (the directory is bounded either way).
	IsContact func(node [32]byte) bool
	// NoJitter turns the +-20 % of the retry pauses off (tests, AD13).
	NoJitter bool

	// Limits; zero means the default below.
	MaxOutbox        int // all outgoing items
	MaxOutboxPerPeer int // outgoing items for one account
	MaxOutboxBytes   int // plaintext and envelope bytes of all outgoing items
	MaxSeen          int // remembered incoming message IDs
	MaxSeenPerDevice int // ... of one sender device
}

// Default limits.
const (
	DefaultMaxOutbox        = 4096
	DefaultMaxOutboxPerPeer = 512
	DefaultMaxOutboxBytes   = 64 << 20
	DefaultMaxSeen          = 500_000
	DefaultMaxSeenPerDevice = 50_000
	// MaxReceiptsPerPeer bounds the queued delivery receipts for one account; more are dropped (the
	// recipient already has the message, a receipt only updates a status).
	MaxReceiptsPerPeer = 16
)

// maxTrackedDevices bounds each of the failure tables (one entry per sender device).
const maxTrackedDevices = 1024

// maxUnboundDevices bounds the table of failing devices we have no session with.
const maxUnboundDevices = 64

// ErrOutboxFull is Send with the outbox at a limit.
var ErrOutboxFull = errors.New("chat: outbox full")

// Stats are the counters of the outbox and of the remembered IDs.
type Stats struct {
	Outbox  int
	PerPeer map[[32]byte]int
	Bytes   int
	Seen    int
}

// Service is the delivery side of chat/2: outbox, receiving, deduplication and recovery.
type Service struct {
	cfg ServiceConfig

	tickMu sync.Mutex // one Tick at a time

	mu    sync.Mutex
	items map[[16]byte]*item
	fails map[[32]byte][]failure // by sender device, at most maxTrackedDevices
	recov map[[32]byte]time.Time // last recovery start by sender device, at most maxTrackedDevices

	// counters (accounting.go): active outbox items, by account, receipts by account, bytes
	count    int
	perPeer  map[[32]byte]int
	receipts map[[32]byte]int
	bytes    int
	// remembered incoming IDs: in all and by sender device
	seenN   int
	seenDev map[[32]byte]int
	// lookups of accounts that failed, until when they are not tried again (finding 6)
	lookupFail map[lookupKey]time.Time
	unbound    map[[32]byte][]failure // failures of devices we have no session with
	unboundAt  []time.Time            // recoveries started for them in the last hour
	receiptN   int
	seq        uint64
}

type failure struct {
	id   [16]byte
	at   time.Time
	node [32]byte // the account the envelope named (unbound failures only)
}

// lookupKey is an (account, device) pair named by an incoming init.
type lookupKey struct{ node, device [32]byte }

// item is one outgoing message in the outbox.
type item struct {
	ID        [16]byte `json:"id"`
	To        [32]byte `json:"to"`
	Plain     []byte   `json:"plain,omitempty"`
	Env       []byte   `json:"env,omitempty"`
	Device    [32]byte `json:"device"`
	State     State    `json:"state"`
	Attempts  int      `json:"attempts"`
	Next      int64    `json:"next"` // unix nanoseconds
	ForceInit bool     `json:"force_init,omitempty"`
	Control   bool     `json:"control,omitempty"`
	Receipt   bool     `json:"receipt,omitempty"`
	Created   int64    `json:"created"` // unix seconds
	Seq       uint64   `json:"seq"`     // order of sending, so that messages made in one instant keep it
	acct      int      // bytes counted for it while it is active, 0 when it is not counted
	Since     int64    `json:"since"` // when it entered StateMailbox, StateDelivered or StateFailed, unix seconds
}

const (
	outPrefix  = "out:"
	seenPrefix = "seen:"

	// seenFor is how long a received message ID is remembered (AD19: the logical term; the store TTL
	// is a day more).
	seenFor = 14 * 24 * time.Hour
	// doneFor is how long a delivered message stays in the outbox so that its status can be read.
	doneFor = 7 * 24 * time.Hour
	// mailboxFor is how long a deposit is trusted: mailbox items live 7 days (services.md 5.2).
	mailboxFor   = 7 * 24 * time.Hour
	mailboxRetry = 5 * time.Minute

	failWindow    = 10 * time.Minute
	failThreshold = 3
)

// backoff is the pause after the n-th attempt: 2 s, 5 s, 15 s, 45 s, then every minute (v1's pace).
func backoff(n int) time.Duration {
	switch n {
	case 0:
		return 2 * time.Second
	case 1:
		return 5 * time.Second
	case 2:
		return 15 * time.Second
	case 3:
		return 45 * time.Second
	}
	return time.Minute
}

// NewService builds a Service and loads the outbox from the store.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Crypto == nil || cfg.Dir == nil || cfg.Link == nil || cfg.Store == nil || cfg.Now == nil {
		return nil, errors.New("chat: Crypto, Dir, Link, Store and Now are required")
	}
	s := &Service{cfg: cfg, items: map[[16]byte]*item{}, fails: map[[32]byte][]failure{}, recov: map[[32]byte]time.Time{},
		perPeer: map[[32]byte]int{}, receipts: map[[32]byte]int{}, seenDev: map[[32]byte]int{}, lookupFail: map[lookupKey]time.Time{}, unbound: map[[32]byte][]failure{}}
	var after []byte
	for {
		var (
			page []store.Entry
			err  error
		)
		if after == nil {
			page, err = cfg.Store.Scan([]byte(outPrefix), store.MaxScanLimit)
		} else {
			page, err = cfg.Store.ScanFrom([]byte(outPrefix), after, store.MaxScanLimit)
		}
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			var it item
			if json.Unmarshal(e.Value, &it) != nil {
				continue // a record this build cannot read is skipped, not fatal
			}
			if it.Created == 0 {
				it.Created = cfg.Now().Unix() // a record from before the lifetimes: it starts now
			}
			s.items[it.ID] = &it
			if it.Seq > s.seq {
				s.seq = it.Seq
			}
			if it.active() {
				s.addLocked(&it)
			}
		}
		if len(page) < store.MaxScanLimit {
			break
		}
		after = page[len(page)-1].Key
	}
	if err := s.loadSeen(); err != nil {
		return nil, err
	}
	return s, nil
}

func outKey(id [16]byte) []byte { return []byte(outPrefix + hex.EncodeToString(id[:])) }

// persist stores the item. s.mu is held.
func (s *Service) persist(it *item) error {
	raw, err := json.Marshal(it)
	if err != nil {
		return err
	}
	return s.cfg.Store.Put(outKey(it.ID), raw)
}

// Send queues a message for the account to and returns its ID. The message is stored before this
// returns, so it survives a restart. A message above the largest bucket is refused: it goes as a file.
func (s *Service) Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error) {
	return s.enqueue(to, m, false)
}

func (s *Service) enqueue(to [32]byte, m *chatpb.PersonalMessageProto, forceInit bool) ([16]byte, error) {
	var id [16]byte
	if m == nil {
		return id, errors.New("chat: no message")
	}
	if _, err := rand.Read(id[:]); err != nil {
		return id, err
	}
	msg := proto.Clone(m).(*chatpb.PersonalMessageProto)
	msg.Id = hex.EncodeToString(id[:])
	plain, err := chatpb.PadPersonal(msg)
	if err != nil {
		return id, err
	}
	now := s.cfg.Now()
	it := &item{ID: id, To: to, Plain: plain, State: StateWaiting, Next: now.UnixNano(), ForceInit: forceInit,
		Control: msg.GetControl() != nil, Receipt: msg.GetControl().GetDelivered() != nil, Created: now.Unix()}
	s.mu.Lock()
	if !s.admitLocked(it) {
		s.mu.Unlock()
		return id, ErrOutboxFull
	}
	if err := s.persist(it); err != nil {
		s.mu.Unlock()
		return id, err
	}
	s.seq++
	it.Seq = s.seq
	s.items[id] = it
	s.addLocked(it)
	_ = s.persist(it)
	s.mu.Unlock()
	s.emitStatus(id, StateWaiting)
	return id, nil
}

// Status reports where an outgoing message is.
func (s *Service) Status(id [16]byte) (State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return 0, false
	}
	return it.State, true
}

func (s *Service) emitStatus(id [16]byte, st State) {
	if f := s.cfg.Events.Status; f != nil {
		f(id, st)
	}
}

// Tick works on the outbox items that are due. The runtime calls it on a timer.
func (s *Service) Tick(ctx context.Context) error {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	now := s.cfg.Now()

	s.expireSeen(now)
	if p, ok := s.cfg.MailDir.(interface{ Prune(time.Time) }); ok {
		p.Prune(now)
	}

	s.mu.Lock()
	var due, failed []*item
	for _, it := range s.items {
		switch {
		case !it.active():
			if now.Sub(time.Unix(it.Since, 0)) > doneFor {
				s.removeLocked(it)
			}
		case it.Receipt && now.Sub(time.Unix(it.Created, 0)) > receiptLife:
			s.removeLocked(it) // a receipt that waited a day is not worth sending any more
		case now.Sub(time.Unix(it.Created, 0)) > outboxLife:
			failed = append(failed, it)
		case it.Next <= now.UnixNano():
			due = append(due, it)
		}
	}
	for k, until := range s.lookupFail {
		if !now.Before(until) {
			delete(s.lookupFail, k)
		}
	}
	s.mu.Unlock()
	for _, it := range failed {
		s.set(it, StateFailed, time.Time{})
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].Next != due[j].Next {
			return due[i].Next < due[j].Next
		}
		return due[i].Seq < due[j].Seq
	})
	lookups := map[[32]byte]lookup{} // one lookup per account and Tick, not one per item
	for _, it := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.attempt(ctx, it, lookups)
	}
	return nil
}

type lookup struct {
	res *dht.Resolved
	err error
}

// set moves an item to a state and stores it.
func (s *Service) set(it *item, st State, next time.Time) {
	s.mu.Lock()
	changed := it.State != st
	it.State = st
	if st == StateDelivered || st == StateFailed || (st == StateMailbox && changed) {
		it.Since = s.cfg.Now().Unix()
	}
	if st == StateDelivered || st == StateFailed {
		s.dropLocked(it)
		it.Plain, it.Env = nil, nil
	}
	if !next.IsZero() {
		it.Next = next.UnixNano()
	}
	_ = s.persist(it)
	s.mu.Unlock()
	if changed {
		s.emitStatus(it.ID, st)
	}
}

// retry schedules the next attempt after the n-th one.
func (s *Service) retry(it *item, st State) {
	s.mu.Lock()
	n := it.Attempts
	it.Attempts++
	s.mu.Unlock()
	pause := backoff(n)
	if !s.cfg.NoJitter {
		pause = time.Duration(float64(pause) * (0.8 + 0.4*mrand.Float64())) // +-20 % (AD13)
	}
	s.set(it, st, s.cfg.Now().Add(pause))
}

func (s *Service) attempt(ctx context.Context, it *item, lookups map[[32]byte]lookup) {
	l, ok := lookups[it.To]
	if !ok {
		l.res, l.err = s.cfg.Dir.Resolve(ctx, it.To)
		lookups[it.To] = l
	}
	res, err := l.res, l.err
	if err != nil || res == nil || res.Devices == nil {
		s.retry(it, s.keepState(it))
		return
	}
	pk := pickPrekeys(res)
	if pk == nil {
		s.retry(it, s.keepState(it))
		return
	}
	s.mu.Lock()
	needBuild := len(it.Env) == 0 || it.Device != pk.DeviceID
	s.mu.Unlock()
	if needBuild {
		env, err := s.build(it, pk, res.Devices)
		if err != nil {
			if errors.Is(err, errTooLarge) {
				s.set(it, StateFailed, time.Time{})
				return
			}
			s.retry(it, s.keepState(it))
			return
		}
		s.mu.Lock()
		it.Env, it.Device, it.ForceInit = env, pk.DeviceID, false
		s.resizeLocked(it)
		_ = s.persist(it)
		s.mu.Unlock()
	}
	status, err := s.cfg.Link.Deliver(ctx, it.Device, it.Env)
	if err == nil {
		switch status {
		case chatpb.StatusOK, chatpb.StatusDuplicate:
			s.set(it, StateDelivered, time.Time{})
		case chatpb.StatusNoSession:
			// the recipient lost the session: start again with a fresh PQXDH
			s.mu.Lock()
			it.Env, it.ForceInit = nil, true
			s.resizeLocked(it)
			s.mu.Unlock()
			s.retry(it, StateWaiting)
		case chatpb.StatusTooLarge:
			s.set(it, StateFailed, time.Time{})
		default:
			s.retry(it, s.keepState(it))
		}
		return
	}
	// not reachable directly: a mailbox takes it once; the recipient's receipt confirms it
	s.mu.Lock()
	inMailbox := it.State == StateMailbox
	expired := inMailbox && s.cfg.Now().Sub(time.Unix(it.Since, 0)) > mailboxFor
	s.mu.Unlock()
	if inMailbox && !expired {
		s.set(it, StateMailbox, s.cfg.Now().Add(mailboxRetry))
		return
	}
	if s.cfg.Mail != nil {
		if err := s.cfg.Mail.Deposit(ctx, it.To, it.Env); err == nil {
			s.mu.Lock()
			it.State = StateWaiting // so that set stamps the time of this deposit
			s.mu.Unlock()
			s.set(it, StateMailbox, s.cfg.Now().Add(mailboxRetry))
			return
		}
	}
	s.retry(it, StateWaiting)
}

// keepState is the state an item keeps when an attempt failed: one in a mailbox stays there.
func (s *Service) keepState(it *item) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it.State == StateMailbox {
		return StateMailbox
	}
	return StateWaiting
}

var errTooLarge = errors.New("chat: envelope above the limit")

// pickPrekeys chooses the device to send to: the first listed device whose prekeys the list vouches
// for (per-device fan-out is later, services.md 4.4).
func pickPrekeys(res *dht.Resolved) *objects.Prekeys {
	for _, p := range res.Prekeys {
		if p != nil && p.VouchedBy(res.Devices) {
			return p
		}
	}
	return nil
}

// build makes the sealed envelope of an item for a device.
func (s *Service) build(it *item, pk *objects.Prekeys, dl *objects.DeviceList) ([]byte, error) {
	s.mu.Lock()
	plain, force := it.Plain, it.ForceInit
	s.mu.Unlock()
	in := &chatpb.Inner{Sender: s.cfg.Self.Account, Device: s.cfg.Self.DeviceKey, Id: it.ID[:], Time: uint64(s.cfg.Now().Unix())}
	if !force {
		m, err := s.cfg.Crypto.Encrypt(pk.DeviceID, plain)
		switch {
		case err == nil:
			in.Ratchet = m
		case !errors.Is(err, chatcrypto.ErrNoSession):
			return nil, err
		}
	}
	if in.Ratchet == nil {
		init, err := s.cfg.Crypto.Start(pk, dl, plain)
		if err != nil {
			return nil, err
		}
		in.PqxdhInit = init
	}
	raw, err := objects.Marshal(in)
	if err != nil {
		return nil, err
	}
	ik, err := keys.ParseKind(pk.Body.GetIk(), keys.X25519)
	if err != nil {
		return nil, err
	}
	env, err := chatcrypto.SealEnvelope(dl.NodeID, [32]byte(ik.Key()), raw)
	if err != nil {
		return nil, err
	}
	if len(env) > chatpb.MaxEnvelope {
		return nil, errTooLarge
	}
	return env, nil
}

// Receive handles one incoming envelope (from a DELIVER or from a mailbox) and returns the DELIVERED
// status. Every refusal is the same status 4: which check failed is not told (AD20).
func (s *Service) Receive(ctx context.Context, envelope []byte) uint32 {
	status, _ := s.receive(ctx, envelope)
	return status
}

// receive is Receive; transient says the refusal may pass (the sender could not be looked up now), so
// an item taken from a mailbox queue is kept.
func (s *Service) receive(ctx context.Context, envelope []byte) (status uint32, transient bool) {
	if len(envelope) > chatpb.MaxEnvelope {
		return chatpb.StatusTooLarge, false
	}
	raw, err := s.cfg.Crypto.OpenEnvelope(s.cfg.Self.Node, envelope)
	if err != nil {
		return chatpb.StatusRefused, false
	}
	in, err := chatpb.ParseInner(raw)
	if err != nil {
		return chatpb.StatusRefused, false
	}
	senderKey, err1 := keys.Parse(in.Sender)
	deviceKey, err2 := keys.Parse(in.Device)
	if err1 != nil || err2 != nil {
		return chatpb.StatusRefused, false
	}
	node, device := senderKey.ID(), deviceKey.ID()
	var id [16]byte
	copy(id[:], in.Id)

	if s.seenBefore(device, in.Id) {
		return chatpb.StatusDuplicate, false
	}

	var plain []byte
	if len(in.PqxdhInit) > 0 {
		key := lookupKey{node, device}
		if s.lookupFailed(key) {
			return chatpb.StatusRefused, true
		}
		res, err := s.cfg.Dir.Resolve(ctx, node)
		if err != nil {
			// only a failed lookup is remembered, per (account, device): an envelope that names a device
			// the account does not have must not keep the account's real init out
			s.noteLookupFailed(key)
			return chatpb.StatusRefused, true
		}
		if res == nil || res.Devices == nil || res.Devices.NodeID != node {
			return chatpb.StatusRefused, false
		}
		var pk *objects.Prekeys
		for _, p := range res.Prekeys {
			if p != nil && p.DeviceID == device && p.VouchedBy(res.Devices) {
				pk = p
			}
		}
		if pk == nil {
			return chatpb.StatusRefused, false
		}
		plain, err = s.cfg.Crypto.Accept(pk, res.Devices, in.PqxdhInit)
		switch {
		case errors.Is(err, chatcrypto.ErrDuplicate):
			return chatpb.StatusDuplicate, false
		case err != nil:
			s.noteFailure(device, node, id)
			return chatpb.StatusRefused, false
		}
		if bound, ok := s.cfg.Crypto.Peer(device); !ok || bound != node {
			return chatpb.StatusRefused, false
		}
		s.invalidate(node) // what waits for this account was made for the old session
	} else {
		// A ratchet message from a device we have no session with and one that fails to open get the
		// same answer: an envelope can be sealed by anybody, so a different answer would tell whether
		// this node talks to the device (AD20). The sender is helped by the recovery after three
		// failures (noteFailure), not by a status.
		bound, ok := s.cfg.Crypto.Peer(device)
		switch {
		case !ok:
			// no session: counted apart, against one small budget (noteUnbound)
			s.noteUnbound(device, node, id)
			return chatpb.StatusRefused, false
		case bound != node:
			// a device we have a session with, named by another account: an impersonation, never a
			// reason to start anything
			return chatpb.StatusRefused, false
		}
		plain, err = s.cfg.Crypto.Decrypt(device, in.Ratchet)
		if err != nil {
			s.noteFailure(device, node, id)
			return chatpb.StatusRefused, false
		}
	}

	pm, err := chatpb.ParsePersonal(plain)
	if err != nil {
		return chatpb.StatusRefused, false
	}
	s.rememberSeen(device, in.Id)
	s.dispatch(node, device, id, pm)
	return chatpb.StatusOK, false
}

func (s *Service) dispatch(node, device [32]byte, id [16]byte, pm *chatpb.PersonalMessageProto) {
	if c := pm.GetControl(); c != nil {
		if q := c.GetMailbox(); q != nil {
			// where the contact receives mail while away; the newest announcement wins, one that has
			// ended is ignored
			if s.cfg.MailDir != nil && (s.cfg.IsContact == nil || s.cfg.IsContact(node)) {
				var t MailTarget
				copy(t.Relay[:], q.GetRelay())
				copy(t.Queue[:], q.GetQueue())
				t.NotAfter = time.Unix(int64(q.GetNotAfter()), 0)
				t.RelayCard = q.GetRelayCard()
				// it must end in the future and within a queue's longest life (30 days)
				if now := s.cfg.Now(); now.Before(t.NotAfter) && !t.NotAfter.After(now.Add(maxQueueLife)) {
					if old, ok := s.cfg.MailDir.Get(node); !ok || !t.NotAfter.Before(old.NotAfter) {
						_ = s.cfg.MailDir.Put(node, t)
					}
				}
			}
			return
		}
		if d := c.GetDelivered(); d != nil {
			for _, raw := range d.GetIds() {
				var mid [16]byte
				copy(mid[:], raw)
				s.mu.Lock()
				it, ok := s.items[mid]
				confirmed := ok && it.To == node && it.State != StateFailed
				s.mu.Unlock()
				if confirmed {
					s.set(it, StateDelivered, time.Time{})
				}
			}
			return
		}
		if f := s.cfg.Events.Control; f != nil {
			f(node, c)
		}
		return
	}
	if f := s.cfg.Events.Message; f != nil {
		f(Incoming{ID: id, From: node, Device: device, Message: pm})
	}
	// tell the sender it arrived: the only confirmation for a message that went through a mailbox
	receipt := &chatpb.PersonalMessageProto{Control: &chatpb.Control{Kind: &chatpb.Control_Delivered{Delivered: &chatpb.Receipt{Ids: [][]byte{id[:]}}}}}
	_, _ = s.enqueue(node, receipt, false)
}

// noteFailure records a message that did not open. Three distinct ones from one device within ten
// minutes make this side start a fresh PQXDH session itself (services.md 4.4).
func (s *Service) noteFailure(device, node [32]byte, id [16]byte) {
	now := s.cfg.Now()
	s.mu.Lock()
	s.pruneFailuresLocked(now)
	var list []failure
	seen := false
	for _, f := range s.fails[device] {
		list = append(list, f)
		if f.id == id {
			seen = true
		}
	}
	if _, known := s.fails[device]; !known && len(s.fails) >= maxTrackedDevices {
		s.evictOldestFailureLocked()
	}
	if !seen {
		list = append(list, failure{id: id, at: now})
	}
	s.fails[device] = list
	start := len(list) >= failThreshold && now.Sub(s.recov[device]) >= failWindow
	if start {
		if _, known := s.recov[device]; !known && len(s.recov) >= maxTrackedDevices {
			s.evictOldestRecoveryLocked()
		}
		s.recov[device] = now
		delete(s.fails, device)
	}
	s.mu.Unlock()
	if start {
		// the first message of the new session says so; the old session is kept 7 days
		reset := &chatpb.PersonalMessageProto{Control: &chatpb.Control{Kind: &chatpb.Control_Reset_{Reset_: &chatpb.Reset{}}}}
		_, _ = s.enqueue(node, reset, true)
	}
}

// pruneFailuresLocked drops what is older than the window. s.mu is held.
func (s *Service) pruneFailuresLocked(now time.Time) {
	for dev, list := range s.fails {
		kept := list[:0:0]
		for _, f := range list {
			if now.Sub(f.at) < failWindow {
				kept = append(kept, f)
			}
		}
		if len(kept) == 0 {
			delete(s.fails, dev)
		} else {
			s.fails[dev] = kept
		}
	}
	for dev, at := range s.recov {
		if now.Sub(at) >= failWindow {
			delete(s.recov, dev)
		}
	}
}

func (s *Service) evictOldestFailureLocked() {
	var oldest [32]byte
	var at time.Time
	first := true
	for dev, list := range s.fails {
		last := list[len(list)-1].at
		if first || last.Before(at) {
			oldest, at, first = dev, last, false
		}
	}
	delete(s.fails, oldest)
}

func (s *Service) evictOldestRecoveryLocked() {
	var oldest [32]byte
	var at time.Time
	first := true
	for dev, t := range s.recov {
		if first || t.Before(at) {
			oldest, at, first = dev, t, false
		}
	}
	delete(s.recov, oldest)
}

// lookupPause is how long an account whose lookup failed is not looked up again for an incoming init.
const lookupPause = time.Minute

func (s *Service) lookupFailed(key lookupKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.lookupFail[key]
	return ok && s.cfg.Now().Before(until)
}

func (s *Service) noteLookupFailed(key lookupKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lookupFail[key]; !ok && len(s.lookupFail) >= maxTrackedDevices {
		return // full: skip remembering, never grow
	}
	s.lookupFail[key] = s.cfg.Now().Add(lookupPause)
}

// unboundBudget is how many recoveries may be started in an hour for devices we have no session with
// (all of them together): the envelope that names such a device is not authenticated, so this is
// small. The recovery of a lost state needs no more.
const unboundBudget = 2

// noteUnbound records a ratchet message from a device we have no session with. Three distinct ones
// within ten minutes start a recovery, if the shared budget of the hour allows.
func (s *Service) noteUnbound(device, node [32]byte, id [16]byte) {
	now := s.cfg.Now()
	s.mu.Lock()
	for dev, list := range s.unbound {
		kept := list[:0:0]
		for _, f := range list {
			if now.Sub(f.at) < failWindow {
				kept = append(kept, f)
			}
		}
		if len(kept) == 0 {
			delete(s.unbound, dev)
		} else {
			s.unbound[dev] = kept
		}
	}
	list := s.unbound[device]
	for _, f := range list {
		if f.id == id {
			s.mu.Unlock()
			return
		}
	}
	if _, known := s.unbound[device]; !known && len(s.unbound) >= maxUnboundDevices {
		var oldest [32]byte
		var at time.Time
		first := true
		for dev, l := range s.unbound {
			if last := l[len(l)-1].at; first || last.Before(at) {
				oldest, at, first = dev, last, false
			}
		}
		delete(s.unbound, oldest)
	}
	list = append(list, failure{id: id, at: now, node: node})
	s.unbound[device] = list
	start := false
	if len(list) >= failThreshold {
		delete(s.unbound, device)
		kept := s.unboundAt[:0:0]
		for _, t := range s.unboundAt {
			if now.Sub(t) < time.Hour {
				kept = append(kept, t)
			}
		}
		s.unboundAt = kept
		if len(kept) < unboundBudget {
			s.unboundAt = append(s.unboundAt, now)
			start = true
		}
	}
	s.mu.Unlock()
	if start {
		reset := &chatpb.PersonalMessageProto{Control: &chatpb.Control{Kind: &chatpb.Control_Reset_{Reset_: &chatpb.Reset{}}}}
		_, _ = s.enqueue(node, reset, true)
	}
}

// invalidate drops the envelopes made for an account before its session was replaced: they cannot be
// opened by the new one, so they are made again at the next attempt.
func (s *Service) invalidate(node [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.To == node && it.active() && len(it.Env) > 0 {
			it.Env = nil
			s.resizeLocked(it)
			_ = s.persist(it)
		}
	}
}

// AnnounceQueue tells a contact where this node receives mail while away (services.md 5.2). It is
// sent as an end-to-end message like any other, and again whenever the queue rotates.
func (s *Service) AnnounceQueue(to [32]byte, q mailbox.QueueRef, card []byte) error {
	msg := &chatpb.PersonalMessageProto{Control: &chatpb.Control{Kind: &chatpb.Control_Mailbox{Mailbox: &chatpb.MailboxQueue{
		Relay: q.Relay[:], Queue: q.Queue[:], NotAfter: uint64(q.Expires.Unix()), RelayCard: card,
	}}}}
	_, err := s.enqueue(to, msg, false)
	return err
}

// ReceiveItem handles an envelope taken from a mailbox queue and says whether the item may be
// acknowledged: true once the envelope was handled or can never be, false when handling failed for a
// reason that may pass (a lookup that failed), so that the relay keeps the item.
func (s *Service) ReceiveItem(ctx context.Context, envelope []byte) bool {
	_, transient := s.receive(ctx, envelope)
	return !transient
}

// Expires says when an outgoing message that is not delivered by then is given up: seven days after it
// was queued, or after it was handed to a mailbox.
func (s *Service) Expires(id [16]byte) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok || !it.active() {
		return time.Time{}, false
	}
	if it.State == StateMailbox {
		return time.Unix(it.Since, 0).Add(mailboxFor), true
	}
	return time.Unix(it.Created, 0).Add(outboxLife), true
}
