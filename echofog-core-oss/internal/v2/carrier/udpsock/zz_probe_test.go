package udpsock

import (
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

func soRcvBuf(t *testing.T, c *Conn) int {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var sz int
	if err := rc.Control(func(fd uintptr) {
		sz, err = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	}); err != nil {
		t.Fatal(err)
	}
	return sz
}

func TestProbeQueueCapacity(t *testing.T) {
	for _, want := range []int{104 << 10, 8 << 20} {
		recv := listen(t, Options{})
		if err := recv.SetReadBuffer(want); err != nil {
			t.Fatal(err)
		}
		t.Logf("asked %d, SO_RCVBUF=%d", want, soRcvBuf(t, recv))
		send := listen(t, Options{})
		dst := addrOf(t, recv)
		p := make([]byte, 1400)
		for i := 0; i < 400; i++ {
			if _, err := send.WriteTo(p, dst); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(100 * time.Millisecond)
		ms := messages(400, 2048)
		_ = recv.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := recv.ReadBatch(ms, 0)
		t.Logf("queued %d %v", n, err)
		if n > 0 {
			t.Logf("bytes of %d datagrams = %d", n, n*1400)
		}
	}
	_ = net.IPv4len
	_ = ipv4.FlagDst
}
