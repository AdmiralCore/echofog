package exit_gateway

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A UDP session's datagrams used to ride its reliable stream, so one lost packet held up every
// datagram behind it: head-of-line blocking that browsers' HTTP/3, games and calls feel at once.
// Over QUIC they now travel as DATAGRAM frames, the way hysteria carries UDP: the stream only
// sets the session up and gives it an ID. Over the TCP fallback, and for the rare datagram too
// big to fragment sensibly, the stream still carries them.

// udpHelloDatagrams in the TypeExitUDP request says the client can take datagrams; an exit that
// can too answers with resultOK followed by the 4-byte session ID. Older peers ignore the byte
// or do not send the ID, and both sides then stay on the stream.
const udpHelloDatagrams byte = 1

const (
	udpDgramHeader = 4 + 2 + 1 + 1 + 2 // session ID, packet ID, fragment index, count, address length
	// udpMaxFragments: past this a datagram goes on the stream. Every fragment must arrive for the
	// datagram to count, so a big one cut in many pieces is lost far more often than it is
	// delayed on the stream.
	udpMaxFragments = 8
)

// udpLink carries one direction of exit UDP datagrams to the peer at the other end of a stream,
// and delivers what that peer sends the other way.
type udpLink interface {
	send(frame []byte) error
	// subscribe hands fn every frame for session id until the returned func is called; nil
	// when id is already taken. fn runs on the connection's datagram loop and must not block.
	subscribe(id uint32, fn func([]byte)) func()
}

// datagramStream is a stream that can give its UDP session a datagram link (swarmStream does,
// over QUIC). toExit: the link sends towards the exit, as the client does.
type datagramStream interface {
	udpLink(toExit bool) udpLink
}

// encodeUDPFragment builds one datagram frame.
func encodeUDPFragment(id uint32, pkt uint16, idx, count uint8, addr string, data []byte) []byte {
	out := make([]byte, udpDgramHeader+len(addr)+len(data))
	binary.BigEndian.PutUint32(out, id)
	binary.BigEndian.PutUint16(out[4:], pkt)
	out[6], out[7] = idx, count
	binary.BigEndian.PutUint16(out[8:], uint16(len(addr)))
	copy(out[udpDgramHeader:], addr)
	copy(out[udpDgramHeader+len(addr):], data)
	return out
}

type udpFragment struct {
	pkt        uint16
	idx, count uint8
	addr       string
	data       []byte
}

// decodeUDPFragment parses a frame without its session ID (the demux took it).
func decodeUDPFragment(b []byte) (udpFragment, bool) {
	if len(b) < udpDgramHeader-4 {
		return udpFragment{}, false
	}
	f := udpFragment{pkt: binary.BigEndian.Uint16(b), idx: b[2], count: b[3]}
	n := int(binary.BigEndian.Uint16(b[4:]))
	b = b[6:]
	if n == 0 || n > maxTargetLen || len(b) < n || f.count == 0 || f.count > udpMaxFragments || f.idx >= f.count {
		return udpFragment{}, false
	}
	f.addr, f.data = string(b[:n]), b[n:]
	return f, true
}

// defrag puts fragmented datagrams back together. Like hysteria's it keeps one datagram in
// progress: a sender emits a datagram's fragments back to back, and a fragment of a newer one
// means the old one lost a piece.
type defrag struct {
	mu    sync.Mutex
	pkt   uint16
	frags [][]byte
	got   int
	size  int
}

func (d *defrag) feed(f udpFragment) ([]byte, bool) {
	if f.count == 1 {
		return f.data, true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.frags == nil || f.pkt != d.pkt || len(d.frags) != int(f.count) {
		d.pkt, d.frags, d.got, d.size = f.pkt, make([][]byte, f.count), 0, 0
	}
	if d.frags[f.idx] != nil {
		return nil, false
	}
	d.frags[f.idx] = f.data
	d.got++
	d.size += len(f.data)
	if d.got < len(d.frags) {
		return nil, false
	}
	out := make([]byte, 0, d.size)
	for _, p := range d.frags {
		out = append(out, p...)
	}
	d.frags = nil
	return out, true
}

// dgramSender sends one session's datagrams over its link, cut to what the path takes.
type dgramSender struct {
	link    udpLink
	id      uint32
	pkt     atomic.Uint32
	max     atomic.Int64 // largest frame the path takes, margin included; 0: not probed yet
	probeAt atomic.Int64 // unix nanos of the last probe
}

const (
	// udpDgramSlack is kept free under the limit the link reports. quic-go v0.58 reports the
	// whole packet size once path MTU discovery has grown it, not what is left for a datagram
	// after the packet header and the AEAD tag, and silently drops a frame between the two.
	udpDgramSlack = 48
	// udpProbeEvery: the limit is asked again this often, as path MTU discovery moves it.
	udpProbeEvery = 10 * time.Second
	// udpProbeSize is larger than any datagram a QUIC connection takes (quic-go never grows a
	// packet past 1452 bytes), so asking queues nothing.
	udpProbeSize = 4096
)

// limit returns the largest frame to send now, asking the link when it is not known or stale.
// An oversized frame is refused before anything is queued, which tells the limit for free.
func (s *dgramSender) limit() int64 {
	now := time.Now().UnixNano()
	if m := s.max.Load(); m > 0 && now-s.probeAt.Load() < int64(udpProbeEvery) {
		return m
	}
	s.probeAt.Store(now)
	probe := make([]byte, udpProbeSize) // fragment count 0: a peer that got it would drop it
	binary.BigEndian.PutUint32(probe, s.id)
	var tooLarge *network.DatagramTooLargeError
	if err := s.link.send(probe); errors.As(err, &tooLarge) {
		s.max.Store(max(int64(tooLarge.Max)-udpDgramSlack, 0))
	} else if s.max.Load() == 0 {
		s.max.Store(1200 - udpDgramSlack) // what every QUIC path carries
	}
	return s.max.Load()
}

// send reports false when data must go on the stream instead: too big to fragment sensibly, or
// the link refused it outright.
func (s *dgramSender) send(addr string, data []byte) bool {
	pkt := uint16(s.pkt.Add(1))
	limit := int(s.limit())
	if udpDgramHeader+len(addr)+len(data) <= limit {
		return s.link.send(encodeUDPFragment(s.id, pkt, 0, 1, addr, data)) == nil
	}
	per := limit - udpDgramHeader - len(addr)
	if per < 64 {
		return false
	}
	count := (len(data) + per - 1) / per
	if count > udpMaxFragments {
		return false
	}
	for i := 0; i < count; i++ {
		chunk := data[i*per : min(len(data), (i+1)*per)]
		if err := s.link.send(encodeUDPFragment(s.id, pkt, uint8(i), uint8(count), addr, chunk)); err != nil {
			return false
		}
	}
	return true
}

// udpDemux routes a swarm's exit UDP datagrams of one direction to their sessions.
type udpDemux struct {
	mu sync.RWMutex
	m  map[udpDemuxKey]func([]byte)
}

type udpDemuxKey struct {
	peer types.PeerID // the authenticated sender: a peer only ever reaches its own sessions
	id   uint32
}

type udpDemuxSlot struct {
	sw *swarm.Swarm
	t  net_types.MessageType
}

var udpDemuxes sync.Map // udpDemuxSlot -> *udpDemux

// demuxFor returns the demux for frames of type t arriving at sw, registering it on first use.
func demuxFor(sw *swarm.Swarm, t net_types.MessageType) *udpDemux {
	slot := udpDemuxSlot{sw, t}
	if d, ok := udpDemuxes.Load(slot); ok {
		return d.(*udpDemux)
	}
	d, loaded := udpDemuxes.LoadOrStore(slot, &udpDemux{m: make(map[udpDemuxKey]func([]byte))})
	dm := d.(*udpDemux)
	if !loaded {
		sw.RegisterFastFrameHandler(t, dm.dispatch)
	}
	return dm
}

func (d *udpDemux) dispatch(payload []byte, peer types.PeerID) error {
	if len(payload) < udpDgramHeader {
		return nil
	}
	d.mu.RLock()
	fn := d.m[udpDemuxKey{peer, binary.BigEndian.Uint32(payload)}]
	d.mu.RUnlock()
	if fn != nil {
		fn(payload[4:])
	}
	return nil // unknown sessions are dropped like stray UDP
}

func (d *udpDemux) add(peer types.PeerID, id uint32, fn func([]byte)) func() {
	k := udpDemuxKey{peer, id}
	d.mu.Lock()
	if d.m[k] != nil {
		d.mu.Unlock()
		return nil // taken
	}
	d.m[k] = fn
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		delete(d.m, k)
		d.mu.Unlock()
	}
}

// swarmUDPLink is a udpLink over the swarm connection to one peer.
type swarmUDPLink struct {
	sw   *swarm.Swarm
	peer types.PeerID
	out  net_types.MessageType // what we send
	in   *udpDemux             // what the peer sends us
}

func (l *swarmUDPLink) send(frame []byte) error {
	return l.sw.SendUnreliableFrame(l.peer, l.out, frame)
}

func (l *swarmUDPLink) subscribe(id uint32, fn func([]byte)) func() {
	return l.in.add(l.peer, id, fn)
}

func (s swarmStream) udpLink(toExit bool) udpLink {
	if s.sw == nil {
		return nil
	}
	p := s.sw.GetPeer(s.RemoteID)
	if p == nil || p.ActiveProtocol() == router.ProtoTCPTLS {
		return nil // over the TCP fallback a datagram is no less reliable than the stream
	}
	out, in := net_types.TypeExitUDPFromExit, net_types.TypeExitUDPToExit
	if toExit {
		out, in = in, out
	}
	return &swarmUDPLink{sw: s.sw, peer: s.RemoteID, out: out, in: demuxFor(s.sw, in)}
}
