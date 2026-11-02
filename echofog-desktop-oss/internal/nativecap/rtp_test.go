package nativecap

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func annexB(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

func TestPacketizerKeyframe(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x1f, 0xac}
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	idr := append([]byte{0x65}, bytes.Repeat([]byte{0xab}, 5000)...)
	au := annexB(sps, pps, idr)
	if !hasIDR(au) {
		t.Fatal("hasIDR missed the IDR")
	}
	if got := annexBNALs(au); len(got) != 3 || !bytes.Equal(got[2], idr) {
		t.Fatalf("annexBNALs split it into %d units", len(got))
	}
	p := newPacketizer(1100)
	pkts := p.packets(au, 9000)
	if len(pkts) < 5 {
		t.Fatalf("%d packets for a 5 KB IDR", len(pkts))
	}
	var first rtp.Packet
	for i, b := range pkts {
		if len(b) > 1100 {
			t.Fatalf("packet %d is %d bytes, over the MTU", i, len(b))
		}
		var pk rtp.Packet
		if err := pk.Unmarshal(b); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = pk
		}
		if pk.Marker != (i == len(pkts)-1) {
			t.Fatalf("marker on packet %d of %d", i, len(pkts))
		}
		if pk.SequenceNumber != first.SequenceNumber+uint16(i) || pk.Timestamp != 9000 {
			t.Fatalf("packet %d: seq %d ts %d", i, pk.SequenceNumber, pk.Timestamp)
		}
	}
	if first.Payload[0]&0x1f != 24 {
		t.Fatalf("SPS and PPS should go first as a STAP-A, got NAL type %d", first.Payload[0]&0x1f)
	}
}

// fakeClock runs the pacer on simulated time: sleeping moves the clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type sentPacket struct {
	at   time.Time
	size int
}

func runPacer(t *testing.T, bitrate uint64, frames ...[][]byte) []sentPacket {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var mu sync.Mutex
	var sent []sentPacket
	total := 0
	for _, f := range frames {
		total += len(f)
	}
	all := make(chan struct{})
	p := newPacer(func(b []byte) {
		mu.Lock()
		sent = append(sent, sentPacket{clk.now(), len(b)})
		if len(sent) == total {
			close(all)
		}
		mu.Unlock()
	}, bitrate)
	p.now, p.sleep = clk.now, clk.sleep
	for _, f := range frames {
		p.push(f)
	}
	p.start()
	select {
	case <-all:
	case <-time.After(5 * time.Second):
		t.Fatalf("the pacer sent %d of %d packets", len(sent), total)
	}
	p.close()
	return sent
}

func packets(n, size int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = make([]byte, size)
	}
	return out
}

// A 1440p keyframe (about 200 KB) at 32 Mbit/s goes out over ~20 ms, never more than a few
// kilobytes in any millisecond, instead of in one burst.
func TestPacerSpreadsAKeyframe(t *testing.T) {
	sent := runPacer(t, 32_000_000, packets(180, 1100))
	span := sent[len(sent)-1].at.Sub(sent[0].at)
	if span < 15*time.Millisecond || span > 30*time.Millisecond {
		t.Fatalf("a 198 KB keyframe went out over %v, want about 20 ms", span)
	}
	perMs := map[int64]int{}
	for _, s := range sent {
		perMs[s.at.UnixMilli()] += s.size
	}
	for ms, b := range perMs {
		if b > 25_000 {
			t.Fatalf("%d bytes in the millisecond %d", b, ms)
		}
	}
}

// A keyframe of 200 ms of a 10 Mbit/s stream (250 KB) leaves at 2.5 times the bitrate, over
// about 80 ms, not in the 50 Mbit/s burst that draining it within 40 ms took.
func TestPacerSendsALargeKeyframeAtTheRateNotABurst(t *testing.T) {
	sent := runPacer(t, 10_000_000, packets(227, 1100))
	span := sent[len(sent)-1].at.Sub(sent[0].at)
	if span < 70*time.Millisecond || span > maxPacerDelay+5*time.Millisecond {
		t.Fatalf("a 250 KB keyframe went out over %v, want about 80 ms", span)
	}
	perMs := map[int64]int{}
	for _, s := range sent {
		perMs[s.at.UnixMilli()/10] += s.size
	}
	for ms, b := range perMs {
		if b > 40_000 { // 25 Mbit/s is 31 KB in 10 ms
			t.Fatalf("%d bytes in the 10 ms from %d", b, ms*10)
		}
	}
}

// An ordinary frame does not wait for the next tick, and a queue far over the bitrate still
// drains within maxPacerDelay.
func TestPacerSmallFrameAndBacklog(t *testing.T) {
	sent := runPacer(t, 32_000_000, packets(2, 1100))
	if d := sent[1].at.Sub(sent[0].at); d != 0 {
		t.Fatalf("a two-packet frame took %v", d)
	}
	sent = runPacer(t, 1_000_000, packets(400, 1100)) // 440 KB at a 1 Mbit/s target
	if span := sent[len(sent)-1].at.Sub(sent[0].at); span > maxPacerDelay+5*time.Millisecond {
		t.Fatalf("a 440 KB backlog took %v to drain", span)
	}
}
