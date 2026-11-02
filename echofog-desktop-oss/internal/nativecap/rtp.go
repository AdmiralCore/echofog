package nativecap

import (
	"bytes"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/pkg/videolayer"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// h264PayloadType is what the packets carry; viewers' bridges rewrite it to what their webview
// negotiated, and relays do not look at it.
const h264PayloadType = 112

// annexBNALs returns the NAL units of an Annex-B stream, without their start codes.
func annexBNALs(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				if n := bytes.TrimRight(b[start:i], "\x00"); len(n) > 0 {
					out = append(out, n)
				}
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// hasIDR reports whether an access unit holds an IDR slice.
func hasIDR(au []byte) bool {
	for _, n := range annexBNALs(au) {
		if n[0]&0x1f == 5 {
			return true
		}
	}
	return false
}

// packetizer turns access units into RTP packets of at most mtu bytes, in the H.264 packetization
// mode 1 the bridge negotiates (SPS and PPS go out as one STAP-A, large NAL units as FU-A). With
// layered set, every packet carries the frame's temporal-layer mark (videolayer).
type packetizer struct {
	mu      sync.Mutex // frame: the encoder being replaced may still let out frames as the new one starts
	mtu     int
	ssrc    uint32
	seq     uint16
	pl      codecs.H264Payloader
	layered bool
}

func newPacketizer(mtu int) *packetizer {
	return &packetizer{mtu: mtu, ssrc: rand.Uint32(), seq: uint16(rand.Uint32())}
}

// frame packs one access unit, marked with its layer l when layered, and hands its packets to out,
// all under the packetizer's lock: the encoder's goroutine calls it, and while a change of level
// replaces the encoder, the old one's last frames and the new one's first may come at once. Their
// packets then still get sequence numbers of their own, in the order they reach out.
func (p *packetizer) frame(layered bool, au []byte, ts uint32, l frameLayer, key bool, out func([][]byte)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.layered = layered
	out(p.packets(au, ts, l, key))
}

// packets returns the marshaled packets of one access unit with the RTP timestamp ts (90 kHz),
// marked with its layer l when the packetizer is layered. The core counts the packets of each
// layer (the mark's L0 and L1) as it numbers the units.
func (p *packetizer) packets(au []byte, ts uint32, l frameLayer, key bool) [][]byte {
	room := p.mtu - 12
	if p.layered {
		room -= videolayer.ExtensionLen
	}
	payloads := p.pl.Payload(uint16(room), au)
	out := make([][]byte, 0, len(payloads))
	for i, pl := range payloads {
		pkt := rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: h264PayloadType, SequenceNumber: p.seq, Timestamp: ts, SSRC: p.ssrc,
			Marker: i == len(payloads)-1,
		}, Payload: pl}
		if p.layered {
			m := videolayer.Mark{TID: l.tid, Start: i == 0, End: i == len(payloads)-1, Key: key, Discardable: l.discardable,
				Index: uint16(i), Count: uint16(len(payloads))}
			pkt.Header.Extension, pkt.Header.ExtensionProfile = true, 0xBEDE
			if err := pkt.Header.SetExtension(videolayer.ExtensionID, m.Data()); err != nil {
				continue
			}
		}
		b, err := pkt.Marshal()
		if err != nil {
			continue
		}
		p.seq++
		out = append(out, b)
	}
	return out
}

// The webview's WebRTC paced what it sent; without it, a frame's packets would leave in one burst
// that a queue on the path drops the end of, a keyframe's hundreds of them most of all. The pacer
// sends at a multiple of the bitrate, like WebRTC's, and faster when its queue would otherwise
// hold more than maxPacerDelay. That is as long as the viewers' playout buffer: the hardware
// encoder makes a keyframe 90-215 ms of the bitrate whatever its rate buffer says, and draining
// it within 40 ms sent 250 KB at 50 Mbit/s into a 15 Mbit/s link, whose router dropped its end.
const (
	pacingFactor  = 2.5
	maxPacerDelay = 100 * time.Millisecond
	pacerTick     = time.Millisecond
	pacerBurst    = 2 * time.Millisecond // what the bucket may hold when the queue was idle
)

type pacedFrame struct {
	packets  [][]byte
	bytes    int
	enqueued time.Time
}

type pacer struct {
	publish func([]byte)
	bitrate atomic.Uint64 // bits per second the encoder targets
	now     func() time.Time
	sleep   func(time.Duration)

	mu     sync.Mutex
	queue  []*pacedFrame
	queued int // bytes
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}

	// stats, for the stream log
	packets, sentBytes, frames atomic.Uint64
	lastDelay                  atomic.Int64 // ns a frame waited before its last packet left
}

func newPacer(publish func([]byte), bitrate uint64) *pacer {
	p := &pacer{publish: publish, now: time.Now, sleep: time.Sleep,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	p.bitrate.Store(bitrate)
	return p
}

func (p *pacer) start() { go p.run() }

// push queues the packets of one frame.
func (p *pacer) push(packets [][]byte) {
	f := &pacedFrame{packets: packets, enqueued: p.now()}
	for _, b := range packets {
		f.bytes += len(b)
	}
	p.mu.Lock()
	p.queue = append(p.queue, f)
	p.queued += f.bytes
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// rate is the send rate in bytes per second for a queue of queued bytes whose oldest frame
// waits since age: fast enough that the queue is empty when that frame is maxPacerDelay old.
func (p *pacer) rate(queued int, age time.Duration) float64 {
	r := pacingFactor * float64(p.bitrate.Load()) / 8
	left := max(maxPacerDelay-age, pacerTick)
	return max(r, float64(queued)/left.Seconds())
}

func (p *pacer) run() {
	defer close(p.done)
	var budget float64
	last := p.now()
	for {
		p.mu.Lock()
		empty := len(p.queue) == 0
		p.mu.Unlock()
		if empty {
			select {
			case <-p.stop:
				return
			case <-p.wake:
			}
			budget, last = math.MaxFloat64, p.now() // an idle bucket is full
		}
		now := p.now()
		p.mu.Lock()
		if len(p.queue) == 0 { // woken by a push already sent
			p.mu.Unlock()
			continue
		}
		r := p.rate(p.queued, now.Sub(p.queue[0].enqueued))
		budget = min(budget+r*now.Sub(last).Seconds(), max(r*pacerBurst.Seconds(), 1500))
		last = now
		var out [][]byte
		var finished []*pacedFrame
		for budget > 0 && len(p.queue) > 0 {
			f := p.queue[0]
			b := f.packets[0]
			f.packets = f.packets[1:]
			out = append(out, b)
			budget -= float64(len(b))
			p.queued -= len(b)
			if len(f.packets) == 0 {
				p.queue = p.queue[1:]
				finished = append(finished, f)
			}
		}
		p.mu.Unlock()
		for _, b := range out {
			p.publish(b)
			p.packets.Add(1)
			p.sentBytes.Add(uint64(len(b)))
		}
		for _, f := range finished {
			p.frames.Add(1)
			p.lastDelay.Store(int64(p.now().Sub(f.enqueued)))
		}
		select {
		case <-p.stop:
			return
		default:
		}
		if len(out) == 0 || budget <= 0 {
			p.sleep(pacerTick)
		}
	}
}

// close stops the pacer; what is still queued is dropped.
func (p *pacer) close() {
	close(p.stop)
	<-p.done
}
