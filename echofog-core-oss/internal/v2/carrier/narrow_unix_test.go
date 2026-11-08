//go:build linux || darwin || freebsd

package carrier

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// maxSeg reads the MSS the kernel settled on for a connected TCP socket.
func maxSeg(t *testing.T, c net.Conn) int {
	t.Helper()
	if u, ok := c.(interface{ NetConn() net.Conn }); ok {
		c = u.NetConn()
	}
	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var gerr error
	if err := rc.Control(func(fd uintptr) { v, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG) }); err != nil || gerr != nil {
		t.Fatalf("TCP_MAXSEG: %v %v", err, gerr)
	}
	return v
}

// TestRealSocketMSS checks the default opener on real sockets: the first attempt on an unmarked
// network has the system MSS (the SYN carries no limit), the opener of a marked network limits the
// MSS before connect (T3.12).
func TestRealSocketMSS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	addr := ln.Addr().String()
	probe := func(narrow *NarrowPath) int {
		seen := -1
		req := DialRequest{Narrow: narrow, NetworkID: func() string { return "n1" }}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := WithNarrowRetry(ctx, req, func(ctx context.Context, r DialRequest) (mux.Carrier, error) {
			conn, err := r.DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			seen = maxSeg(t, conn)
			_ = conn.Close()
			return newTestCarrier(), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		return seen
	}
	narrow := NewNarrowPath(systemClock{}, nil)
	if got := probe(narrow); got <= 1240 {
		t.Fatalf("an unmarked network's first socket has MSS %d: the SYN would carry a limit", got)
	}
	narrow.Mark("n1", ReasonMSS)
	if got := probe(narrow); got > 1240 || got < 500 {
		t.Fatalf("a marked network's socket has MSS %d, want at most 1240", got)
	}
}
