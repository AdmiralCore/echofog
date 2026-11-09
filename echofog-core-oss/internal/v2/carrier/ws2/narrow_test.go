package ws2

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// holeConn drops what the client writes while the socket's MSS is unlimited: a path that swallows
// the first big segments (T3.12).
type holeConn struct {
	net.Conn
	drop bool
}

func (c *holeConn) Write(p []byte) (int, error) {
	if c.drop {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// TestDialRetriesOnALimitedMSS: ws2 opens its TCP socket inside the WebSocket library; on a black
// hole the whole dial is repeated with a limited MSS, and the network is marked.
func TestDialRetriesOnALimitedMSS(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	var calls []int
	narrow := carrier.NewNarrowPath(nil, nil)
	narrow.FirstTry, narrow.Rung = 500*time.Millisecond, 500*time.Millisecond
	req := carrier.DialRequest{
		Card: r.card, Addr: r.addr, Narrow: narrow, NetworkID: func() string { return "n1" },
		DialMSS: func(ctx context.Context, network, addr string, mss int) (net.Conn, error) {
			mu.Lock()
			calls = append(calls, mss)
			mu.Unlock()
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &holeConn{Conn: c, drop: mss == 0}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() {
		if c, err := r.node.Accept(ctx); err == nil {
			defer c.Close()
		}
	}()
	c, err := r.dialer().Dial(ctx, req)
	if err != nil {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("Dial: %v (sockets %v)", err, calls)
	}
	defer c.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 || calls[0] != 0 || calls[1] != 1240 {
		t.Fatalf("sockets opened with MSS %v, want the first unlimited and the retry 1240", calls)
	}
	if !narrow.Marked("n1") {
		t.Fatal("the network was not marked")
	}
}
