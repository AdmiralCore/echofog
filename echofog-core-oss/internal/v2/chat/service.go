package chat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	// NoJitter turns the +-20 % of the retry pauses off (tests, AD13).
	NoJitter bool
}

// Service is the delivery side of chat/2: outbox, receiving, deduplication and recovery.
type Service struct {
	cfg ServiceConfig

	tickMu sync.Mutex // one Tick at a time

	mu    sync.Mutex
	items map[[16]byte]*item
	fails map[[32]byte][]failure // by sender device
	recov map[[32]byte]time.Time // last recovery start by sender device
}

type failure struct {
	id [16]byte
	at time.Time
}

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
	s := &Service{cfg: cfg, items: map[[16]byte]*item{}, fails: map[[32]byte][]failure{}, recov: map[[32]byte]time.Time{}}
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
			s.items[it.ID] = &it
		}
		if len(page) < store.MaxScanLimit {
			break
		}
		after = page[len(page)-1].Key
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
	it := &item{ID: id, To: to, Plain: plain, State: StateWaiting, Next: s.cfg.Now().UnixNano(), ForceInit: forceInit,
		Control: msg.GetControl() != nil}
	s.mu.Lock()
	if err := s.persist(it); err != nil {
		s.mu.Unlock()
		return id, err
	}
	s.items[id] = it
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

	s.mu.Lock()
	var due []*item
	for id, it := range s.items {
		switch {
		case (it.State == StateDelivered || it.State == StateFailed) && now.Sub(time.Unix(it.Since, 0)) > doneFor:
			delete(s.items, id)
			_ = s.cfg.Store.Delete(outKey(id))
		case (it.State == StateWaiting || it.State == StateMailbox) && it.Next <= now.UnixNano():
			due = append(due, it)
		}
	}
	s.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].Next < due[j].Next })
	for _, it := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.attempt(ctx, it)
	}
	return nil
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

func (s *Service) attempt(ctx context.Context, it *item) {
	res, err := s.cfg.Dir.Resolve(ctx, it.To)
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

func seenKey(device [32]byte, id []byte) []byte {
	h := sha256.Sum256(append(append([]byte(nil), device[:]...), id...))
	return []byte(seenPrefix + hex.EncodeToString(h[:]))
}

// Receive handles one incoming envelope (from a DELIVER or from a mailbox) and returns the DELIVERED
// status. Every refusal is the same status 4: which check failed is not told (AD20).
func (s *Service) Receive(ctx context.Context, envelope []byte) uint32 {
	if len(envelope) > chatpb.MaxEnvelope {
		return chatpb.StatusTooLarge
	}
	raw, err := s.cfg.Crypto.OpenEnvelope(s.cfg.Self.Node, envelope)
	if err != nil {
		return chatpb.StatusRefused
	}
	in, err := chatpb.ParseInner(raw)
	if err != nil {
		return chatpb.StatusRefused
	}
	senderKey, err1 := keys.Parse(in.Sender)
	deviceKey, err2 := keys.Parse(in.Device)
	if err1 != nil || err2 != nil {
		return chatpb.StatusRefused
	}
	node, device := senderKey.ID(), deviceKey.ID()
	var id [16]byte
	copy(id[:], in.Id)

	if ok, _ := s.cfg.Store.Has(seenKey(device, in.Id)); ok {
		return chatpb.StatusDuplicate
	}

	var plain []byte
	if len(in.PqxdhInit) > 0 {
		res, err := s.cfg.Dir.Resolve(ctx, node)
		if err != nil || res == nil || res.Devices == nil || res.Devices.NodeID != node {
			return chatpb.StatusRefused
		}
		var pk *objects.Prekeys
		for _, p := range res.Prekeys {
			if p != nil && p.DeviceID == device && p.VouchedBy(res.Devices) {
				pk = p
			}
		}
		if pk == nil {
			return chatpb.StatusRefused
		}
		plain, err = s.cfg.Crypto.Accept(pk, res.Devices, in.PqxdhInit)
		switch {
		case errors.Is(err, chatcrypto.ErrDuplicate):
			return chatpb.StatusDuplicate
		case err != nil:
			s.noteFailure(device, node, id)
			return chatpb.StatusRefused
		}
		if bound, ok := s.cfg.Crypto.Peer(device); !ok || bound != node {
			return chatpb.StatusRefused
		}
	} else {
		bound, ok := s.cfg.Crypto.Peer(device)
		if !ok {
			return chatpb.StatusNoSession
		}
		if bound != node { // the sender is not the account the session is bound to: dropped
			return chatpb.StatusRefused
		}
		plain, err = s.cfg.Crypto.Decrypt(device, in.Ratchet)
		if err != nil {
			s.noteFailure(device, node, id)
			return chatpb.StatusRefused
		}
	}

	pm, err := chatpb.ParsePersonal(plain)
	if err != nil {
		return chatpb.StatusRefused
	}
	_ = s.cfg.Store.PutTTL(seenKey(device, in.Id), []byte{1}, seenFor+24*time.Hour)
	s.dispatch(node, device, id, pm)
	return chatpb.StatusOK
}

func (s *Service) dispatch(node, device [32]byte, id [16]byte, pm *chatpb.PersonalMessageProto) {
	if c := pm.GetControl(); c != nil {
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
	var list []failure
	seen := false
	for _, f := range s.fails[device] {
		if now.Sub(f.at) < failWindow {
			list = append(list, f)
			if f.id == id {
				seen = true
			}
		}
	}
	if !seen {
		list = append(list, failure{id: id, at: now})
	}
	s.fails[device] = list
	start := len(list) >= failThreshold && now.Sub(s.recov[device]) >= failWindow
	if start {
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
