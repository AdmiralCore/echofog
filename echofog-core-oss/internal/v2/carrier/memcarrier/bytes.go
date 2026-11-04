package memcarrier

import (
	"net"
	"sync/atomic"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// newBytePair builds the byte-stream mode: a reliable pipe under two mux.Conn instances.
func newBytePair(cfg config, loss *atomic.Uint64, latency *atomic.Int64) *Pair {
	c1, c2 := net.Pipe()
	d1 := &delayRW{rw: c1, latency: latency, done: make(chan struct{})}
	d2 := &delayRW{rw: c2, latency: latency, done: make(chan struct{})}

	client := mux.NewConn(d1, mux.ConnConfig{
		Client:         true,
		ChannelBinding: cfg.cb,
		Sealed:         cfg.sealed,
		DatagramQueue:  cfg.dgramQueue,
	})
	server := mux.NewConn(d2, mux.ConnConfig{
		Client:         false,
		ChannelBinding: cfg.cb,
		Sealed:         cfg.sealed,
		DatagramQueue:  cfg.dgramQueue,
	})
	return &Pair{
		Client:  lossyCarrier{Carrier: client, loss: loss},
		Server:  lossyCarrier{Carrier: server, loss: loss},
		loss:    loss,
		latency: latency,
		drop: func() {
			_ = d1.Close()
			_ = d2.Close()
		},
	}
}
