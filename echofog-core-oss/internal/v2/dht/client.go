package dht

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Limits of the lookup client (dht.md §4, services.md §2).
const (
	// MaxQueries is the query budget of one path (dht.md §4.1: "64 queries have been sent"). A
	// lookup over three paths therefore sends at most 3 × 64.
	MaxQueries = 64
	// Paths is the number of disjoint paths of a value lookup (dht.md §4.2).
	Paths = 3
	// RequestTimeout is the per-request timeout of services.md §2: the time from sending a
	// request to its answer on an open stream.
	RequestTimeout = 2 * time.Second
	// QueryTimeout bounds one query including opening the stream, which may dial and
	// handshake first.
	QueryTimeout = 12 * time.Second
	// maxRecordBytes is the largest record a node accepts or a VALUE carries (STORE maximum).
	maxRecordBytes = 24576
)

// Errors of the lookup client.
var (
	// ErrNotFound means a majority of the answering closest nodes on every path said that the
	// record does not exist (dht.md §4.2).
	ErrNotFound = errors.New("dht: not found")
	// ErrInconclusive means the lookup could not tell: too few nodes answered on some path.
	ErrInconclusive = errors.New("dht: lookup inconclusive")
	// ErrRollback means the best record found is older than one this reader has accepted.
	ErrRollback = errors.New("dht: older than a record seen before")
	// ErrNotStored means no node stored the record.
	ErrNotStored = errors.New("dht: record stored on no node")
	// ErrNotStorable means the DHT never stores this record (name bindings, foreign labels).
	ErrNotStorable = errors.New("dht: record is not storable")
)

// RemoteError is an ERROR frame of a peer (services.md §2: 0x100 bad-record, 0x101 wrong-key).
type RemoteError struct{ Code uint32 }

func (e *RemoteError) Error() string { return fmt.Sprintf("dht: remote error 0x%x", e.Code) }

// Transport sends the dht/2 requests of one client. Results are raw: the client verifies every
// card and record itself, and an answer that does not verify counts against the node.
type Transport interface {
	// FindNode asks one node for the cards closest to target.
	FindNode(ctx context.Context, to *objects.Card, target [32]byte) (cards [][]byte, err error)
	// FindValue asks one node for the record under key, or for closer cards.
	FindValue(ctx context.Context, to *objects.Card, key [32]byte) (record []byte, cards [][]byte, err error)
	// Store asks one node to store a record and returns the STORED status.
	Store(ctx context.Context, to *objects.Card, record []byte) (status uint32, err error)
}

// ClientConfig wires a Client.
type ClientConfig struct {
	// Self is the local device ID; it never becomes a candidate. Required.
	Self DeviceID
	// Table supplies the starting nodes and learns the cards the lookups find. Required.
	Table *Table
	// Transport sends requests. Required.
	Transport Transport
	// Memory is the persistent namespace of the reader memory (AD13); nil keeps it in memory
	// only, which does not survive a restart.
	Memory store.KV
	// Now is the corrected clock (AD13). Nil is the system clock.
	Now func() time.Time
	// Rand is the jitter source. Nil seeds one from the clock.
	Rand *rand.Rand
	// Learn is called for every verified card a node sent; nil adds it to Table with AddFromCard.
	Learn func(*objects.Card)
	// OnQuery, when set, is told which path sends which query (tests, diagnostics).
	OnQuery func(path int, to DeviceID)
	// Own returns the records this node publishes by itself; Step registers them again each time,
	// so a new card (a higher seq) replaces the old one. Nil means none.
	Own func() [][]byte
	// Log receives diagnostics. Nil is silent.
	Log *slog.Logger
}

// Client is the lookup and publishing side of dht/2 (T5.2): iterative node lookup, value lookup
// over three disjoint paths with the reader memory, STORE to the closest replicas, and the
// republication of own records.
type Client struct {
	cfg ClientConfig
	now func() time.Time
	mem *Memory
	log *slog.Logger

	rndMu sync.Mutex
	rnd   *rand.Rand

	pubMu sync.Mutex
	pubs  map[string]*published
}

// NewClient builds a client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Table == nil || cfg.Transport == nil {
		return nil, fmt.Errorf("%w: table and transport are required", ErrConfig)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	rnd := cfg.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if cfg.Learn == nil {
		tbl := cfg.Table
		cfg.Learn = func(c *objects.Card) { tbl.AddFromCard(c) }
	}
	return &Client{
		cfg:  cfg,
		now:  now,
		mem:  NewMemory(cfg.Memory),
		log:  log,
		rnd:  rnd,
		pubs: map[string]*published{},
	}, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func (c *Client) random() float64 {
	c.rndMu.Lock()
	defer c.rndMu.Unlock()
	return c.rnd.Float64()
}

// Lookup is the LookupFunc of the routing table: an iterative node lookup whose answers feed the
// table (the start-up self-lookup and the bucket refreshes of dht.md §5).
func (c *Client) Lookup(ctx context.Context, target [32]byte) error {
	_, err := c.FindNode(ctx, target)
	return err
}

// FindNode is the iterative node lookup of dht.md §4.1: it returns the cards of the closest nodes
// that answered, closest first, at most one per diversity unit and two per /16.
func (c *Client) FindNode(ctx context.Context, target [32]byte) ([]*objects.Card, error) {
	lk := c.newLookup(target, 1, false, nil)
	lk.run(ctx)
	out := lk.paths[0].answeredClosest()
	if len(out) == 0 {
		return nil, ErrInconclusive
	}
	return out, nil
}

// Get looks up the record of (label, signer) on the keys of the current day and, in the first
// 2 hours of a day, of the previous one (dht.md §2), over three disjoint paths (§4.2). It returns
// the valid record with the highest seq, or ErrNotFound only when a majority of the answering
// closest nodes on every path said so, or ErrRollback when the best record is older than one the
// reader has accepted before.
func (c *Client) Get(ctx context.Context, label string, signer [32]byte) (*objects.Envelope, error) {
	return c.get(ctx, label, signer, func(day uint32) [32]byte { return objects.DHTKey(label, signer, day) })
}

// GetRelease looks up the release manifest of the day (any signer: manifests of rotated release
// keys share the key, objects.md §9). The caller checks the threshold signatures.
func (c *Client) GetRelease(ctx context.Context, valid func(raw []byte, now time.Time) error) (*objects.Envelope, error) {
	return c.get(ctx, objects.LabelRelease, [32]byte{}, objects.ReleaseDHTKey)
}

func (c *Client) get(ctx context.Context, label string, signer [32]byte, keyOf func(day uint32) [32]byte) (*objects.Envelope, error) {
	now := c.now()
	days := readDays(now)
	accept := func(raw []byte) (*objects.Envelope, error) { return acceptRecord(label, signer, raw, now, nil) }

	lookups := make([]*lookup, len(days))
	var wg sync.WaitGroup
	for i, d := range days {
		lookups[i] = c.newLookup(keyOf(d), Paths, true, accept)
		wg.Add(1)
		go func(lk *lookup) {
			defer wg.Done()
			lk.run(ctx)
		}(lookups[i])
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var best *objects.Envelope
	concluded := true
	for _, lk := range lookups {
		if lk.best != nil && better(lk.best, best) {
			best = lk.best
		}
		if !lk.notFound() {
			concluded = false
		}
	}
	if best != nil {
		if err := c.mem.Accept(label, best.SignerID(), best.Seq); err != nil {
			return nil, err
		}
		return best, nil
	}
	if concluded {
		return nil, ErrNotFound
	}
	return nil, ErrInconclusive
}

// better reports whether a beats b: the higher seq, then the smaller object ID (dht.md §4.2).
func better(a, b *objects.Envelope) bool {
	if b == nil {
		return true
	}
	if a.Seq != b.Seq {
		return a.Seq > b.Seq
	}
	return objects.Prefer(a, b) == a && !equalRaw(a, b)
}

func equalRaw(a, b *objects.Envelope) bool { return string(a.Raw) == string(b.Raw) }

// readDays are the days a reader looks under: the current one, and during its first 2 hours the
// previous one (dht.md §2).
func readDays(now time.Time) []uint32 {
	d := objects.DHTDay(now)
	days := []uint32{d}
	if now.Unix()%86400 < 2*3600 && d > 0 {
		days = append(days, d-1)
	}
	return days
}

// writeDays are the days a publisher stores under: the current one, and during the last 2 hours
// of a day the next one too (dht.md §2).
func writeDays(now time.Time) []uint32 {
	d := objects.DHTDay(now)
	days := []uint32{d}
	if now.Unix()%86400 >= 86400-2*3600 {
		days = append(days, d+1)
	}
	return days
}

// Replicas is the number of replicas of a label (registries.md §9).
func Replicas(label string) int {
	switch label {
	case objects.LabelCard, objects.LabelPrekeys, objects.LabelDevices:
		return 8
	case objects.LabelMailboxHint:
		return 5
	case objects.LabelRelease:
		return 20
	}
	return 5
}

// RequiresDisjointPaths reports whether a value lookup of the label needs three disjoint paths
// (dht.md §4.2); for the other labels the paths are only recommended, and the client uses them
// anyway.
func RequiresDisjointPaths(label string) bool {
	switch label {
	case objects.LabelPrekeys, objects.LabelCard, objects.LabelMailboxHint, objects.LabelRelease:
		return true
	}
	return false
}

// PutResult counts the STORED statuses of one Put (services.md §2).
type PutResult struct {
	Stored, Stale, Refused, Quota, Invalid, Failed int
}

// Put stores a record on the closest replicas of its key: for each day key of the current time
// (dht.md §2), a lookup finds the closest nodes, of which the label's replica count are chosen
// with at most one per diversity unit (§3). A node that is a replica of two keys gets one STORE:
// the storing node derives the keys itself. ErrNotStored is returned when no node stored it.
func (c *Client) Put(ctx context.Context, record []byte) (PutResult, error) {
	now := c.now()
	env, err := objects.Verify(record, now)
	if err != nil {
		return PutResult{}, fmt.Errorf("%w: %v", ErrNotStorable, err)
	}
	if !storable(env.Label) {
		return PutResult{}, ErrNotStorable
	}

	chosen := map[DeviceID]*objects.Card{}
	var order []DeviceID
	for _, d := range writeDays(now) {
		key := objects.DHTKey(env.Label, env.SignerID(), d)
		if env.Label == objects.LabelRelease {
			key = objects.ReleaseDHTKey(d)
		}
		closest, err := c.FindNode(ctx, key)
		if err != nil {
			if ctx.Err() != nil {
				return PutResult{}, ctx.Err()
			}
			continue
		}
		for _, card := range pickReplicas(closest, Replicas(env.Label)) {
			id := DeviceID(card.DeviceID)
			if chosen[id] == nil {
				chosen[id] = card
				order = append(order, id)
			}
		}
	}

	var res PutResult
	var mu sync.Mutex
	sem := make(chan struct{}, Alpha)
	var wg sync.WaitGroup
	for _, id := range order {
		card := chosen[id]
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			qctx, cancel := context.WithTimeout(ctx, QueryTimeout)
			status, err := c.cfg.Transport.Store(qctx, card, record)
			cancel()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Failed++
				return
			}
			switch status {
			case StatusStored:
				res.Stored++
			case StatusStale:
				res.Stale++
			case StatusRefused:
				res.Refused++
			case StatusQuota:
				res.Quota++
			default:
				res.Invalid++
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if res.Stored == 0 {
		return res, ErrNotStored
	}
	return res, nil
}

func storable(label string) bool {
	return label != objects.LabelName && len(label) > len("echofog/") && label[:len("echofog/")] == "echofog/"
}

// pickReplicas takes the first n cards of a distance-ordered list with at most one per diversity
// unit (dht.md §3).
func pickReplicas(closest []*objects.Card, n int) []*objects.Card {
	var out []*objects.Card
	used := map[string]bool{}
	for _, card := range closest {
		units, _ := unitsOf(card)
		clash := false
		for _, u := range units {
			if used[u] {
				clash = true
			}
		}
		if clash {
			continue
		}
		for _, u := range units {
			used[u] = true
		}
		out = append(out, card)
		if len(out) == n {
			break
		}
	}
	return out
}
