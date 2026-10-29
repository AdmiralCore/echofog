package network

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// rotLab: a rotConn client and server over plain TCP on loopback, every carrier counted, a freeze
// at limit bytes of what a server sends on one connection (0: none).
type rotLab struct {
	t       *testing.T
	ln      net.Listener
	mu      sync.Mutex
	server  *rotConn
	ccs     []*countConn
	servers chan *rotConn
	client  *rotConn
	opened  bool
}

func newRotLab(t *testing.T, budget int) *rotLab {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &rotLab{t: t, ln: ln, servers: make(chan *rotConn, 1)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go l.accept(c, budget)
		}
	}()
	var id [16]byte
	_, _ = rand.Read(id[:])
	dial := func() (*rotCarrier, error) {
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return nil, err
		}
		cc := newCountConn(raw)
		l.mu.Lock()
		l.ccs = append(l.ccs, cc)
		l.mu.Unlock()
		flags := byte(0)
		l.mu.Lock()
		if !l.opened {
			flags, l.opened = rotFlagNew, true
		}
		l.mu.Unlock()
		if _, err := cc.Write(rotAttachFrame(id, flags)); err != nil {
			return nil, err
		}
		return newRotCarrier(cc, cc, nil), nil
	}
	first, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	client := newRotConn(true, id, rotOptions{Budget: budget, Dial: func(_ context.Context) (*rotCarrier, error) { return dial() }})
	client.addCarrier(first)
	l.client = client
	select {
	case srv := <-l.servers:
		l.mu.Lock()
		l.server = srv
		l.mu.Unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("no server session")
	}
	t.Cleanup(func() { l.client.Close(); l.server.Close() })
	return l
}

func (l *rotLab) accept(raw net.Conn, budget int) {
	cc := newCountConn(raw)
	l.mu.Lock()
	l.ccs = append(l.ccs, cc)
	l.mu.Unlock()
	br := bufio.NewReader(cc)
	if b, err := br.Peek(1); err != nil || b[0] != rotAttach {
		raw.Close()
		return
	}
	id, flags, err := readRotAttach(br)
	if err != nil {
		raw.Close()
		return
	}
	c := newRotCarrier(cc, cc, br)
	if flags&rotFlagNew != 0 {
		s := newRotConn(false, id, rotOptions{Budget: budget})
		s.addCarrier(c)
		l.servers <- s
		return
	}
	l.mu.Lock()
	s := l.server
	l.mu.Unlock()
	for s == nil { // a spare can arrive before the test has taken the session off the channel
		time.Sleep(time.Millisecond)
		l.mu.Lock()
		s = l.server
		l.mu.Unlock()
	}
	s.addCarrier(c)
}

func (l *rotLab) maxCarrierBytes() (tx, rx int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.ccs {
		tx, rx = max(tx, c.tx.Load()), max(rx, c.rx.Load())
	}
	return
}

func pump(t *testing.T, w io.Writer, data []byte, chunk int) {
	for len(data) > 0 {
		n := min(chunk, len(data))
		if _, err := w.Write(data[:n]); err != nil {
			t.Errorf("write: %v", err)
			return
		}
		data = data[n:]
	}
}

func readAll(t *testing.T, r io.Reader, n int) []byte {
	got := make([]byte, n)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Errorf("read: %v", err)
	}
	return got
}

func TestRotConn_BothWaysIntactUnderBudget(t *testing.T) {
	const budget = 6 << 10
	l := newRotLab(t, budget)
	a, b := make([]byte, 3<<20), make([]byte, 1<<20)
	_, _ = rand.Read(a)
	_, _ = rand.Read(b)
	var wg sync.WaitGroup
	var gotA, gotB []byte
	wg.Add(4)
	go func() { defer wg.Done(); pump(t, l.server, a, 7000) }()
	go func() { defer wg.Done(); pump(t, l.client, b, 1500) }()
	go func() { defer wg.Done(); gotA = readAll(t, l.client, len(a)) }()
	go func() { defer wg.Done(); gotB = readAll(t, l.server, len(b)) }()
	wg.Wait()
	if !bytes.Equal(gotA, a) || !bytes.Equal(gotB, b) {
		t.Fatal("bytes changed on the way")
	}
	tx, rx := l.maxCarrierBytes()
	if tx > budget || rx > budget {
		t.Fatalf("a carrier carried %d out, %d in; budget %d", tx, rx, budget)
	}
	l.mu.Lock()
	n := len(l.ccs)
	l.mu.Unlock()
	if n < 100 {
		t.Fatalf("only %d carriers for 4 MB: it did not rotate", n)
	}
}

func TestRotConn_CarriersKilledMidTransfer(t *testing.T) {
	l := newRotLab(t, 8<<10)
	data := make([]byte, 2<<20)
	_, _ = rand.Read(data)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-time.After(15 * time.Millisecond):
			case <-l.client.stop:
				return
			}
			l.mu.Lock()
			if len(l.ccs) > 2 {
				k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(l.ccs))))
				_ = l.ccs[k.Int64()].Conn.Close() // an abrupt end, in the middle of whatever it carried
			}
			l.mu.Unlock()
		}
	}()
	var wg sync.WaitGroup
	var got []byte
	wg.Add(2)
	go func() { defer wg.Done(); pump(t, l.server, data, 3000) }()
	go func() { defer wg.Done(); got = readAll(t, l.client, len(data)) }()
	wg.Wait()
	l.client.Close()
	<-done
	if !bytes.Equal(got, data) {
		t.Fatal("bytes lost or changed when carriers died")
	}
}

func TestRotConn_CloseReachesThePeer(t *testing.T) {
	l := newRotLab(t, 6<<10)
	go func() { _, _ = l.server.Write([]byte("bye")); l.server.Close() }()
	b, err := io.ReadAll(l.client)
	if err != nil || string(b) != "bye" {
		t.Fatalf("read %q, %v", b, err)
	}
}

func TestRotConn_Deadline(t *testing.T) {
	l := newRotLab(t, 6<<10)
	_ = l.client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := l.client.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	_ = l.client.SetReadDeadline(time.Time{})
	go func() { _, _ = l.server.Write([]byte("x")) }()
	if _, err := l.client.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
}

// TestRotConn_FreezeCannotCatchIt: a plain connection stops after 16 KB of the server's bytes (the
// lab's freeze); a rotating one, over carriers that are cut at that count, carries on.
func TestRotConn_FreezeCannotCatchIt(t *testing.T) {
	const freeze = 16 << 10
	l := newRotLab(t, 12<<10)
	data := make([]byte, 1<<20)
	_, _ = rand.Read(data)
	go pump(t, l.server, data, 5000)
	got := readAll(t, l.client, len(data))
	if !bytes.Equal(got, data) {
		t.Fatal("bytes changed")
	}
	// the server side of every carrier stayed under the count the freeze works at
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.ccs {
		if c.rx.Load() > freeze && c.tx.Load() > freeze {
			t.Fatalf("a carrier went past the freeze count: in %d out %d", c.rx.Load(), c.tx.Load())
		}
	}
}
