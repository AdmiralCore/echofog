package ws2

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// conn adapts a WebSocket to a net.Conn that carries a byte stream in binary messages: every
// write is cut into messages of at most MaxMessage bytes, and a message above that, or one that
// is not binary, ends the connection (wire.md §4.4).
type conn struct {
	ws     *websocket.Conn
	remote net.Addr
	onEnd  func()

	rmu sync.Mutex
	r   io.Reader
	got int // bytes of the current message read so far

	wmu sync.Mutex

	once sync.Once
}

// newConn wraps ws. remote, when not nil, replaces RemoteAddr (the client behind a CDN); onEnd,
// when not nil, runs once when the connection is closed.
func newConn(ws *websocket.Conn, remote net.Addr, onEnd func()) *conn {
	return &conn{ws: ws, remote: remote, onEnd: onEnd}
}

var (
	errNotBinary = errors.New("ws2: a message that is not binary")
	errTooBig    = errors.New("ws2: a message above 16 KiB")
)

// Read returns the bytes of binary messages. A violation (a text message, a message above
// MaxMessage) is an error and nothing else: this layer writes no close frame, so that a stranger's
// connection ends only through the listener's one refusal (wire.md §4.4, AD20); an admitted
// connection simply closes.
func (c *conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.r == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			if mt != websocket.BinaryMessage {
				return 0, errNotBinary
			}
			c.r, c.got = r, 0
		}
		n, err := c.r.Read(p)
		c.got += n
		if c.got > MaxMessage {
			return 0, errTooBig
		}
		if errors.Is(err, io.EOF) {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue // an empty message carries no bytes
		}
		return n, err
	}
}

func (c *conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n := 0
	for len(p) > 0 {
		k := min(len(p), MaxMessage)
		if err := c.ws.WriteMessage(websocket.BinaryMessage, p[:k]); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}

func (c *conn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.ws.Close()
		if c.onEnd != nil {
			c.onEnd()
		}
	})
	return err
}

// refuse ends the WebSocket the way a back end that does not understand a message does: close
// code 1008 (policy violation). It waits briefly for the peer's close, so that the frame is not
// lost to a reset, then drops the connection.
func (c *conn) refuse() {
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, ""), time.Now().Add(time.Second))
	_ = c.ws.SetReadDeadline(time.Now().Add(time.Second))
	c.ws.SetReadLimit(MaxMessage)
	for {
		if _, _, err := c.ws.ReadMessage(); err != nil {
			break
		}
	}
	_ = c.Close()
}

func (c *conn) LocalAddr() net.Addr { return c.ws.LocalAddr() }

func (c *conn) RemoteAddr() net.Addr {
	if c.remote != nil {
		return c.remote
	}
	return c.ws.RemoteAddr()
}

func (c *conn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
func (c *conn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *conn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
