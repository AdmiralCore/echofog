// Package memcarrier provides an in-memory pair of connected mux.Carrier values for session and
// service tests (E2, T2.1). It has two modes:
//
//   - NewQUIC: native streams and datagrams, like a QUIC carrier. Stream IDs follow QUIC:
//     the client opens 0, 4, 8, … and the server 1, 5, 9, …
//   - NewBytes: two mux.Conn instances over a byte pipe, like reality2-tcp and ws2.
//
// Datagram loss, delivery latency and an abrupt disconnect are controllable at run time. Streams
// are always reliable: loss applies to datagrams only, because a carrier's stream layer sits
// above its own retransmission and must not drop bytes.
package memcarrier

import (
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Mode selects the in-memory carrier shape.
type Mode uint8

const (
	// QUIC is the native-stream, native-datagram mode.
	QUIC Mode = iota
	// Byte is the byte-stream mux mode used by TCP carriers and ws2.
	Byte
)

// ErrClosed is returned by a carrier after a drop or close.
var ErrClosed = errors.New("memcarrier: closed")

// Pair is one connected client/server carrier pair.
type Pair struct {
	Client mux.Carrier
	Server mux.Carrier

	loss    *atomic.Uint64 // math.Float64bits of the datagram drop probability
	latency *atomic.Int64  // nanoseconds
	drop    func()
}

type config struct {
	cb         [32]byte
	cbSet      bool
	sealed     bool
	loss       float64
	latency    time.Duration
	dgramQueue int
}

// Option configures an in-memory pair.
type Option func(*config)

// WithChannelBinding fixes the channel binding both sides report. Without it a random value is
// generated per pair.
func WithChannelBinding(cb [32]byte) Option {
	return func(c *config) {
		c.cb = cb
		c.cbSet = true
	}
}

// WithSealed makes both sides report sealed mode (wire.md §8.5).
func WithSealed(sealed bool) Option {
	return func(c *config) { c.sealed = sealed }
}

// WithLoss sets the datagram drop probability in [0, 1]. Loss never applies to streams.
func WithLoss(p float64) Option {
	return func(c *config) { c.loss = clampLoss(p) }
}

// WithLatency delays every delivery from either side.
func WithLatency(d time.Duration) Option {
	return func(c *config) {
		if d < 0 {
			d = 0
		}
		c.latency = d
	}
}

// WithDatagramQueue sets the receive datagram queue length (default 64).
func WithDatagramQueue(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.dgramQueue = n
		}
	}
}

// New creates a connected pair in the given mode.
func New(mode Mode, opts ...Option) *Pair {
	cfg := config{dgramQueue: 64, loss: 0}
	for _, o := range opts {
		o(&cfg)
	}
	if !cfg.cbSet {
		cfg.cb = randomBinding()
	}
	loss := &atomic.Uint64{}
	loss.Store(math.Float64bits(clampLoss(cfg.loss)))
	latency := &atomic.Int64{}
	latency.Store(int64(cfg.latency))

	if mode == Byte {
		return newBytePair(cfg, loss, latency)
	}
	return newNativePair(cfg, loss, latency)
}

// NewQUIC returns a native-stream pair.
func NewQUIC(opts ...Option) *Pair { return New(QUIC, opts...) }

// NewBytes returns a byte-stream mux pair.
func NewBytes(opts ...Option) *Pair { return New(Byte, opts...) }

// SetLoss changes the datagram drop probability at run time.
func (p *Pair) SetLoss(v float64) { p.loss.Store(math.Float64bits(clampLoss(v))) }

// SetLatency changes the delivery latency at run time.
func (p *Pair) SetLatency(d time.Duration) {
	if d < 0 {
		d = 0
	}
	p.latency.Store(int64(d))
}

// Drop disconnects both sides abruptly: pending reads and writes fail and Done closes.
func (p *Pair) Drop() { p.drop() }

// Close closes both sides gracefully.
func (p *Pair) Close() error {
	err := p.Client.Close()
	if serr := p.Server.Close(); err == nil {
		err = serr
	}
	return err
}

func clampLoss(p float64) float64 {
	switch {
	case p != p || p < 0: // NaN or negative
		return 0
	case p > 1:
		return 1
	}
	return p
}

func randomBinding() [32]byte {
	var b [32]byte
	if _, err := crand.Read(b[:]); err == nil {
		return b
	}
	binary.LittleEndian.PutUint64(b[0:8], mrand.Uint64())
	binary.LittleEndian.PutUint64(b[8:16], uint64(time.Now().UnixNano()))
	binary.LittleEndian.PutUint64(b[16:24], mrand.Uint64())
	binary.LittleEndian.PutUint64(b[24:32], mrand.Uint64())
	return b
}

// lossyCarrier drops a fraction of outgoing datagrams; streams and everything else delegate.
type lossyCarrier struct {
	mux.Carrier
	loss *atomic.Uint64
}

func (c lossyCarrier) SendDatagram(d []byte) error {
	if p := math.Float64frombits(c.loss.Load()); p > 0 && mrand.Float64() < p {
		return nil
	}
	return c.Carrier.SendDatagram(d)
}

// delayRW delays every write and can be closed while sleeping.
type delayRW struct {
	rw      io.ReadWriteCloser
	latency *atomic.Int64

	once     sync.Once
	done     chan struct{}
	closeErr error
}

func (d *delayRW) delay() bool {
	n := d.latency.Load()
	if n <= 0 {
		return true
	}
	t := time.NewTimer(time.Duration(n))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-d.done:
		return false
	}
}

func (d *delayRW) Read(p []byte) (int, error) { return d.rw.Read(p) }

func (d *delayRW) Write(p []byte) (int, error) {
	if !d.delay() {
		return 0, ErrClosed
	}
	return d.rw.Write(p)
}

func (d *delayRW) Close() error {
	d.once.Do(func() {
		close(d.done)
		d.closeErr = d.rw.Close()
	})
	return d.closeErr
}
