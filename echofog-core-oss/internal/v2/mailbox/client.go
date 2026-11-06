package mailbox

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
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
// queue it first finds the proof of work; ctx cancels the search. An error means F could not be
// used (the caller picks another F); a status other than StatusStored is M's answer.
func (s *Sender) Deposit(ctx context.Context, via [32]byte, t Target, blob []byte) (uint32, error) {
	if len(blob) > MaxBlob {
		return 0, ErrTooLarge
	}
	if len(t.Queue) != QueueSize || len(t.HS) != 32 {
		return 0, ErrBadRequest
	}
	dep := Deposit{Req: 1, Queue: t.Queue, Blob: blob}
	if t.Public {
		if t.PowBits > MaxPowBits {
			return 0, ErrBadRequest
		}
		pow, err := minePow(ctx, t.Queue, blob, t.PowBits)
		if err != nil {
			return 0, err
		}
		dep.Pow = pow
	}
	rnd := s.Rand
	if rnd == nil {
		rnd = rand.Reader
	}
	eph := make([]byte, 32)
	if _, err := io.ReadFull(rnd, eph); err != nil {
		return 0, err
	}
	sealed, err := SealForward(eph, t.HS, dep.Append(nil))
	zero(eph)
	if err != nil {
		return 0, err
	}
	conn, err := s.Dial.Dial(ctx, via)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	d, err := conn.Forward(ctx, Forward{Req: 1, Relay: t.Relay[:], RelayCard: t.RelayCard, Sealed: sealed})
	if err != nil {
		return 0, err
	}
	return d.Status, nil
}

// minePow finds a proof of work of bits leading zero bits for a deposit (services.md 5.2). The cost is
// about 2^bits hashes; ctx is looked at every few thousand.
func minePow(ctx context.Context, queue, blob []byte, bits uint32) (uint64, error) {
	for pow := uint64(0); ; pow++ {
		if pow&0xfff == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if ValidPow(queue, blob, pow, bits) {
			return pow, nil
		}
	}
}

// ---- the recipient ----------------------------------------------------------------------------

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
	// Limited says the node is in background=limited mode: no long-poll.
	Limited func() bool
	// Wait is the long-poll time of a FETCH; zero means 30 s.
	Wait time.Duration
}

const (
	queuePrefix = "q:"
	// rotateMin and rotateSpan give uniform(20, 28) hours (services.md 5.2).
	rotateMin  = 20 * time.Hour
	rotateSpan = 8 * time.Hour
	// retireAfter keeps a replaced queue read for 7 days (services.md 5.2).
	retireAfter = 7 * 24 * time.Hour
	// queueDays is the life asked of a queue: a rotation period plus the 7 days of the old queue.
	queueDays = 9
	fetchPage = 100
)

// queueState is one stored queue of the recipient.
type queueState struct {
	Relay    [32]byte `json:"relay"`
	Queue    [16]byte `json:"queue"`
	Seed     [32]byte `json:"seed"` // Ed25519 seed of the queue's auth key
	Created  int64    `json:"created"`
	Expires  int64    `json:"expires"`
	RotateAt int64    `json:"rotate_at"`
	Old      bool     `json:"old,omitempty"`
	RetireAt int64    `json:"retire_at,omitempty"`
}

// Receiver owns the recipient's queues: it creates them, rotates them, fetches and acknowledges.
type Receiver struct {
	cfg ReceiverConfig
	rnd io.Reader

	mu     sync.Mutex // one Tick at a time
	queues map[[16]byte]*queueState
	req    uint64
	held   map[string]time.Time // items the handler did not take, by queue and id: first seen
}

// NewReceiver builds a Receiver over its stored queues.
func NewReceiver(cfg ReceiverConfig) (*Receiver, error) {
	if cfg.Store == nil || cfg.Dial == nil || cfg.Relays == nil || cfg.Now == nil || cfg.Handle == nil {
		return nil, fmt.Errorf("%w: Store, Dial, Relays, Now and Handle are required", ErrBadRequest)
	}
	r := &Receiver{cfg: cfg, rnd: cfg.Rand, queues: map[[16]byte]*queueState{}}
	if r.rnd == nil {
		r.rnd = rand.Reader
	}
	var after []byte
	for {
		var (
			page []store.Entry
			err  error
		)
		if after == nil {
			page, err = cfg.Store.Scan([]byte(queuePrefix), store.MaxScanLimit)
		} else {
			page, err = cfg.Store.ScanFrom([]byte(queuePrefix), after, store.MaxScanLimit)
		}
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			var q queueState
			if json.Unmarshal(e.Value, &q) == nil {
				r.queues[q.Queue] = &q
			}
		}
		if len(page) < store.MaxScanLimit {
			break
		}
		after = page[len(page)-1].Key
	}
	return r, nil
}

func queueKeyOf(q [16]byte) []byte { return []byte(queuePrefix + hex.EncodeToString(q[:])) }

func (r *Receiver) save(q *queueState) error {
	raw, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return r.cfg.Store.Put(queueKeyOf(q.Queue), raw)
}

// current returns the current (not replaced) queue, if any and not expired.
func (r *Receiver) current(now time.Time) *queueState {
	for _, q := range r.queues {
		if !q.Old && now.Unix() < q.Expires {
			return q
		}
	}
	return nil
}

// Queues returns the current queue of each relay in use.
func (r *Receiver) Queues() []QueueRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []QueueRef
	if q := r.current(r.cfg.Now()); q != nil {
		out = append(out, QueueRef{Relay: q.Relay, Queue: q.Queue, Expires: time.Unix(q.Expires, 0)})
	}
	return out
}

// Tick creates the first queue or the next one when it is due, retires queues replaced 7 days ago, then
// fetches every queue once. Errors are joined; a queue that could not be reached stays.
func (r *Receiver) Tick(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.cfg.Now()
	var errs []error

	// a queue that ended on the relay (the node was off for longer than it lives) is forgotten
	for id, q := range r.queues {
		if now.Unix() >= q.Expires {
			delete(r.queues, id)
			_ = r.cfg.Store.Delete(queueKeyOf(id))
		}
	}
	cur := r.current(now)
	if cur == nil || now.Unix() >= cur.RotateAt {
		if err := r.rotate(ctx, cur, now); err != nil {
			errs = append(errs, err)
		}
	}
	for id, q := range r.queues {
		if q.Old && now.Unix() >= q.RetireAt {
			delete(r.queues, id)
			_ = r.cfg.Store.Delete(queueKeyOf(id))
		}
	}
	conns := map[[32]byte]Conn{}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	cur = r.current(now)
	for _, q := range r.queues {
		conn, ok := conns[q.Relay]
		if !ok {
			var err error
			if conn, err = r.cfg.Dial.Dial(ctx, q.Relay); err != nil {
				errs = append(errs, fmt.Errorf("mailbox: relay %x: %w", q.Relay[:4], err))
				continue
			}
			conns[q.Relay] = conn
		}
		if err := r.poll(ctx, conn, q, q == cur); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rotate makes a new current queue on the preferred relay and replaces cur.
func (r *Receiver) rotate(ctx context.Context, cur *queueState, now time.Time) error {
	relays := r.cfg.Relays()
	if len(relays) == 0 {
		return nil
	}
	relay := relays[0]
	var q queueState
	q.Relay = relay
	if _, err := io.ReadFull(r.rnd, q.Queue[:]); err != nil {
		return err
	}
	if _, err := io.ReadFull(r.rnd, q.Seed[:]); err != nil {
		return err
	}
	var span [8]byte
	if _, err := io.ReadFull(r.rnd, span[:]); err != nil {
		return err
	}
	q.RotateAt = now.Add(rotateMin + time.Duration(binary.BigEndian.Uint64(span[:])%uint64(rotateSpan))).Unix()
	signer, err := keys.NewEd25519Signer(q.Seed[:])
	if err != nil {
		return err
	}
	conn, err := r.cfg.Dial.Dial(ctx, relay)
	if err != nil {
		return fmt.Errorf("mailbox: relay %x: %w", relay[:4], err)
	}
	defer conn.Close()
	r.req++
	expires, err := conn.CreateQueue(ctx, QueueCreate{Req: r.req, Queue: q.Queue[:], AuthKey: signer.Public().Key(), TTL: queueDays})
	if err != nil {
		return fmt.Errorf("mailbox: creating a queue: %w", err)
	}
	q.Created, q.Expires = now.Unix(), int64(expires)
	if err := r.save(&q); err != nil {
		return err
	}
	r.queues[q.Queue] = &q
	if cur != nil {
		cur.Old, cur.RetireAt = true, now.Add(retireAfter).Unix()
		_ = r.save(cur)
	}
	if r.cfg.Announce != nil {
		r.cfg.Announce(QueueRef{Relay: q.Relay, Queue: q.Queue, Expires: time.Unix(q.Expires, 0)})
	}
	return nil
}

func (r *Receiver) wait(current bool) uint32 {
	if !current || (r.cfg.Limited != nil && r.cfg.Limited()) {
		return 0
	}
	w := r.cfg.Wait
	if w == 0 {
		w = 30 * time.Second
	}
	if s := uint32(w / time.Second); s > 0 {
		return s
	}
	return 1
}

// poll fetches a queue and acknowledges what the handler took, page by page.
func (r *Receiver) poll(ctx context.Context, conn Conn, q *queueState, current bool) error {
	signer, err := keys.NewEd25519Signer(q.Seed[:])
	if err != nil {
		return err
	}
	wait := r.wait(current)
	var after []byte
	for page := 0; page < maxPollPages; page++ {
		r.req++
		req := r.req
		items, err := conn.Fetch(ctx, Fetch{Req: req, Queue: q.Queue[:], After: after, Max: fetchPage, Wait: wait,
			Auth: signer.Sign(AuthInput(OpFetch, conn.SID(), q.Queue[:], req))})
		if err != nil {
			return fmt.Errorf("mailbox: fetch: %w", err)
		}
		wait = 0 // only the first request of a poll waits
		var ids [][]byte
		for _, it := range items.Items {
			if r.cfg.Handle(ctx, it.Blob) || r.giveUp(q.Queue, it.ID) {
				ids = append(ids, it.ID)
				r.forget(q.Queue, it.ID)
			}
		}
		if len(ids) > 0 {
			r.req++
			req = r.req
			if err := conn.Ack(ctx, Ack{Req: req, Queue: q.Queue[:], IDs: ids,
				Auth: signer.Sign(AuthInput(OpAck, conn.SID(), q.Queue[:], req))}); err != nil {
				return fmt.Errorf("mailbox: ack: %w", err)
			}
		}
		// the next page starts after the last item of this one, whether or not it was taken: items the
		// handler left must not hide the ones behind them
		if !items.More || len(items.Items) == 0 {
			return nil
		}
		after = items.Items[len(items.Items)-1].ID
	}
	return nil
}

const (
	// maxPollPages bounds one poll: a queue holds at most 500 items, 100 to a page.
	maxPollPages = 10
	// holdFor is how long an item the handler does not take is kept in the queue; after it the item is
	// acknowledged and lost, so that items nobody can handle (a sender that cannot be looked up) block
	// nothing for long.
	holdFor = time.Hour
	// maxHeld bounds the table of such items; past it an item is given up at once.
	maxHeld = 4096
)

// giveUp notes that the handler did not take an item and says whether it has waited long enough.
func (r *Receiver) giveUp(queue [16]byte, id []byte) bool {
	now := r.cfg.Now()
	key := string(queue[:]) + string(id)
	if r.held == nil {
		r.held = map[string]time.Time{}
	}
	first, ok := r.held[key]
	if !ok {
		if len(r.held) >= maxHeld {
			return true
		}
		r.held[key] = now
		return false
	}
	return now.Sub(first) >= holdFor
}

func (r *Receiver) forget(queue [16]byte, id []byte) { delete(r.held, string(queue[:])+string(id)) }
