package peerstore

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
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

// PeerStore handles persistence of known peers, reputation/trust scores, and ban lists.
// It isolates storage I/O and banning logic away from the core Swarm peer-routing registry.
type PeerStore struct {
	storage storage.Storage
	logger  *slog.Logger

	bansMu sync.RWMutex
	banned map[types.PeerID]bool

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

func NewPeerStore(store storage.Storage, logger *slog.Logger) *PeerStore {
	ctx, cancel := context.WithCancel(context.Background())
	ps := &PeerStore{
		storage: store,
		logger:  logger,
		banned:  make(map[types.PeerID]bool),
		ctx:     ctx,
		cancel:  cancel,
	}

	// Warm up memory ban cache
	ps.loadBans()
	return ps
}

func (ps *PeerStore) loadBans() {
	if ps.storage == nil {
		return
	}
	values, err := ps.storage.FindValues([]byte(bansPrefix), 1000)
	if err != nil {
		return
	}
	ps.bansMu.Lock()
	defer ps.bansMu.Unlock()
	for _, v := range values {
		if len(v.Key) > len(bansPrefix) {
			pBytes := v.Key[len(bansPrefix):]
			if pID, err := types.ToPeerID(pBytes); err == nil {
				ps.banned[pID] = true
			}
		}
	}
}

// SavePeer persists or updates a peer's last known address and trust score.
func (ps *PeerStore) SavePeer(pubKey types.PeerPublicKey, addr string, trustScore uint32) error {
	if ps.storage == nil {
		return nil
	}

	data := &storage.PeerStoreEntry{
		PubKey:        pubKey[:],
		LastKnownAddr: addr,
		LastSeen:      uint64(time.Now().UnixNano()),
		TrustScore:    trustScore,
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
	ps.wg.Add(1)
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

// GetHistoryConnected returns historical peers up to count (0 = all up to 100).
func (ps *PeerStore) GetHistoryConnected(count uint) []*storage.PeerStoreEntry {
	if ps.storage == nil {
		return nil
	}

	limit := 100
	if count > 0 && int(count) < limit {
		limit = int(count)
	}

	findValues, err := ps.storage.FindValues([]byte(savedPeersPrefix), limit)
	if err != nil {
		return nil
	}

	var peers []*storage.PeerStoreEntry
	for _, v := range findValues {
		var peer storage.PeerStoreEntry
		if err := proto.Unmarshal(v.Value, &peer); err != nil {
			continue
		}
		peers = append(peers, &peer)
		if count > 0 && uint(len(peers)) >= count {
			break
		}
	}
	return peers
}

// BanPeer records a ban flag in both the fast in-memory set and storage.
func (ps *PeerStore) BanPeer(peerID types.PeerID) error {
	ps.bansMu.Lock()
	ps.banned[peerID] = true
	ps.bansMu.Unlock()

	if ps.storage != nil {
		key := append([]byte(bansPrefix), peerID[:]...)
		return ps.storage.Set(key, []byte("true"))
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

// IsBanned checks whether a peer is currently banned.
func (ps *PeerStore) IsBanned(peerID types.PeerID) bool {
	ps.bansMu.RLock()
	banned, ok := ps.banned[peerID]
	ps.bansMu.RUnlock()
	if ok {
		return banned
	}

	if ps.storage != nil {
		key := append([]byte(bansPrefix), peerID[:]...)
		_, err := ps.storage.Get(key)
		isBan := (err == nil)
		ps.bansMu.Lock()
		ps.banned[peerID] = isBan
		ps.bansMu.Unlock()
		return isBan
	}
	return false
}

// Close cancels pending async jobs and waits for in-flight writes to finish.
func (ps *PeerStore) Close() error {
	ps.cancel()
	ps.wg.Wait()
	return nil
}
