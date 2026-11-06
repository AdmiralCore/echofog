package censor

import (
	"math/rand"
	"net/netip"
	"time"
)

// The generators below are the lab's synthetic traffic. They are written from how the traffic is
// built (TLS 1.3 record overheads, HTTP/2 exchanges, a proxy's far-side latency), never fitted to a
// detector's thresholds.

const recOverhead = 22 // 5 byte header + 16 byte tag + 1 byte inner type

type synthKind int

const (
	kBrowser synthKind = iota
	kTunnel
	kTunnelEdge // a tunnel whose exit answers from the edge: no far-side latency
	kConstSize
	kBulk
)

type pktBuilder struct {
	f        Flow
	t        time.Duration
	seqUp    uint32
	seqDn    uint32
	rng      *rand.Rand
	rtt      time.Duration // outer round trip: border to server and back
	think    time.Duration
	recBytes int
}

func (b *pktBuilder) pkt(up bool, payload int) {
	p := Pkt{At: b.t, Up: up, Payload: payload}
	if up {
		p.Seq, p.Ack = b.seqUp, b.seqDn
		b.seqUp += uint32(payload)
	} else {
		p.Seq, p.Ack = b.seqDn, b.seqUp
		b.seqDn += uint32(payload)
	}
	b.f.Pkts = append(b.f.Pkts, p)
}

// record adds one TLS record (wire length n, header included) split into segments of 1448 bytes.
func (b *pktBuilder) record(up bool, typ byte, n int) {
	b.f.Records = append(b.f.Records, Record{At: b.t, Up: up, Type: typ, Len: n})
	for n > 0 {
		seg := min(n, 1448)
		b.pkt(up, seg)
		n -= seg
		b.t += 30 * time.Microsecond
	}
	if up {
		b.f.BytesUp += n
	}
}

// ack adds the server's pure ACK of the client's last data one rtt after it.
func (b *pktBuilder) ack(afterUp time.Duration) {
	b.t = afterUp + b.rtt
	b.pkt(false, 0)
}

// reaction is how long a client takes to answer a flight it has just received: the border sits on
// the client's side, so this is processing time, not a round trip.
func (b *pktBuilder) reaction() time.Duration {
	return time.Duration(rngSize(b.rng, 1, 8)) * time.Millisecond
}

func (b *pktBuilder) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*b.rng.Float64()))
}

func rngSize(rng *rand.Rand, lo, hi int) int { return lo + rng.Intn(hi-lo+1) }

// synthFlow builds one flow. i makes the client port unique.
func synthFlow(kind synthKind, seed int64, i int) LabeledFlow {
	rng := rand.New(rand.NewSource(seed))
	b := &pktBuilder{rng: rng, rtt: time.Duration(rngSize(rng, 10, 60)) * time.Millisecond}
	b.f = Flow{
		Proto: "tcp", Client: netip.AddrPortFrom(netip.MustParseAddr("203.0.113.21"), uint16(20000+i)),
		Server: netip.AddrPortFrom(netip.MustParseAddr("203.0.113.15"), 443), Looks: "TLS",
	}
	lf := LabeledFlow{}
	handshake := func() {
		b.record(true, 22, 517+rng.Intn(3)) // ClientHello
		b.t += b.rtt
		b.record(false, 22, 122)                      // ServerHello
		b.record(false, 20, 6)                        // CCS
		b.record(false, 23, rngSize(rng, 2400, 4600)) // encrypted extensions, certificate, finished
		b.t += b.reaction()
		b.record(true, 20, 6)
		b.record(true, 23, 74) // Finished
	}
	switch kind {
	case kBrowser:
		lf.Label = LabelBrowser
		handshake()
		n := rngSize(rng, 2, 12)
		for k := 0; k < n; k++ {
			b.t += time.Duration(rngSize(rng, 5, 400)) * time.Millisecond
			sent := b.t
			b.record(true, 23, rngSize(rng, 80, 700)+recOverhead)
			b.ack(sent)
			b.t = sent + b.rtt + b.jitter(time.Duration(rngSize(rng, 4, 40))*time.Millisecond) // origin think time
			b.record(false, 23, rngSize(rng, 40, 120))
			for r := 0; r < rngSize(rng, 1, 4); r++ {
				b.record(false, 23, rngSize(rng, 300, 16389))
			}
			if rng.Float64() >= 0.4 { // a WINDOW_UPDATE; otherwise the next request comes after the page is read
				b.t += b.reaction()
				b.record(true, 23, rngSize(rng, 33, 60))
			}
		}
	case kTunnel, kTunnelEdge:
		lf.Label, lf.Suite = LabelCarrier, SuiteTLSInTLS
		lf.Synthetic = true
		handshake()
		far := time.Duration(rngSize(rng, 30, 160)) * time.Millisecond
		if kind == kTunnelEdge {
			far = 0 // the exit answers from the edge: only the think time a site has, which is added below
		}
		b.t += time.Duration(rngSize(rng, 20, 120)) * time.Millisecond
		// the inner handshake: ClientHello, ServerHello + the rest of the flight, client Finished
		sent := b.t
		b.record(true, 23, rngSize(rng, 517, 1750)+recOverhead)
		b.ack(sent)
		b.t = sent + b.rtt + far
		b.record(false, 23, 122+recOverhead)
		b.record(false, 23, rngSize(rng, 2400, 4600)+recOverhead)
		b.t += b.reaction()
		b.record(true, 23, rngSize(rng, 70, 110)+recOverhead) // inner CCS + Finished
		n := rngSize(rng, 2, 10)
		for k := 0; k < n; k++ {
			b.t += time.Duration(rngSize(rng, 5, 300)) * time.Millisecond
			sent := b.t
			b.record(true, 23, rngSize(rng, 100, 700)+2*recOverhead)
			b.ack(sent)
			b.t = sent + b.rtt + far + b.jitter(time.Duration(rngSize(rng, 4, 40))*time.Millisecond)
			for r := 0; r < rngSize(rng, 1, 4); r++ {
				b.record(false, 23, rngSize(rng, 300, 16389))
			}
			b.record(true, 23, rngSize(rng, 33, 60))
		}
	case kConstSize:
		lf.Label, lf.Suite = LabelCarrier, SuiteConstSize
		lf.Synthetic = true
		handshake()
		for k := 0; k < 60; k++ {
			b.t += 20 * time.Millisecond
			b.record(k%2 == 0, 23, 1400)
		}
	case kBulk:
		lf.Label, lf.Suite = LabelCarrier, SuiteBulkTunnel
		lf.Synthetic = true
		handshake()
		for k := 0; k < 40; k++ {
			b.t += 15 * time.Second
			b.record(false, 23, 16389)
			b.record(true, 23, 40)
		}
		b.f.BytesDn = 60 << 20
		b.f.BytesUp = 1 << 20
	}
	b.f.Duration = b.t
	if b.f.BytesDn == 0 {
		for _, p := range b.f.Pkts {
			if p.Up {
				b.f.BytesUp += p.Payload + 52
			} else {
				b.f.BytesDn += p.Payload + 52
			}
		}
	}
	lf.Flow = b.f
	return lf
}

// synthSet builds n flows of a kind with seeds derived from base.
func synthSet(kind synthKind, base int64, n int) []LabeledFlow {
	out := make([]LabeledFlow, n)
	for i := range out {
		out[i] = synthFlow(kind, base+int64(i)*7919, i)
	}
	return out
}

// stormFlows builds n short flows from one client to the same server address.
func stormFlows(n int) []LabeledFlow {
	out := make([]LabeledFlow, n)
	for i := range out {
		f := synthFlow(kBrowser, int64(i+1), 1000+i)
		f.Label, f.Suite, f.Synthetic = LabelCarrier, SuiteConnStorm, true
		f.Client = netip.AddrPortFrom(netip.MustParseAddr("203.0.113.99"), uint16(30000+i))
		f.Server = netip.AddrPortFrom(netip.MustParseAddr("203.0.113.77"), 443)
		f.Start = time.Unix(1_700_000_000, 0).Add(time.Duration(i) * 300 * time.Millisecond)
		out[i] = f
	}
	return out
}
