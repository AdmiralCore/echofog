package bwprobe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
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
	maxBurstTime  = 2 * time.Second
	ackTimeout    = 4 * time.Second
	headerSize    = 1 + 8
)

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

	mu      sync.Mutex
	lastReq map[types.PeerID]time.Time
	rx      map[[8]byte]*rxState
	waiting map[[8]byte]chan ackResult
}

type ackResult struct {
	bytes   uint64
	elapsed time.Duration
}

func New(tr Transport) *Prober {
	p := &Prober{lastReq: make(map[types.PeerID]time.Time), tr: tr, rx: make(map[[8]byte]*rxState), waiting: make(map[[8]byte]chan ackResult)}
	tr.RegisterFastFrameHandler(net_types.TypeBandwidthProbe, p.handle)
	return p
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
		last := p.lastReq[from]
		allowed := time.Since(last) > 5*time.Second
		if allowed {
			p.lastReq[from] = time.Now()
		}
		p.mu.Unlock()
		if allowed {
			go func() { _ = p.burst(context.Background(), from, id) }()
		}

	case kindEnd:
		p.mu.Lock()
		st := p.rx[id]
		delete(p.rx, id)
		ch := p.waiting[id]
		p.mu.Unlock()

		if ch != nil {
			if st != nil {
				select {
				case ch <- ackResult{bytes: st.bytes, elapsed: st.last.Sub(st.first)}:
				default:
				}
			}
			return nil
		}

		ack := make([]byte, headerSize+16)
		ack[0] = kindAck
		copy(ack[1:headerSize], id[:])
		if st != nil {
			binary.BigEndian.PutUint64(ack[headerSize:], st.bytes)
			binary.BigEndian.PutUint64(ack[headerSize+8:], uint64(st.last.Sub(st.first)))
		}
		return p.tr.SendRawFrame(from, net_types.TypeBandwidthProbe, ack)

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
		kbps, err := p.Measure(ctx, peer)
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
	frame := make([]byte, frameSize)
	frame[0] = kindData
	copy(frame[1:headerSize], id[:])
	_, _ = rand.Read(frame[headerSize:])

	start := time.Now()
	for sent := 0; sent < maxBurstBytes && time.Since(start) < maxBurstTime; sent += len(frame) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.tr.SendRawFrame(peer, net_types.TypeBandwidthProbe, frame); err != nil {
			return err
		}
	}

	end := make([]byte, headerSize)
	end[0] = kindEnd
	copy(end[1:], id[:])
	return p.tr.SendRawFrame(peer, net_types.TypeBandwidthProbe, end)
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
		return 0, ErrProbeFailed
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
