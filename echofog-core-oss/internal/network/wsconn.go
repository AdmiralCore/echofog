package network

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn turns a WebSocket connection into a plain byte-stream net.Conn, so the same inner TLS
// camouflage and smux that run over the TCP fallback run unchanged over a WebSocket. Each Write
// becomes one binary WebSocket message; reads reassemble the messages into a stream. A CDN such as
// Cloudflare forwards the WebSocket frames without seeing inside them, because inside is a TLS 1.3
// session of ours (see WSTransport).
type wsConn struct {
	ws *websocket.Conn

	rmu sync.Mutex
	r   io.Reader // the current message being read

	wmu sync.Mutex
}

func newWSConn(ws *websocket.Conn) *wsConn { return &wsConn{ws: ws} }

func (c *wsConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.r == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			if mt != websocket.BinaryMessage {
				continue // ignore pings/text; gorilla handles control frames itself
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error         { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }
func (c *wsConn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

// SetWriteDeadline takes the write lock: gorilla keeps the write deadline in a plain field that
// WriteMessage reads, so setting it while smux writes (the handshake deadline is cleared once the
// peer is authenticated) would race.
func (c *wsConn) SetWriteDeadline(t time.Time) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.SetWriteDeadline(t)
}
func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
