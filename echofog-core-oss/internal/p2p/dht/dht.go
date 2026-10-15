package dht

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/pkg/events"

	"github.com/echofog/echofog-core/internal/cache"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

const Alpha = 3

// maxLookupQueries bounds the peers one value lookup asks. Peers answer with the peers closest to
// the key, which point back at each other; without a bound and a seen-set a lookup bounces
// between them until it times out, redialing every one each time.
const maxLookupQueries = 32

// valueLookup is the state one lookup shares across its parallel branches: who has been asked.
type valueLookup struct {
	mu     sync.Mutex
	self   types.PeerID
	seen   map[types.PeerID]bool
	budget int
}

func newValueLookup(self types.PeerID) *valueLookup {
	return &valueLookup{self: self, seen: map[types.PeerID]bool{self: true}, budget: maxLookupQueries}
}

// claim reports whether p should be asked: not this node, not asked before, budget left.
func (l *valueLookup) claim(p types.PeerID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen[p] || l.budget <= 0 {
		return false
	}
	l.seen[p] = true
	l.budget--
	return true
}

type DHT interface {
	Get(key []byte) ([]byte, error)
	Set(key, data []byte) error
}

type EchoFogDHT struct {
	routingTable *RoutingTable
	swarm        *swarm.Swarm

	// peerID -> IP:Port (Bounded LRU + TTL to eliminate memory leaks and stale IPs)
	hotCache *cache.Cache[types.PeerID, string]

	pending sync.Map

	quotasMu   sync.Mutex
	peerQuotas *cache.Cache[types.PeerID, int]

	emitter atomic.Pointer[events.Emitter]
}

type PeerCapabilities struct {
	IsRouter    bool
	AcceptsIPv6 bool
	RelayFee    uint64
}

func NewEchoFogDHT(rt *RoutingTable, swarm *swarm.Swarm) *EchoFogDHT {
	dht := &EchoFogDHT{
		routingTable: rt,
		swarm:        swarm,
		hotCache:     cache.New[types.PeerID, string](2048, 15*time.Minute),
		peerQuotas:   cache.New[types.PeerID, int](4096, 1*time.Hour),
	}
	swarm.AddObserver(dht)
	return dht
}

func (d *EchoFogDHT) Name() string {
	return "EchoFogDHT"
}

func (d *EchoFogDHT) Start(ctx context.Context) error {
	return nil
}

func (d *EchoFogDHT) Stop() error {
	return nil
}

var ErrDHTEmpty = errors.New("DHT is empty, cannot start routing")
var ErrPeerLookupTimedOut = errors.New("peer lookup timed out")

func (d *EchoFogDHT) FindPeer(ctx context.Context, targetID types.PeerID) (string, error) {
	if addr, ok := d.hotCache.Get(targetID); ok {
		return addr, nil
	}

	closestLocal := d.routingTable.GetClosestPeers(targetID, 1)
	if len(closestLocal) > 0 && closestLocal[0].ID == targetID {
		d.hotCache.Set(targetID, closestLocal[0].Addr)
		return closestLocal[0].Addr, nil
	}

	nodesToQuery := d.routingTable.GetClosestPeers(targetID, 3)
	if len(nodesToQuery) == 0 {
		return "", ErrDHTEmpty
	}

	fastestResponse := make(chan string, 1)

	searchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	for _, node := range nodesToQuery {
		go func(p *PeerNode) {
			addr, found := d.queryPeerForNode(searchCtx, p.ID, targetID)
			if found {
				select {
				case fastestResponse <- addr:
				default:
				}
			}
		}(node)
	}

	select {
	case <-searchCtx.Done():
		return "", ErrPeerLookupTimedOut
	case addr := <-fastestResponse:
		d.hotCache.Set(targetID, addr)
		return addr, nil
	}
}

// targetOf is the place of a key in the ID space: the key itself when it is an ID, the hash of it
// otherwise (a key such as "bundle:<id>" would else map to the zero ID, one hot spot for all).
func targetOf(key []byte) types.PeerID {
	if id, err := types.ToPeerID(key); err == nil {
		return id
	}
	return sha256.Sum256(key)
}

// RecordKey is where a DHT record lives in local storage. Records never touch any other key: what
// peers store and ask for stays apart from our own data.
func RecordKey(key []byte) []byte { return append([]byte("dht:rec:"), key...) }

// localRecord returns the value of a record we hold.
func (d *EchoFogDHT) localRecord(key []byte) (*internal_pb.DhtRecord, bool) {
	raw, err := d.swarm.GetStorage().Get(RecordKey(key))
	if err != nil || raw == nil {
		return nil, false
	}
	var rec internal_pb.DhtRecord
	if proto.Unmarshal(raw, &rec) != nil {
		return nil, false
	}
	return &rec, true
}

func (d *EchoFogDHT) Get(key []byte) ([]byte, error) {
	if rec, ok := d.localRecord(key); ok {
		slog.Debug("DHT: Found value locally", "key", hex.EncodeToString(key)[:8])
		return rec.Value, nil
	}

	targetID := targetOf(key)
	nodesToQuery := d.routingTable.GetClosestPeers(targetID, Alpha)
	if len(nodesToQuery) == 0 {
		return nil, errors.New("DHT: no peers known to search for value")
	}

	fastestResponse := make(chan *internal_pb.DhtRecord, 1)

	searchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lookup := newValueLookup(d.swarm.GetSelfID())
	for _, node := range nodesToQuery {
		if !lookup.claim(node.ID) {
			continue
		}
		go func(p *PeerNode) {
			rec, found := d.queryPeerForValue(searchCtx, lookup, p.ID, key)
			if found {
				select {
				case fastestResponse <- rec:
				default:
				}
			}
		}(node)
	}

	select {
	case <-searchCtx.Done():
		return nil, errors.New("value lookup timed out or not found in network")
	case rec := <-fastestResponse:
		// not kept: what we looked up would show whom we talk to
		return rec.Value, nil
	}
}

func (d *EchoFogDHT) Set(key []byte, value []byte) error {
	myPubKey := d.swarm.GetMyPubKey()
	rec := &internal_pb.DhtRecord{
		Key:             key,
		Value:           value,
		PublisherPubKey: myPubKey[:],
		Timestamp:       uint64(time.Now().Unix()),
		Ttl:             86400,
	}

	signData := PackRecordForSigning(rec.Key, rec.Value, rec.Timestamp, rec.Ttl)

	var err error
	rec.Signature, err = d.swarm.CreateSignature(signData)
	if err != nil {
		return err
	}

	if recBytes, err := proto.Marshal(rec); err == nil {
		_ = d.swarm.GetStorage().Set(RecordKey(key), recBytes)
	}

	targetID := targetOf(key)
	nodes := d.routingTable.GetClosestPeers(targetID, Alpha)

	if len(nodes) == 0 {
		return nil
	}

	for _, node := range nodes {
		go func(p *PeerNode) {
			if !d.swarm.ThisIsActivePeer(p.ID) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()

				err := d.swarm.AddPotentialPeerWithCtx(ctx, p.ID, p.Addr)
				if err != nil {
					slog.Error("DHT: Network connection failed", "peer", p.ID, "err", err)
					return
				}
			}

			req := &internal_pb.MessageData{
				Payload: &internal_pb.MessageData_StoreReq{
					StoreReq: &internal_pb.StoreReq{Record: rec},
				},
			}

			err := d.swarm.SendDataForPeer(p.ID, net_types.TypeStore, req)
			if err != nil {
				slog.Error("DHT: Failed to send store request", "peer", p.ID, "err", err)
			} else {
				slog.Info("DHT: Successfully sent StoreReq", "peer", p.ID)
			}
		}(node)
	}

	return nil

}

// queryPeerForValue asks one peer for the record; when it answers with closer peers, those are
// asked in turn, each at most once per lookup (the caller has claimed peerToAsk already).
func (d *EchoFogDHT) queryPeerForValue(ctx context.Context, lookup *valueLookup, peerToAsk types.PeerID, targetKey []byte) (*internal_pb.DhtRecord, bool) {
	msgID := types.NewMessageID()

	req := &internal_pb.MessageData{
		MessageId: msgID[:],
		Payload: &internal_pb.MessageData_FindValueReq{
			FindValueReq: &internal_pb.FindValueReq{TargetKey: targetKey},
		},
	}

	waitCh := make(chan *internal_pb.FindValueRes, 1)
	d.pending.Store(string(msgID[:]), waitCh)

	defer d.pending.Delete(string(msgID[:]))

	err := d.swarm.SendDataForPeer(peerToAsk, net_types.TypeFindValue, req)
	if err != nil {
		return nil, false
	}

	select {
	case res := <-waitCh:
		if res.Record != nil {
			// only signed records, and only for the key that was asked
			if err := CheckRecord(res.Record, targetKey); err != nil {
				slog.Warn("DHT: Peer sent invalid record during Get", "peer", peerToAsk, "err", err)
				return nil, false
			}
			return res.Record, true
		}

		if len(res.ClosestPeers) > 0 {
			for _, peer := range res.ClosestPeers {
				var pubKey types.PeerPublicKey
				copy(pubKey[:], peer.PubKey)
				d.routingTable.AddOrUpdatePeer(pubKey, peer.Address)
				pID := types.PeerPubKeyToID(pubKey)
				if ctx.Err() != nil || !lookup.claim(pID) {
					continue
				}
				if !d.swarm.ThisIsActivePeer(pID) {
					dialCtx, dialCancel := context.WithTimeout(ctx, 2*time.Second)
					_ = d.swarm.AddPotentialPeerWithCtx(dialCtx, pID, peer.Address)
					dialCancel()
				}
				if d.swarm.ThisIsActivePeer(pID) {
					if nextRec, nextFound := d.queryPeerForValue(ctx, lookup, pID, targetKey); nextFound {
						return nextRec, true
					}
				}
			}
		}
		return nil, false

	case <-ctx.Done():
		return nil, false
	}
}

func (d *EchoFogDHT) queryPeerForNode(ctx context.Context, peerToAsk, targetID types.PeerID) (string, bool) {
	msgID := types.NewMessageID()

	req := &internal_pb.MessageData{
		MessageId: msgID[:],
		Payload: &internal_pb.MessageData_FindNodeReq{
			FindNodeReq: &internal_pb.FindNodeRequest{TargetId: targetID[:]},
		},
	}

	waitCh := make(chan *internal_pb.FindNodeResponse, 1)
	d.pending.Store(string(msgID[:]), waitCh)

	defer d.pending.Delete(string(msgID[:]))

	slog.Info("[STEP 1: OUT] Sending FindNodeReq", "msgID", hex.EncodeToString(msgID[:]), "targetID", targetID)

	err := d.swarm.SendDataForPeer(peerToAsk, net_types.TypeFindNode, req)
	if err != nil {
		return "", false
	}

	select {
	case res := <-waitCh:
		for _, peer := range res.ClosestPeers {
			var pubKey types.PeerPublicKey
			copy(pubKey[:], peer.PubKey)
			peerID := types.PeerPubKeyToID(pubKey)

			d.routingTable.AddOrUpdatePeer(pubKey, peer.Address)

			if peerID == targetID {
				return peer.Address, true
			}
		}
		return "", false

	case <-ctx.Done():
		return "", false
	}
}

func (d *EchoFogDHT) OnPeerConnected(peerID types.PeerID, pubKey types.PeerPublicKey, addr string) {
	if addr == "" {
		return // connected, but not reachable at a known address (inbound TCP)
	}
	d.routingTable.AddOrUpdatePeer(pubKey, addr)
	d.hotCache.Set(peerID, addr)
}

func (d *EchoFogDHT) OnPeerDisconnected(peerID types.PeerID) {
	d.hotCache.Delete(peerID)
}

func (d *EchoFogDHT) countRecordFromPeer(peerID types.PeerID) int {
	d.quotasMu.Lock()
	defer d.quotasMu.Unlock()

	count, _ := d.peerQuotas.Get(peerID)
	count++
	d.peerQuotas.Set(peerID, count)

	return count
}

func (d *EchoFogDHT) PruneExpired() {
	prunedHot := d.hotCache.PruneExpired()
	prunedQuotas := d.peerQuotas.PruneExpired()
	if prunedHot > 0 || prunedQuotas > 0 {
		slog.Debug("DHT: Pruned expired cache entries", "hot_cache", prunedHot, "quotas", prunedQuotas)
	}
}

func (d *EchoFogDHT) ResetQuotas() {
	d.quotasMu.Lock()
	defer d.quotasMu.Unlock()
	d.peerQuotas.Clear()
}

func PackRecordForSigning(key []byte, value []byte, ts uint64, ttl uint32) []byte {
	buf := make([]byte, 0, len(key)+len(value)+12)
	buf = append(buf, key...)
	buf = append(buf, value...)

	tsBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(tsBytes, ts)
	buf = append(buf, tsBytes...)

	ttlBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(ttlBytes, ttl)
	buf = append(buf, ttlBytes...)

	return buf
}

// SetEmitter attaches the node event bus for DHT security telemetry
func (d *EchoFogDHT) SetEmitter(e events.Emitter) {
	d.emitter.Store(&e)
}

func (d *EchoFogDHT) emit(topic string, payload any) {
	if p := d.emitter.Load(); p != nil && *p != nil {
		(*p).Emit(topic, payload)
	}
}
