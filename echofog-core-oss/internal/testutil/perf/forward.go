package perf

import (
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type delayed struct {
	b   []byte
	due time.Time
}

// UDPPath stands between one client and a UDP server on loopback, like a network path: every
// datagram is held for Delay each way, Loss of them are dropped, and Blackhole drops them all
// (a network that blocks UDP).
type UDPPath struct {
	Delay     time.Duration
	Loss      float64 // 0..1, each direction
	Blackhole atomic.Bool

	listen *net.UDPConn
	up     *net.UDPConn
	server *net.UDPAddr
	mu     sync.Mutex
	client *net.UDPAddr
	toSrv  chan delayed
	toCli  chan delayed
	done   chan struct{}
	once   sync.Once
}

// NewUDPPath forwards to server; clients dial Addr().
func NewUDPPath(server string, delay time.Duration) (*UDPPath, error) {
	sa, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, err
	}
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	up, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		l.Close()
		return nil, err
	}
	for _, c := range []*net.UDPConn{l, up} {
		_ = c.SetReadBuffer(8 << 20)
		_ = c.SetWriteBuffer(8 << 20)
	}
	p := &UDPPath{Delay: delay, listen: l, up: up, server: sa,
		toSrv: make(chan delayed, 1<<16), toCli: make(chan delayed, 1<<16), done: make(chan struct{})}
	go p.read(l, p.toSrv, true)
	go p.read(up, p.toCli, false)
	go p.deliver(p.toSrv, func(b []byte) { _, _ = up.WriteToUDP(b, sa) })
	go p.deliver(p.toCli, func(b []byte) {
		p.mu.Lock()
		c := p.client
		p.mu.Unlock()
		if c != nil {
			_, _ = l.WriteToUDP(b, c)
		}
	})
	return p, nil
}

// Addr is where the client sends.
func (p *UDPPath) Addr() string { return p.listen.LocalAddr().String() }

func (p *UDPPath) read(c *net.UDPConn, out chan delayed, fromClient bool) {
	buf := make([]byte, 65536)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if fromClient {
			p.mu.Lock()
			p.client = from
			p.mu.Unlock()
		}
		if p.Blackhole.Load() || (p.Loss > 0 && rand.Float64() < p.Loss) {
			continue
		}
		select {
		case out <- delayed{append([]byte(nil), buf[:n]...), time.Now().Add(p.Delay)}:
		default: // a full queue drops, like a router's
		}
	}
}

func (p *UDPPath) deliver(in chan delayed, send func([]byte)) {
	for {
		select {
		case d := <-in:
			if w := time.Until(d.due); w > 0 {
				time.Sleep(w)
			}
			send(d.b)
		case <-p.done:
			return
		}
	}
}

// Close stops forwarding.
func (p *UDPPath) Close() {
	p.once.Do(func() {
		close(p.done)
		p.listen.Close()
		p.up.Close()
	})
}

// TCPPath is UDPPath for TCP: each accepted connection is joined to the server, and every chunk
// is held for Delay each way. Blackhole refuses new connections by holding them open silently.
type TCPPath struct {
	Delay     time.Duration
	Blackhole atomic.Bool

	ln     net.Listener
	server string
}

// NewTCPPath forwards to server; clients dial Addr().
func NewTCPPath(server string, delay time.Duration) (*TCPPath, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &TCPPath{Delay: delay, ln: ln, server: server}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.join(c)
		}
	}()
	return p, nil
}

// Addr is where the client dials.
func (p *TCPPath) Addr() string { return p.ln.Addr().String() }

// Close stops accepting.
func (p *TCPPath) Close() { p.ln.Close() }

func (p *TCPPath) join(c net.Conn) {
	if p.Blackhole.Load() {
		time.AfterFunc(30*time.Second, func() { c.Close() })
		_, _ = io.Copy(io.Discard, c)
		return
	}
	s, err := net.Dial("tcp", p.server)
	if err != nil {
		c.Close()
		return
	}
	go p.pipe(c, s)
	p.pipe(s, c)
}

func (p *TCPPath) pipe(from, to net.Conn) {
	defer to.Close()
	defer from.Close()
	q := make(chan delayed, 4096)
	go func() {
		defer close(q)
		buf := make([]byte, 64<<10)
		for {
			n, err := from.Read(buf)
			if n > 0 {
				q <- delayed{append([]byte(nil), buf[:n]...), time.Now().Add(p.Delay)}
			}
			if err != nil {
				return
			}
		}
	}()
	for d := range q {
		if w := time.Until(d.due); w > 0 {
			time.Sleep(w)
		}
		if _, err := to.Write(d.b); err != nil {
			return
		}
	}
}
