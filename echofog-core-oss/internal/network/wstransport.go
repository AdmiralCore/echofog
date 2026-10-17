package network

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/gorilla/websocket"
)

// WSTransport carries peer connections inside WebSocket frames, so a node can sit behind a CDN
// such as Cloudflare: the CDN terminates the outer HTTPS and forwards the WebSocket to the origin.
// Inside the WebSocket runs the same stack as the TCP fallback — a TLS 1.3 session of ours (with
// the REALITY token in the ClientHello) and then smux — so the CDN and anyone on the CDN-to-origin
// link see only TLS-looking bytes, never the protocol, and a stranger is still handed to the real
// site. The outer HTTPS to the CDN uses the CDN's own certificate for the domain.
type WSTransport struct {
	cfg       *mimicry.MimicryConfig
	serverTLS *tls.Config
	auth      PeerAuthenticator
	path      string

	perNode bool
	secret  string
	own     types.PeerID
	book    peerBook

	connChan chan NewConnEvent
	ctx      context.Context
	cancel   context.CancelFunc

	mu     sync.Mutex
	ln     net.Listener
	srv    *http.Server
	closed bool
	logger *slog.Logger

	upgrader websocket.Upgrader
}

// NewWSTransport makes a WebSocket transport that upgrades requests on path. cert is presented only
// to a client that reaches the origin directly over HTTPS (behind a CDN the CDN's certificate is
// used instead); inside, the mimicry config drives the inner TLS and token.
func NewWSTransport(cert tls.Certificate, cfg *mimicry.MimicryConfig, auth PeerAuthenticator, path string) *WSTransport {
	if path == "" {
		path = "/"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &WSTransport{
		cfg:       cfg,
		serverTLS: mimicry.BuildServerTLSConfig(cert),
		auth:      auth,
		path:      path,
		connChan:  make(chan NewConnEvent, 32),
		ctx:       ctx,
		cancel:    cancel,
		logger:    slog.Default().With("component", "ws_transport"),
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   32 << 10,
			WriteBufferSize:  32 << 10,
			CheckOrigin:      func(*http.Request) bool { return true },
		},
	}
}

func (t *WSTransport) SetLogger(l *slog.Logger) {
	if l != nil {
		t.logger = l
	}
}

func (t *WSTransport) SetNodeKeys(secret string, own types.PeerID) {
	c := *t.cfg
	c.AuthKey = mimicry.NodeStealthKey(secret, own)
	t.cfg = &c
	t.secret = secret
	t.own = own
	t.perNode = true
}

func (t *WSTransport) ExpectPeer(addr string, id types.PeerID) { t.book.set(addr, id) }

func (t *WSTransport) SetCaps(c handshake.Capability) {
	if s, ok := t.auth.(CapsSetter); ok {
		s.SetCaps(c)
	}
}

func (t *WSTransport) ConnChan() <-chan NewConnEvent { return t.connChan }

func (t *WSTransport) Addr() net.Addr {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln == nil {
		return nil
	}
	return t.ln.Addr()
}

// Listen serves the WebSocket endpoint on addr. Requests that are not a WebSocket upgrade on the
// configured path get a plain 404, like an ordinary web server.
func (t *WSTransport) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(t.path, t.handleRequest)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	t.mu.Lock()
	t.ln = ln
	t.srv = srv
	t.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (t *WSTransport) handleRequest(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.NotFound(w, r)
		return
	}
	ws, err := t.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	go t.handleInbound(newWSConn(ws))
}

func (t *WSTransport) handleInbound(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(tcpHandshakeTimeout))
	inner, isPeer, binding, err := mimicry.ServerHandshake(conn, t.serverTLS, t.cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	if !isPeer {
		// a visitor without a valid token is forwarded to the real site with an idle timeout, as a
		// genuine connection would be kept; never counted or banned (the WS transport has no Guard)
		forwardToRealSite(inner, t.cfg)
		return
	}
	mc, err := NewSmuxConn(t.ctx, inner, binding, false)
	if err != nil {
		_ = inner.Close()
		return
	}
	res, err := t.auth.AuthenticateInbound(t.ctx, mc)
	if err != nil {
		_ = mc.CloseWithError(ErrCodeAuthFailed, "auth failed")
		return
	}
	_ = conn.SetDeadline(time.Time{})
	// a WebSocket peer's source is the CDN or an ephemeral port; it is not dialable back as ws
	t.emit(NewConnEvent{Conn: mc, IsOut: false, PeerID: res.PeerID, PeerPubKey: res.PubKey, Addr: conn.RemoteAddr().String(), Dialable: false, Caps: res.Caps})
	t.logger.Info("Peer authenticated over WebSocket", "peer_id", res.PeerID, "direction", "inbound")
}

// Dial connects to a peer at a ws:// or wss:// URL (the peer ID is kept via ExpectPeer, as for
// TCP). The CDN handles the outer HTTPS; the inner TLS + token run over the WebSocket.
func (t *WSTransport) Dial(ctx context.Context, url string) error {
	if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		return errors.New("ws transport needs a ws:// or wss:// address")
	}
	expected, haveExpected := t.book.get(url)
	if t.perNode && !haveExpected {
		return ErrPeerUnknown
	}
	dialer := websocket.Dialer{HandshakeTimeout: tcpHandshakeTimeout, NetDialContext: DirectDialer(0).DialContext}
	ws, _, err := dialer.DialContext(ctx, url, http.Header{})
	if err != nil {
		return err
	}
	conn := newWSConn(ws)
	deadline := time.Now().Add(tcpHandshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	clientCfg := t.cfg
	if t.perNode {
		c := *t.cfg
		c.AuthKey = mimicry.NodeStealthKey(t.secret, expected)
		clientCfg = &c
	}
	inner, binding, err := mimicry.ClientHandshake(conn, clientCfg)
	if err != nil {
		_ = conn.Close()
		return err
	}
	mc, err := NewSmuxConn(t.ctx, inner, binding, true)
	if err != nil {
		_ = inner.Close()
		return err
	}
	res, err := t.auth.AuthenticateOutbound(ctx, mc)
	if err != nil {
		_ = mc.CloseWithError(ErrCodeAuthFailed, "auth failed")
		return err
	}
	if haveExpected && res.PeerID != expected {
		_ = mc.CloseWithError(ErrCodeAuthFailed, "unexpected peer")
		return ErrPeerMismatch
	}
	_ = conn.SetDeadline(time.Time{})
	t.emit(NewConnEvent{Conn: mc, IsOut: true, PeerID: res.PeerID, PeerPubKey: res.PubKey, Addr: url, Dialable: true, Caps: res.Caps})
	t.logger.Info("Peer authenticated over WebSocket", "peer_id", res.PeerID, "direction", "outbound", "url", url)
	return nil
}

func (t *WSTransport) emit(ev NewConnEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = ev.Conn.CloseWithError(ErrCodeNormalClose, "shutting down")
		return
	}
	select {
	case t.connChan <- ev:
	default:
		go func() {
			select {
			case t.connChan <- ev:
			case <-t.ctx.Done():
				_ = ev.Conn.CloseWithError(ErrCodeNormalClose, "shutting down")
			}
		}()
	}
}

func (t *WSTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	srv := t.srv
	t.mu.Unlock()
	t.cancel()
	if srv != nil {
		return srv.Close()
	}
	return nil
}
