package selfcover

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
)

// Limits of wire.md §4.2.
const (
	// firstRecordTimeout is how long the front waits for the first TLS record.
	firstRecordTimeout = 10 * time.Second
	// maxFirstRecord is the largest first record read (16 KiB).
	maxFirstRecord = 16 << 10
	// DefaultMaxPreToken is the number of connections that may wait for their first record.
	DefaultMaxPreToken = 256
	// selectedBacklog is how many admitted connections may wait for Accept.
	selectedBacklog = 64
)

// front is the TCP side of the cover (wire.md §4.2). It reads the first TLS record of every
// connection. A valid token makes the connection ours: it is handed out as a tls.Conn of the
// very config the website uses. Anything else goes to the website with the bytes already read,
// in this process: there is no proxy hop, so the stranger gets the real site's behaviour.
type front struct {
	ln       net.Listener
	verifier *reality2.Verifier
	tlsCfg   *tls.Config
	now      func() time.Time
	timeout  time.Duration

	site     *connQueue
	selected chan net.Conn
	slots    chan struct{}

	wg        sync.WaitGroup
	closeOnce sync.Once
	done      chan struct{}
}

func newFront(ln net.Listener, v *reality2.Verifier, tlsCfg *tls.Config, site *connQueue, maxPre int) *front {
	if maxPre <= 0 {
		maxPre = DefaultMaxPreToken
	}
	f := &front{
		ln: ln, verifier: v, tlsCfg: tlsCfg, site: site, timeout: firstRecordTimeout,
		selected: make(chan net.Conn, selectedBacklog), slots: make(chan struct{}, maxPre), done: make(chan struct{}),
	}
	f.wg.Add(1)
	go f.acceptLoop()
	return f
}

func (f *front) acceptLoop() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handle(c)
		}()
	}
}

// handle decides one connection. The pre-token slot is held only while the first record is read.
func (f *front) handle(c net.Conn) {
	select {
	case f.slots <- struct{}{}:
	default:
		// Over the limit: straight to the website, unread, never reset (wire.md §4.2).
		f.toSite(c, nil)
		return
	}
	buf, hello := f.readFirst(c)
	<-f.slots // released as soon as the token is checked, whatever the answer

	if hello != nil && f.verifier.Verify(hello) {
		f.toSelected(&prefixConn{Conn: c, pre: buf})
		return
	}
	f.toSite(c, buf)
}

// readFirst reads up to the first TLS record and returns every byte read plus the ClientHello
// when the record is a complete one. Any shortfall (timeout, EOF, a record that is not a
// handshake) returns what was read and a nil hello: the connection is a stranger's.
func (f *front) readFirst(c net.Conn) (buf []byte, hello *reality2.Hello) {
	_ = c.SetReadDeadline(time.Now().Add(f.timeout))
	defer c.SetReadDeadline(time.Time{})
	var hdr [5]byte
	n, err := io.ReadFull(c, hdr[:])
	buf = append(buf, hdr[:n]...)
	if err != nil || hdr[0] != 0x16 {
		return buf, nil
	}
	l := int(hdr[3])<<8 | int(hdr[4])
	if l == 0 || l > maxFirstRecord {
		return buf, nil
	}
	body := make([]byte, l)
	n, err = io.ReadFull(c, body)
	buf = append(buf, body[:n]...)
	if err != nil {
		return buf, nil
	}
	if len(body) < 4 || body[0] != 1 {
		return buf, nil
	}
	msgLen := 4 + (int(body[1])<<16 | int(body[2])<<8 | int(body[3]))
	if msgLen > len(body) {
		return buf, nil // the hello spans records: not a hello of ours
	}
	h, err := reality2.ParseHelloTCP(body[:msgLen])
	if err != nil {
		return buf, nil
	}
	return buf, h
}

func (f *front) toSite(c net.Conn, pre []byte) {
	f.site.push(&prefixConn{Conn: c, pre: pre})
}

func (f *front) toSelected(c net.Conn) {
	select {
	case f.selected <- tls.Server(c, f.tlsCfg):
	case <-f.done:
		_ = c.Close()
	}
}

// accept returns the next admitted connection (a tls.Conn whose handshake has not run yet).
func (f *front) accept(done <-chan struct{}) (net.Conn, error) {
	select {
	case c := <-f.selected:
		return c, nil
	case <-done:
		return nil, errors.New("selfcover: accept cancelled")
	case <-f.done:
		return nil, net.ErrClosed
	}
}

func (f *front) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.ln.Close()
		f.site.close()
		f.wg.Wait()
		for {
			select {
			case c := <-f.selected:
				_ = c.Close()
			default:
				return
			}
		}
	})
}

// prefixConn replays bytes already read before the rest of the connection.
type prefixConn struct {
	net.Conn
	pre []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(p, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// connQueue is the net.Listener the website's http.Server serves: the front pushes strangers in.
type connQueue struct {
	addr net.Addr
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newConnQueue(addr net.Addr) *connQueue {
	return &connQueue{addr: addr, ch: make(chan net.Conn), done: make(chan struct{})}
}

func (q *connQueue) push(c net.Conn) {
	select {
	case q.ch <- c:
	case <-q.done:
		_ = c.Close()
	}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, net.ErrClosed
	}
}

func (q *connQueue) Close() error   { q.close(); return nil }
func (q *connQueue) close()         { q.once.Do(func() { close(q.done) }) }
func (q *connQueue) Addr() net.Addr { return q.addr }
