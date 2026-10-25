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
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

type DHT interface {
	Get(key []byte) ([]byte, error)
	Set(key, data []byte) error
}

type EchoFogDHT struct {
	routingTable *RoutingTable
	swarm        *swarm.Swarm

	// peerID -> IP:Port (Bounded LRU + TTL to eliminate memory leaks and stale IPs)
	hotCache *cache.Cache[types.PeerID, string]
	// peers that did not answer a dial lately: out of the table and of lookups (see lookup.go)
	unreachable *cache.Cache[types.PeerID, struct{}]
	// what recent value lookups came to, by key: the value found, or that none was. The exit
	// directory and messaging ask for the same keys again within seconds; a walk per ask would
	// cost round trips for an answer that cannot have changed yet. Memory only, never on disk.
	recent *cache.Cache[string, []byte]
	misses *cache.Cache[string, struct{}]

	dialsMu sync.Mutex
	dials   map[types.PeerID]*dialAttempt // dials in progress, one per peer (see dialOnce)

	pending sync.Map

	quotasMu   sync.Mutex
	peerQuotas *cache.Cache[types.PeerID, int]

	// records held and new ones stored this hour, all peers together (see records.go)
	recMu      sync.Mutex
	records    int
	recCounted bool
	storesHour int
	sweepMu    sync.Mutex

	emitter atomic.Pointer[events.Emitter]

	alone  atomic.Bool   // no peer connected: the next connection is a (re)join
	joined chan struct{} // a (re)join happened: look up our own ID
	stop   context.CancelFunc
	done   chan struct{}
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
		unreachable:  cache.New[types.PeerID, struct{}](4096, unreachableFor),
		recent:       cache.New[string, []byte](1024, recentValueFor),
		misses:       cache.New[string, struct{}](4096, missFor),
		peerQuotas:   cache.New[types.PeerID, int](4096, 1*time.Hour),
		dials:        map[types.PeerID]*dialAttempt{},
		joined:       make(chan struct{}, 1),
	}
	dht.alone.Store(true)
	rt.SetAddrPolicy(swarm.AddrPolicy())
	swarm.AddObserver(dht)
	swarm.AddCandidateSource(dht.dialCandidates)
	swarm.OnPeerLearned(func(pub types.PeerPublicKey, addr string, _ handshake.Capability) {
		rt.AddOrUpdatePeer(pub, addr)
	})
	return dht
}

func (d *EchoFogDHT) Name() string {
	return "EchoFogDHT"
}

// Start reloads the routing table saved by the last run and keeps the table fresh: a lookup of
// our own ID whenever the node joins (or rejoins) the network, and a lookup of a random ID in
// each bucket no lookup went into for a while.
func (d *EchoFogDHT) Start(ctx context.Context) error {
	if n := d.routingTable.Load(); n > 0 {
		slog.Info("DHT: routing table restored", "peers", n)
	}
	ctx, cancel := context.WithCancel(ctx)
	d.stop = cancel
	d.done = make(chan struct{})
	go d.maintain(ctx)
	return nil
}

// Stop ends the maintenance and saves the routing table.
func (d *EchoFogDHT) Stop() error {
	if d.stop != nil {
		d.stop()
		<-d.done
	}
	return d.routingTable.Save()
}

var ErrDHTEmpty = errors.New("DHT is empty, cannot start routing")
var ErrPeerLookupTimedOut = errors.New("peer lookup timed out")
var ErrValueNotFound = errors.New("value not found in network")
var ErrNoPeers = errors.New("DHT: no peers known to search for value")

const (
	// findPeerWait bounds one peer lookup: each step may have to connect to the peer it asks.
	findPeerWait = 10 * time.Second
	// valueLookupWait bounds one value lookup; a lookup normally ends well before, once the
	// closest peers have all answered.
	valueLookupWait = 6 * time.Second
	// recentValueFor is how long a value found is answered from memory, far below how often
	// records are republished. missFor is how long a key nobody held is: long enough to absorb a
	// burst of asks for the same key, short enough that a record just published is found soon.
	recentValueFor = 30 * time.Second
	missFor        = 15 * time.Second
)

// FindPeer returns where a peer accepts connections: from the cache, the routing table or a
// connection we have, or else by an iterative lookup through the network.
func (d *EchoFogDHT) FindPeer(ctx context.Context, targetID types.PeerID) (string, error) {
	if addr, ok := d.hotCache.Get(targetID); ok {
		return addr, nil
	}
	if p := d.swarm.GetPeer(targetID); p != nil && p.Addr() != "" {
		return p.Addr(), nil
	}

	closestLocal := d.routingTable.GetClosestPeers(targetID, 1)
	if len(closestLocal) > 0 && closestLocal[0].ID == targetID {
		d.hotCache.Set(targetID, closestLocal[0].Addr)
		return closestLocal[0].Addr, nil
	}
	if len(closestLocal) == 0 && len(d.swarm.GetAllPeers()) == 0 {
		return "", ErrDHTEmpty
	}

	searchCtx, cancel := context.WithTimeout(ctx, findPeerWait)
	defer cancel()
	if res := d.newLookup(targetID, nil, &targetID).run(searchCtx); res.found != nil && res.found.Addr != "" {
		d.hotCache.Set(targetID, res.found.Addr)
		return res.found.Addr, nil
	}
	return "", ErrPeerLookupTimedOut
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
// peers store and ask for stays apart from our own data. The key is hashed: record keys name
// peers ("bundle:<id>"), and the database must not list in the open whose records this node
// holds or looked up. The record itself carries its key, so nothing needs it back.
func RecordKey(key []byte) []byte {
	sum := sha256.Sum256(key)
	return append([]byte(RecordPrefix), hex.EncodeToString(sum[:])...)
}

// RecordPrefix starts every DHT record key in local storage.
const RecordPrefix = "dht:rec:"

// localRecord returns the value of a record we hold.
func (d *EchoFogDHT) localRecord(key []byte) (*internal_pb.DhtRecord, bool) {
	raw, err := d.swarm.GetStorage().Get(RecordKey(key))
	if err != nil || raw == nil {
		return nil, false
	}
	var rec internal_pb.DhtRecord
	if proto.Unmarshal(raw, &rec) != nil || recordExpired(&rec, time.Now()) {
		return nil, false // the sweep deletes it
	}
	return &rec, true
}

func (d *EchoFogDHT) Get(key []byte) ([]byte, error) {
	if rec, ok := d.localRecord(key); ok {
		slog.Debug("DHT: Found value locally", "key", hex.EncodeToString(key)[:8])
		return rec.Value, nil
	}
	return d.findValue(key)
}

// findValue walks the network for a record: a lookup toward the place of the key in the ID
// space that ends as soon as a peer hands the record over, or once the closest peers have all
// answered without it. What a lookup came to lately is answered from memory.
func (d *EchoFogDHT) findValue(key []byte) ([]byte, error) {
	k := string(key)
	if v, ok := d.recent.Get(k); ok {
		return v, nil
	}
	if _, ok := d.misses.Get(k); ok {
		return nil, ErrValueNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), valueLookupWait)
	defer cancel()
	res := d.newLookup(targetOf(key), key, nil).run(ctx)
	if res.record != nil {
		// kept in memory only: on disk, what we looked up would show whom we talk to
		d.recent.Set(k, res.record.Value)
		return res.record.Value, nil
	}
	if res.asked == 0 {
		return nil, ErrNoPeers
	}
	if ctx.Err() == nil {
		d.misses.Set(k, struct{}{}) // the lookup converged: nobody near the key holds it
	}
	return nil, ErrValueNotFound
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
	d.recent.Delete(string(key))
	d.misses.Delete(string(key))

	go d.replicate(rec, targetOf(key))
	return nil
}

// The walk toward a key before publishing gives up after this long; the peers it reached by
// then are the ones stored on.
const replicateTimeout = 10 * time.Second

// replicate stores a signed record on the closest peers that can actually be reached. The
// Replicas closest among the peers we are connected to get it at once: in a small network those
// are the entry nodes, where everybody's lookups start, so the record can be read right away.
// Then a walk toward the key finds the closest live peers in the network (the local table often
// holds only peers that left), and the Replicas closest of those get it as well. The walk
// connects to the peers it asks, so they can be stored on without another dial.
func (d *EchoFogDHT) replicate(rec *internal_pb.DhtRecord, target types.PeerID) {
	ctx, cancel := context.WithTimeout(context.Background(), replicateTimeout)
	defer cancel()

	req := &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_StoreReq{StoreReq: &internal_pb.StoreReq{Record: rec}},
	}
	tried := map[types.PeerID]bool{}
	store := func(candidates []*PeerNode) int {
		stored := 0
		for _, p := range candidates {
			if stored == Replicas {
				break
			}
			if tried[p.ID] || !d.swarm.ThisIsActivePeer(p.ID) {
				continue
			}
			tried[p.ID] = true
			if err := d.swarm.SendDataForPeer(p.ID, net_types.TypeStore, req); err != nil {
				slog.Debug("DHT: store request failed, trying the next peer", "peer", p.ID, "err", err)
				continue
			}
			stored++
		}
		return stored
	}
	near := store(d.connectedClosest(target))
	res := d.newLookup(target, nil, nil).run(ctx)
	walked := store(res.closest)
	slog.Debug("DHT: record published", "on_connected", near, "on_closest", walked, "closest_found", len(res.closest))
}

func (d *EchoFogDHT) OnPeerConnected(peerID types.PeerID, pubKey types.PeerPublicKey, addr string) {
	if d.alone.CompareAndSwap(true, false) {
		select {
		case d.joined <- struct{}{}:
		default:
		}
	}
	d.unreachable.Delete(peerID) // reachable after all
	if addr == "" {
		return // connected, but not reachable at a known address (inbound TCP)
	}
	d.routingTable.AddOrUpdatePeer(pubKey, addr)
	d.hotCache.Set(peerID, addr)
}

func (d *EchoFogDHT) OnPeerDisconnected(peerID types.PeerID) {
	d.hotCache.Delete(peerID)
	if d.swarm != nil && len(d.swarm.GetAllPeers()) == 0 {
		d.alone.Store(true)
	}
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
	d.unreachable.PruneExpired()
	d.recent.PruneExpired()
	d.misses.PruneExpired()
	if prunedHot > 0 || prunedQuotas > 0 {
		slog.Debug("DHT: Pruned expired cache entries", "hot_cache", prunedHot, "quotas", prunedQuotas)
	}
	d.SweepRecords(time.Now())
}

func (d *EchoFogDHT) ResetQuotas() {
	d.quotasMu.Lock()
	d.peerQuotas.Clear()
	d.quotasMu.Unlock()
	d.recMu.Lock()
	d.storesHour = 0
	d.recMu.Unlock()
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
