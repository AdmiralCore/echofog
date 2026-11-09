package servernode

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/ratelimit"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
)

// Traffic is the byte counter of the node: everything since it started and the part of the last
// UTC day. It counts carrier bytes (encrypted), which is what the operator pays for.
type Traffic struct {
	In     uint64
	Out    uint64
	DayIn  uint64
	DayOut uint64
}

// trafficMeter counts bytes and rolls the day counter at the UTC date boundary. It is cheap on the
// hot path: two atomic adds per read or write.
type trafficMeter struct {
	in, out       atomic.Uint64
	dayIn, dayOut atomic.Uint64
	mu            sync.Mutex
	day           string
	now           func() time.Time
}

func newTrafficMeter() *trafficMeter {
	return &trafficMeter{now: time.Now}
}

func (m *trafficMeter) add(in, out int) {
	if in > 0 {
		m.in.Add(uint64(in))
	}
	if out > 0 {
		m.out.Add(uint64(out))
	}
	today := m.now().UTC().Format("2006-01-02")
	m.mu.Lock()
	if m.day != today {
		m.day = today
		m.dayIn.Store(0)
		m.dayOut.Store(0)
	}
	m.mu.Unlock()
	if in > 0 {
		m.dayIn.Add(uint64(in))
	}
	if out > 0 {
		m.dayOut.Add(uint64(out))
	}
}

func (m *trafficMeter) snapshot() Traffic {
	return Traffic{In: m.in.Load(), Out: m.out.Load(), DayIn: m.dayIn.Load(), DayOut: m.dayOut.Load()}
}

// gate is the server-side limit of the carriers: at most max live carriers and one shared
// bandwidth bucket (T12.4f). A zero max or rate means no limit.
type gate struct {
	meter *trafficMeter
	mu    sync.Mutex
	max   int
	live  int
	bw    *ratelimit.Bucket
	rate  float64
}

func newGate(l *LimitsSpec, meter *trafficMeter) *gate {
	g := &gate{meter: meter}
	if l != nil {
		g.max = l.Sessions
		if l.BandwidthMbps > 0 {
			g.rate = float64(l.BandwidthMbps) * 1e6 / 8
			g.bw = ratelimit.NewBucket(g.rate, maxFloat(g.rate/4, 64<<10))
		}
	}
	return g
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (g *gate) acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.max > 0 && g.live >= g.max {
		return false
	}
	g.live++
	return true
}

func (g *gate) release() {
	g.mu.Lock()
	if g.live > 0 {
		g.live--
	}
	g.mu.Unlock()
}

// wait spends n bytes of the shared bandwidth and sleeps as long as the bucket says, with a cap so
// one huge chunk does not stall a session for minutes.
func (g *gate) wait(n int) {
	if g.bw == nil || n <= 0 {
		return
	}
	if d := g.bw.Wait(float64(n)); d > 0 {
		if d > 5*time.Second {
			d = 5 * time.Second
		}
		time.Sleep(d)
	}
}

// countingSource wraps a ListenerSource so every carrier it accepts is counted and limited.
type countingSource struct {
	src  node.ListenerSource
	gate *gate
}

func (s countingSource) Listen(ctx context.Context) (carrier.Listener, error) {
	l, err := s.src.Listen(ctx)
	if err != nil {
		return nil, err
	}
	return &countingListener{Listener: l, gate: s.gate}, nil
}

type countingListener struct {
	carrier.Listener
	gate *gate
}

// wrapAccess wraps a listener the access manager builds (its invitations); a failure passes through.
func wrapAccess(l carrier.Listener, err error, g *gate) (carrier.Listener, error) {
	if err != nil {
		return nil, err
	}
	return &countingListener{Listener: l, gate: g}, nil
}

func (l *countingListener) Accept(ctx context.Context) (mux.Carrier, error) {
	for {
		c, err := l.Listener.Accept(ctx)
		if err != nil {
			return nil, err
		}
		if !l.gate.acquire() {
			_ = c.Close()
			continue
		}
		return &countingCarrier{Carrier: c, gate: l.gate}, nil
	}
}

type countingCarrier struct {
	mux.Carrier
	gate *gate
	once sync.Once
}

func (c *countingCarrier) done() { c.once.Do(c.gate.release) }

func (c *countingCarrier) Close() error {
	c.done()
	return c.Carrier.Close()
}

func (c *countingCarrier) Done() <-chan struct{} {
	d := c.Carrier.Done()
	go func() {
		<-d
		c.done()
	}()
	return d
}

func (c *countingCarrier) OpenStream(ctx context.Context) (mux.RawStream, error) {
	s, err := c.Carrier.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	return &countingStream{RawStream: s, gate: c.gate}, nil
}

func (c *countingCarrier) AcceptStream(ctx context.Context) (mux.RawStream, error) {
	s, err := c.Carrier.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &countingStream{RawStream: s, gate: c.gate}, nil
}

func (c *countingCarrier) SendDatagram(b []byte) error {
	c.gate.wait(len(b))
	err := c.Carrier.SendDatagram(b)
	if err == nil {
		c.gate.meter.add(0, len(b))
	}
	return err
}

func (c *countingCarrier) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	b, err := c.Carrier.ReceiveDatagram(ctx)
	if err == nil {
		c.gate.meter.add(len(b), 0)
	}
	return b, err
}

type countingStream struct {
	mux.RawStream
	gate *gate
}

func (s *countingStream) Read(p []byte) (int, error) {
	n, err := s.RawStream.Read(p)
	s.gate.meter.add(n, 0)
	if n > 0 {
		s.gate.wait(n)
	}
	return n, err
}

func (s *countingStream) Write(p []byte) (int, error) {
	s.gate.wait(len(p))
	n, err := s.RawStream.Write(p)
	s.gate.meter.add(0, n)
	return n, err
}
