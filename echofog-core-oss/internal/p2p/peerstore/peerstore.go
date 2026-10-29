package peerstore

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// savedKey: the peer ID in the key is blinded, the database does not list whom we met.
func (ps *PeerStore) savedKey(id types.PeerID) []byte {
	return []byte(savedPeersPrefix + storage.BlindID(ps.storage, hex.EncodeToString(id[:])))
}

const (
	savedPeersPrefix = "saved:peers:"
	bansPrefix       = "bans:peer:"
)

const (
	// MaxDialFailures in a row make a peer forgotten: it moved or is gone.
	MaxDialFailures = 8
	// ForgetAfter is how long a peer is kept without a successful connection.
	ForgetAfter = 30 * 24 * time.Hour
	// maxScan bounds the entries read to sort the history or prune it.
	maxScan = 5000
	// MaxStored is the most entries kept before peers only heard of (SaveDiscovered) are refused.
	MaxStored = 2000

	retryBase = 15 * time.Second
	retryMax  = 6 * time.Hour
)

// PeerStore handles persistence of known peers, reputation/trust scores, and ban lists.
// It isolates storage I/O and banning logic away from the core Swarm peer-routing registry.
type PeerStore struct {
	storage storage.Storage
	logger  *slog.Logger

	bansMu sync.RWMutex
	banned map[types.PeerID]time.Time // until when; the zero time is for good
	// bansAll: every stored ban is in banned, so a peer missing there is not banned; false when
	// there were more than loadBans reads, and storage is asked
	bansAll bool

	entryMu sync.Mutex // read-modify-write of entries (failures, discovered peers)
	stored  int        // entries in storage as of countAt, plus discovered ones written since
	countAt time.Time

	// asyncMu orders Add on wg against Close: a WaitGroup must not gain a job while Wait runs
	asyncMu sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewPeerStore(store storage.Storage, logger *slog.Logger) *PeerStore {
	ctx, cancel := context.WithCancel(context.Background())
	ps := &PeerStore{
		storage: store,
		logger:  logger,
		banned:  make(map[types.PeerID]time.Time),
		ctx:     ctx,
		cancel:  cancel,
	}

	// Warm up memory ban cache
	ps.loadBans()
	return ps
}

const (
	maxLoadedBans = 1000
	maxCachedBans = 20000
)

func (ps *PeerStore) loadBans() {
	if ps.storage == nil {
		ps.bansAll = true
		return
	}
	values, err := ps.storage.FindValues([]byte(bansPrefix), maxLoadedBans)
	if err != nil {
		return
	}
	ps.bansMu.Lock()
	defer ps.bansMu.Unlock()
	now := time.Now()
	for _, v := range values {
		if len(v.Key) > len(bansPrefix) {
			pBytes := v.Key[len(bansPrefix):]
			if pID, err := types.ToPeerID(pBytes); err == nil {
				until := banUntil(v.Value)
				if !until.IsZero() && now.After(until) {
					_ = ps.storage.Delete(v.Key) // served its time
					continue
				}
				ps.banned[pID] = until
			}
		}
	}
	ps.bansAll = len(values) < maxLoadedBans
}

// banUntil reads a stored ban: "true" (older versions) is for good, otherwise the end in unix
// nanoseconds.
func banUntil(v []byte) time.Time {
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// SavePeer persists or updates a peer's last known address and trust score.
func (ps *PeerStore) SavePeer(pubKey types.PeerPublicKey, addr string, trustScore uint32) error {
	if ps.storage == nil {
		return nil
	}
	ps.entryMu.Lock()
	defer ps.entryMu.Unlock()

	data := &storage.PeerStoreEntry{
		PubKey:        pubKey[:],
		LastKnownAddr: addr,
		LastSeen:      uint64(time.Now().UnixNano()),
		TrustScore:    trustScore,
	}
	return ps.writeEntry(data)
}

func (ps *PeerStore) writeEntry(data *storage.PeerStoreEntry) error {
	pubKey, err := types.ToPeerPubKey(data.PubKey)
	if err != nil {
		return err
	}
	protoData, err := proto.Marshal(data)
	if err != nil {
		if ps.logger != nil {
			ps.logger.Error("PeerStore: failed to marshal peer entry", "err", err)
		}
		return fmt.Errorf("marshal peer entry: %w", err)
	}

	peerID := types.PeerPubKeyToID(pubKey)
	key := ps.savedKey(peerID)

	if err := ps.storage.Set(key, protoData); err != nil {
		if ps.logger != nil {
			ps.logger.Error("PeerStore: failed to write peer entry", "err", err, "peer", peerID.String()[:8])
		}
		return fmt.Errorf("storage set: %w", err)
	}

	return nil
}

// SavePeerAsync safely writes peer metadata in the background tracking active goroutines.
func (ps *PeerStore) SavePeerAsync(pubKey types.PeerPublicKey, addr string, trustScore uint32) {
	ps.asyncMu.Lock()
	if ps.closed {
		ps.asyncMu.Unlock()
		return // closing: no new writes
	}
	ps.wg.Add(1)
	ps.asyncMu.Unlock()
	go func() {
		defer ps.wg.Done()
		select {
		case <-ps.ctx.Done():
			return
		default:
			_ = ps.SavePeer(pubKey, addr, trustScore)
		}
	}()
}

// GetPeer retrieves an entry for a specific peer ID.
func (ps *PeerStore) GetPeer(peerID types.PeerID) (*storage.PeerStoreEntry, error) {
	if ps.storage == nil {
		return nil, fmt.Errorf("storage not initialized")
	}

	key := ps.savedKey(peerID)
	data, err := ps.storage.Get(key)
	if err != nil {
		return nil, err
	}

	var entry storage.PeerStoreEntry
	if err := proto.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("unmarshal peer entry: %w", err)
	}
	return &entry, nil
}

// SaveDiscovered keeps a peer another node told us about, for dialing it later. A peer already
// known keeps its entry: what somebody else says must not replace an address we connected to,
// nor make a peer gone for weeks look fresh. Returns whether the peer was new.
func (ps *PeerStore) SaveDiscovered(pubKey types.PeerPublicKey, addr string) bool {
	if ps.storage == nil || addr == "" {
		return false
	}
	ps.entryMu.Lock()
	defer ps.entryMu.Unlock()
	if _, err := ps.storage.Get(ps.savedKey(types.PeerPubKeyToID(pubKey))); err == nil {
		return false
	}
	// what others tell us must not fill the disk: past MaxStored entries only peers we connected
	// to are kept (SavePeer), until Prune makes room
	now := time.Now()
	if ps.countAt.IsZero() || now.Sub(ps.countAt) > time.Minute {
		ps.stored, ps.countAt = len(ps.allEntries()), now
	}
	if ps.stored >= MaxStored {
		return false
	}
	ok := ps.writeEntry(&storage.PeerStoreEntry{
		PubKey:        pubKey[:],
		LastKnownAddr: addr,
		LastSeen:      uint64(now.UnixNano()),
		TrustScore:    50,
	}) == nil
	if ok {
		ps.stored++
	}
	return ok
}

// RecordDialFailure notes that a peer could not be reached at its address. After
// MaxDialFailures in a row it is forgotten; a successful connection (SavePeer) clears the count.
func (ps *PeerStore) RecordDialFailure(peerID types.PeerID) {
	if ps.storage == nil {
		return
	}
	ps.entryMu.Lock()
	defer ps.entryMu.Unlock()
	key := ps.savedKey(peerID)
	raw, err := ps.storage.Get(key)
	if err != nil {
		return
	}
	var e storage.PeerStoreEntry
	if proto.Unmarshal(raw, &e) != nil {
		_ = ps.storage.Delete(key)
		return
	}
	e.FailCount++
	e.LastFailed = uint64(time.Now().UnixNano())
	if e.FailCount >= MaxDialFailures {
		_ = ps.storage.Delete(key)
		return
	}
	_ = ps.writeEntry(&e)
}

// DialDue reports whether a peer that failed before is due for another try: the wait doubles
// with each failure in a row, from 15 seconds up to 6 hours.
func DialDue(e *storage.PeerStoreEntry, now time.Time) bool {
	if e.GetFailCount() == 0 {
		return true
	}
	wait := retryMax
	if e.FailCount < 20 {
		if w := retryBase << (e.FailCount - 1); w < retryMax {
			wait = w
		}
	}
	return now.Sub(time.Unix(0, int64(e.GetLastFailed()))) >= wait
}

func (ps *PeerStore) allEntries() []*storage.PeerStoreEntry {
	findValues, err := ps.storage.FindValues([]byte(savedPeersPrefix), maxScan)
	if err != nil {
		return nil
	}
	peers := make([]*storage.PeerStoreEntry, 0, len(findValues))
	for _, v := range findValues {
		var peer storage.PeerStoreEntry
		if err := proto.Unmarshal(v.Value, &peer); err != nil || len(peer.PubKey) != len(types.PeerPublicKey{}) {
			continue
		}
		peers = append(peers, &peer)
	}
	return peers
}

// GetHistoryConnected returns the known peers, most recently seen first, up to count (0 = up
// to 100). The database orders them by blinded key, which says nothing about how fresh they are.
func (ps *PeerStore) GetHistoryConnected(count uint) []*storage.PeerStoreEntry {
	if ps.storage == nil {
		return nil
	}
	limit := 100
	if count > 0 {
		limit = int(count)
	}
	peers := ps.allEntries()
	sort.SliceStable(peers, func(i, j int) bool { return peers[i].LastSeen > peers[j].LastSeen })
	if len(peers) > limit {
		peers = peers[:limit]
	}
	return peers
}

// Prune forgets peers not seen for ForgetAfter and those that failed MaxDialFailures times in a
// row; returns how many it removed.
func (ps *PeerStore) Prune(now time.Time) int {
	if ps.storage == nil {
		return 0
	}
	ps.entryMu.Lock()
	defer ps.entryMu.Unlock()
	ps.countAt = time.Time{} // recount after this
	cutoff := uint64(now.Add(-ForgetAfter).UnixNano())
	removed := 0
	for _, e := range ps.allEntries() {
		if e.LastSeen >= cutoff && e.FailCount < MaxDialFailures {
			continue
		}
		pub, err := types.ToPeerPubKey(e.PubKey)
		if err != nil {
			continue
		}
		if ps.storage.Delete(ps.savedKey(types.PeerPubKeyToID(pub))) == nil {
			removed++
		}
	}
	return removed
}

// BanPeer bans a peer for good, in memory and in storage.
func (ps *PeerStore) BanPeer(peerID types.PeerID) error { return ps.BanPeerFor(peerID, 0) }

// BanPeerFor bans a peer for d (0: for good); the ban survives a restart. A longer ban already in
// place is kept.
func (ps *PeerStore) BanPeerFor(peerID types.PeerID, d time.Duration) error {
	var until time.Time
	if d > 0 {
		until = time.Now().Add(d)
	}
	ps.bansMu.Lock()
	if have, ok := ps.banned[peerID]; ok && (have.IsZero() || (!until.IsZero() && have.After(until))) {
		ps.bansMu.Unlock()
		return nil
	}
	ps.banned[peerID] = until
	if len(ps.banned) > maxCachedBans {
		now := time.Now()
		for id, u := range ps.banned {
			if !u.IsZero() && now.After(u) {
				delete(ps.banned, id) // storage keeps it until it is next read, then drops it
			}
		}
	}
	ps.bansMu.Unlock()

	if ps.storage != nil {
		key := append([]byte(bansPrefix), peerID[:]...)
		val := []byte("true")
		if !until.IsZero() {
			val = []byte(strconv.FormatInt(until.UnixNano(), 10))
		}
		return ps.storage.Set(key, val)
	}
	return nil
}

// UnBanPeer removes a peer from the ban list.
func (ps *PeerStore) UnBanPeer(peerID types.PeerID) error {
	ps.bansMu.Lock()
	delete(ps.banned, peerID)
	ps.bansMu.Unlock()

	if ps.storage != nil {
		key := append([]byte(bansPrefix), peerID[:]...)
		return ps.storage.Delete(key)
	}
	return nil
}

// IsBanned checks whether a peer is currently banned. A ban that ran out is lifted here.
func (ps *PeerStore) IsBanned(peerID types.PeerID) bool {
	ps.bansMu.RLock()
	until, ok := ps.banned[peerID]
	all := ps.bansAll
	ps.bansMu.RUnlock()
	if !ok && !all && ps.storage != nil {
		// not cached: peer IDs are free to make, so unknown ones are looked up, never remembered
		key := append([]byte(bansPrefix), peerID[:]...)
		v, err := ps.storage.Get(key)
		if err != nil {
			return false
		}
		until, ok = banUntil(v), true
		ps.bansMu.Lock()
		ps.banned[peerID] = until
		ps.bansMu.Unlock()
	}
	if !ok {
		return false
	}
	if !until.IsZero() && time.Now().After(until) {
		_ = ps.UnBanPeer(peerID)
		return false
	}
	return true
}

// Close cancels pending async jobs and waits for in-flight writes to finish.
func (ps *PeerStore) Close() error {
	ps.asyncMu.Lock()
	ps.closed = true
	ps.asyncMu.Unlock()
	ps.cancel()
	ps.wg.Wait()
	return nil
}
