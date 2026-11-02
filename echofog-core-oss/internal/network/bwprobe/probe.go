package bwprobe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	kindData byte = 1
	kindEnd  byte = 2
	kindAck  byte = 3
	kindReq  byte = 4

	frameSize     = 16 * 1024
	maxBurstBytes = 12 * 1024 * 1024
	// serveBurstBytes caps a burst sent on another peer's request: its 9-byte request must not buy
	// more than this. Our own upload bursts (Measure) may use maxBurstBytes; we pay for those.
	serveBurstBytes = 4 * 1024 * 1024
	maxBurstTime    = 2 * time.Second
	ackTimeout      = 4 * time.Second
	headerSize      = 1 + 8

	// After the end marker, wait for data frames still in flight: until none has arrived for
	// endSettle, and never longer than endSettleMax.
	endSettle    = 150 * time.Millisecond
	endSettleMax = 1500 * time.Millisecond

	// maxTrackedPeers bounds the per-peer interval table, maxFinishing the end markers settling at
	// once. Peer IDs cost nothing, so neither may grow with how many a sender makes up.
	maxTrackedPeers = 4096
	maxFinishing    = 32
)

// Limits bound what answering other peers' download probes costs this node. A peer ID is free, so
// the per-peer interval alone bounds little: the gate (SetGate), the per-address interval and the
// node-wide budget are what hold.
type Limits struct {
	Serve          bool          // answer download probe requests at all
	PerPeerEvery   time.Duration // at most one burst per peer this often
	PerIPEvery     time.Duration // at most one burst per peer address this often
	MaxConcurrent  int           // bursts in flight at once, node-wide
	BytesPerMinute int64         // burst bytes per minute, node-wide
}

// DefaultLimits answers probes: one 4 MB burst per peer every 30 s and per address every 10 s, two
// at a time, 16 MB a minute. A probe is a check a person asks for, so this costs a client nothing.
func DefaultLimits() Limits {
	return Limits{Serve: true, PerPeerEvery: 30 * time.Second, PerIPEvery: 10 * time.Second, MaxConcurrent: 2, BytesPerMinute: 4 * serveBurstBytes}
}

// Gate says whether a peer may have a download burst at all, and the address its interval is
// counted under. An exit lets in only the clients it serves that opened a tunnel lately.
type Gate func(peer types.PeerID) (addr string, ok bool)

var (
	ErrNoPeers     = errors.New("no connected peers to measure against")
	ErrProbeFailed = errors.New("bandwidth probe failed")
)

// Transport is the part of the swarm the prober needs
type Transport interface {
	SendRawFrame(peerID types.PeerID, msgType net_types.MessageType, data []byte) error
	RegisterFastFrameHandler(msgType net_types.MessageType, handler func(payload []byte, peerID types.PeerID) error)
}

type rxState struct {
	first, last time.Time
	bytes       uint64
}

// Prober measures real peer-to-peer goodput: it bursts data at a peer and reads back what the peer received and how long it took
type Prober struct {
	tr Transport

	mu        sync.Mutex
	limits    Limits
	gate      Gate // nil: no peer gets a burst
	lastReq   map[types.PeerID]time.Time
	lastIP    map[string]time.Time
	serving   int       // bursts in flight for other peers
	tokens    float64   // burst bytes the node-wide budget has left
	refilled  time.Time // when tokens were last topped up
	finishing int       // end markers settling
	rx        map[[8]byte]*rxState
	waiting   map[[8]byte]chan ackResult

	measure func(ctx context.Context, peer types.PeerID) (int, error) // Measure; tests replace it
}

type ackResult struct {
	bytes   uint64
	elapsed time.Duration
}

func New(tr Transport) *Prober {
	p := &Prober{lastReq: make(map[types.PeerID]time.Time), lastIP: make(map[string]time.Time), tr: tr, rx: make(map[[8]byte]*rxState), waiting: make(map[[8]byte]chan ackResult)}
	p.limits = DefaultLimits()
	p.tokens = float64(p.limits.BytesPerMinute)
	p.refilled = time.Now()
	p.measure = p.Measure
	tr.RegisterFastFrameHandler(net_types.TypeBandwidthProbe, p.handle)
	return p
}

// SetLimits replaces the limits on answering other peers' probes; Serve false stops answering.
// Measuring other peers is not affected.
func (p *Prober) SetLimits(l Limits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limits = l
	if p.tokens > float64(l.BytesPerMinute) {
		p.tokens = float64(l.BytesPerMinute)
	}
}

// SetGate sets who may ask this node for a download burst (see Gate). Until it is set, nobody may:
// a 9-byte request for a burst of megabytes is not for any peer that happens to be connected.
func (p *Prober) SetGate(g Gate) {
	p.mu.Lock()
	p.gate = g
	p.mu.Unlock()
}

// Limits returns the limits on answering other peers' probes.
func (p *Prober) Limits() Limits {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.limits
}

// admitLocked decides whether a download probe request gets a burst and reserves a whole burst of
// the node-wide budget for it; served gives back what was not sent. Caller holds p.mu.
// The caller has asked the gate already: addr is where from is.
func (p *Prober) admitLocked(from types.PeerID, addr string, now time.Time) bool {
	l := p.limits
	if !l.Serve || l.MaxConcurrent <= 0 || l.BytesPerMinute < serveBurstBytes {
		return false
	}
	if last, ok := p.lastReq[from]; ok && now.Sub(last) < l.PerPeerEvery {
		return false
	}
	if last, ok := p.lastIP[addr]; ok && addr != "" && now.Sub(last) < l.PerIPEvery {
		return false
	}
	if p.serving >= l.MaxConcurrent {
		return false
	}
	p.tokens = min(p.tokens+now.Sub(p.refilled).Seconds()*float64(l.BytesPerMinute)/60, float64(l.BytesPerMinute))
	p.refilled = now
	if p.tokens < serveBurstBytes {
		return false
	}
	if !makeRoom(p.lastReq, from, now, l.PerPeerEvery) || (addr != "" && !makeRoom(p.lastIP, addr, now, l.PerIPEvery)) {
		return false
	}
	p.lastReq[from] = now
	if addr != "" {
		p.lastIP[addr] = now
	}
	p.tokens -= serveBurstBytes
	p.serving++
	return true
}

// makeRoom keeps an interval table bounded: a new key gets in only while the table is under
// maxTrackedPeers, after the entries past their interval are dropped.
func makeRoom[K comparable](m map[K]time.Time, key K, now time.Time, every time.Duration) bool {
	if _, ok := m[key]; ok || len(m) < maxTrackedPeers {
		return true
	}
	for k, t := range m {
		if now.Sub(t) >= every {
			delete(m, k)
		}
	}
	return len(m) < maxTrackedPeers
}

// served ends a burst admitted by admitLocked, returning the bytes it did not send to the budget.
func (p *Prober) served(sent int) {
	p.mu.Lock()
	p.serving--
	if unsent := serveBurstBytes - sent; unsent > 0 {
		p.tokens = min(p.tokens+float64(unsent), float64(p.limits.BytesPerMinute))
	}
	p.mu.Unlock()
}

func (p *Prober) handle(payload []byte, from types.PeerID) error {
	if len(payload) < headerSize {
		return nil
	}
	var id [8]byte
	copy(id[:], payload[1:headerSize])

	switch payload[0] {
	case kindData:
		now := time.Now()
		p.mu.Lock()
		st := p.rx[id]
		if st == nil {
			if len(p.rx) > 64 {
				p.rx = make(map[[8]byte]*rxState)
			}
			st = &rxState{first: now}
			p.rx[id] = st
		}
		st.last = now
		st.bytes += uint64(len(payload))
		p.mu.Unlock()

	case kindReq:
		p.mu.Lock()
		gate := p.gate
		p.mu.Unlock()
		if gate == nil {
			return nil
		}
		addr, ok := gate(from)
		if !ok {
			return nil
		}
		p.mu.Lock()
		allowed := p.admitLocked(from, addr, time.Now())
		p.mu.Unlock()
		if allowed {
			go func() {
				sent, err := p.burstN(context.Background(), from, id, serveBurstBytes)
				p.served(sent)
				if err != nil {
					slog.Debug("bandwidth probe: could not send the burst", "err", err)
				}
			}()
		}

	case kindEnd:
		// Every frame travels on its own stream and is handled in its own goroutine, so the end
		// marker can overtake data frames sent before it. Settle first, then count. A flood of
		// end markers gets a bounded number of settling goroutines; our own probes always do.
		p.mu.Lock()
		busy := p.finishing >= maxFinishing && p.waiting[id] == nil
		if !busy {
			p.finishing++
		}
		p.mu.Unlock()
		if busy {
			return nil
		}
		go func() {
			p.finish(id, from)
			p.mu.Lock()
			p.finishing--
			p.mu.Unlock()
		}()
		return nil

	case kindAck:
		if len(payload) < headerSize+16 {
			return nil
		}
		res := ackResult{
			bytes:   binary.BigEndian.Uint64(payload[headerSize:]),
			elapsed: time.Duration(binary.BigEndian.Uint64(payload[headerSize+8:])),
		}
		p.mu.Lock()
		ch := p.waiting[id]
		p.mu.Unlock()
		if ch != nil {
			select {
			case ch <- res:
			default:
			}
		}
	}
	return nil
}

// Measure bursts data at one peer and returns the goodput in kbps as observed by that peer
func (p *Prober) Measure(ctx context.Context, peer types.PeerID) (int, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return 0, err
	}
	ch := make(chan ackResult, 1)
	p.mu.Lock()
	p.waiting[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
	}()

	if err := p.burst(ctx, peer, id); err != nil {
		return 0, err
	}

	select {
	case res := <-ch:
		if res.bytes == 0 || res.elapsed <= 0 {
			return 0, ErrProbeFailed
		}
		return int(float64(res.bytes) * 8 / 1000 / res.elapsed.Seconds()), nil
	case <-time.After(ackTimeout):
		return 0, ErrProbeFailed
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// MeasureBest probes up to maxPeers peers and returns the highest observed rate, discounted for safety headroom
func (p *Prober) MeasureBest(ctx context.Context, peers []types.PeerID, maxPeers int) (int, error) {
	if len(peers) == 0 {
		return 0, ErrNoPeers
	}
	if maxPeers <= 0 || maxPeers > len(peers) {
		maxPeers = len(peers)
	}
	best := 0
	var lastErr error
	for _, peer := range peers[:maxPeers] {
		kbps, err := p.measure(ctx, peer)
		if err != nil {
			lastErr = err
			continue
		}
		if kbps > best {
			best = kbps
		}
	}
	if best == 0 {
		if lastErr == nil {
			lastErr = ErrProbeFailed
		}
		return 0, lastErr
	}
	return best * 80 / 100, nil
}

// burst sends the data frames and the end marker for one probe id
func (p *Prober) burst(ctx context.Context, peer types.PeerID, id [8]byte) error {
	_, err := p.burstN(ctx, peer, id, maxBurstBytes)
	return err
}

// burstN is burst of at most limit bytes that also returns how many data bytes went out.
func (p *Prober) burstN(ctx context.Context, peer types.PeerID, id [8]byte, limit int) (int, error) {
	frame := make([]byte, frameSize)
	frame[0] = kindData
	copy(frame[1:headerSize], id[:])
	_, _ = rand.Read(frame[headerSize:])

	start := time.Now()
	sent := 0
	for ; sent < limit && time.Since(start) < maxBurstTime; sent += len(frame) {
		if err := ctx.Err(); err != nil {
			return sent, err
		}
		if err := p.tr.SendRawFrame(peer, net_types.TypeBandwidthProbe, frame); err != nil {
			return sent, err
		}
	}

	end := make([]byte, headerSize)
	end[0] = kindEnd
	copy(end[1:], id[:])
	return sent, p.tr.SendRawFrame(peer, net_types.TypeBandwidthProbe, end)
}

// MeasureDownload asks the peer to burst data at us and returns the goodput in kbps that we observed
func (p *Prober) MeasureDownload(ctx context.Context, peer types.PeerID) (int, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return 0, err
	}
	ch := make(chan ackResult, 1)
	p.mu.Lock()
	p.waiting[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiting, id)
		delete(p.rx, id)
		p.mu.Unlock()
	}()

	req := make([]byte, headerSize)
	req[0] = kindReq
	copy(req[1:], id[:])
	if err := p.tr.SendRawFrame(peer, net_types.TypeBandwidthProbe, req); err != nil {
		return 0, err
	}

	select {
	case res := <-ch:
		if res.bytes == 0 || res.elapsed <= 0 {
			return 0, ErrProbeFailed
		}
		return int(float64(res.bytes) * 8 / 1000 / res.elapsed.Seconds()), nil
	case <-time.After(maxBurstTime + ackTimeout):
		// a slow sender can take longer than its burst budget to get the end marker out, while the
		// data it already sent says enough about the link
		if kbps, ok := p.partialRate(id); ok {
			return kbps, nil
		}
		return 0, ErrProbeFailed
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// partialRate is the rate of a download burst whose end marker has not arrived yet.
func (p *Prober) partialRate(id [8]byte) (int, bool) {
	p.mu.Lock()
	st := p.rx[id]
	p.mu.Unlock()
	if st == nil || st.bytes == 0 {
		return 0, false
	}
	elapsed := st.last.Sub(st.first)
	if elapsed < 100*time.Millisecond {
		return 0, false
	}
	return int(float64(st.bytes) * 8 / 1000 / elapsed.Seconds()), true
}

// finish closes a burst once its end marker arrived: it waits for straggling data frames, then
// hands the result to a local MeasureDownload, or acks it back to the peer that sent the burst.
func (p *Prober) finish(id [8]byte, from types.PeerID) {
	deadline := time.Now().Add(endSettleMax)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		st := p.rx[id]
		quiet := st != nil && time.Since(st.last) >= endSettle
		p.mu.Unlock()
		if quiet {
			break
		}
		time.Sleep(endSettle / 4)
	}

	p.mu.Lock()
	st := p.rx[id]
	delete(p.rx, id)
	ch := p.waiting[id]
	p.mu.Unlock()

	var res ackResult
	if st != nil {
		res = ackResult{bytes: st.bytes, elapsed: st.last.Sub(st.first)}
	}
	if ch != nil {
		select {
		case ch <- res: // zero bytes when nothing arrived: the caller fails at once instead of timing out
		default:
		}
		return
	}

	ack := make([]byte, headerSize+16)
	ack[0] = kindAck
	copy(ack[1:headerSize], id[:])
	binary.BigEndian.PutUint64(ack[headerSize:], res.bytes)
	binary.BigEndian.PutUint64(ack[headerSize+8:], uint64(res.elapsed))
	_ = p.tr.SendRawFrame(from, net_types.TypeBandwidthProbe, ack)
}
