package exit_gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/fakedns"
	"github.com/echofog/echofog-core/pkg/events"
)

var (
	ErrSocksAuthFailed     = errors.New("socks5 authentication failed")
	ErrSocksCommandNotSupp = errors.New("socks5 command not supported (CONNECT and UDP ASSOCIATE are supported)")
	ErrSocksAddressNotSupp = errors.New("socks5 address type not supported")
)

// StreamDialer abstracts tunneling a stream through P2P circuits to an Exit node.
type StreamDialer interface {
	DialExitStream(ctx context.Context, targetAddr string) (io.ReadWriteCloser, error)
}

// SOCKS5Server runs locally on 127.0.0.1:1080 to proxy external applications into the EchoFog P2P network.
type SOCKS5Server struct {
	mu         sync.Mutex
	listenAddr string
	listener   net.Listener
	dialer     StreamDialer // guarded by mu: SetDialer moves the proxy to another exit
	logger     *slog.Logger

	activeConns atomic.Int64
	bytesRx     atomic.Uint64
	bytesTx     atomic.Uint64
	emitter     atomic.Pointer[events.Emitter]
	fake        *fakedns.Mapper // set in whole-computer mode, guarded by mu

	refused      atomic.Uint64
	lastRefusal  atomic.Pointer[Refusal]
	refusalLog   *logLimiter
	refusalsOnce sync.Once
	health       streamHealth

	// caps on what local apps can hold at once: each connection or association costs a
	// stream to the exit, and the exit link carries a few thousand at most
	connSlots chan struct{}
	udpSlots  chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

const (
	maxSocksConns      = 2048
	maxSocksUDPAssocs  = 512
	socksHandshakeTime = 10 * time.Second
)

// NewSOCKS5Server creates a new local SOCKS5 proxy server.
func NewSOCKS5Server(listenAddr string, dialer StreamDialer) *SOCKS5Server {
	return &SOCKS5Server{
		listenAddr: listenAddr,
		dialer:     dialer,
		logger:     slog.Default().With("component", "socks5_server"),
		connSlots:  make(chan struct{}, maxSocksConns),
		udpSlots:   make(chan struct{}, maxSocksUDPAssocs),
	}
}

// Start begins listening on the configured local address.
func (s *SOCKS5Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("socks5 listen failed on %s: %w", s.listenAddr, err)
	}

	s.listener = l
	s.ctx, s.cancel = context.WithCancel(context.Background())

	go s.acceptLoop(s.ctx, l)

	s.logger.Info("SOCKS5 proxy server started", "addr", s.listener.Addr().String())
	return nil
}

// Addr returns the bound network address.
func (s *SOCKS5Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

// Stop gracefully shuts down the SOCKS5 server.
func (s *SOCKS5Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}

	if s.listener != nil {
		err := s.listener.Close()
		s.listener = nil
		return err
	}
	return nil
}

// SetDialer moves new connections to another exit. The listener (and a TUN on top of it) stays
// up, so applications do not notice; connections already open keep their old exit until they end.
func (s *SOCKS5Server) SetDialer(d StreamDialer) {
	s.mu.Lock()
	s.dialer = d
	s.mu.Unlock()
	s.health.reset()
}

func (s *SOCKS5Server) currentDialer() StreamDialer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dialer
}

// Stats returns current telemetry metrics.
func (s *SOCKS5Server) Stats() (active int64, rx uint64, tx uint64) {
	return s.activeConns.Load(), s.bytesRx.Load(), s.bytesTx.Load()
}

// acceptLoop serves l until the proxy stops. An accept error that is not the listener closing
// (too many open files, out of buffers) is waited out, like net/http does: the proxy must not
// die silently while the app and the TUN on top of it still count on it.
func (s *SOCKS5Server) acceptLoop(ctx context.Context, l net.Listener) {
	var backoff time.Duration
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff *= 2; backoff > time.Second {
				backoff = time.Second
			}
			s.logger.Warn("SOCKS5 accept error, retrying", "err", err, "in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			continue
		}
		backoff = 0

		select {
		case s.connSlots <- struct{}{}:
		default:
			_ = conn.Close() // far more connections than any app needs: refuse rather than fall over
			continue
		}
		s.activeConns.Add(1)
		go func(c net.Conn) {
			// stopping the proxy ends its connections too, so nothing keeps using (and paying
			// for) the exit after the VPN is off
			stop := context.AfterFunc(ctx, func() { _ = c.Close() })
			defer func() {
				stop()
				_ = c.Close()
				s.activeConns.Add(-1)
				<-s.connSlots
			}()
			s.handleClient(c)
		}(conn)
	}
}

func (s *SOCKS5Server) handleClient(clientConn net.Conn) {
	_ = clientConn.SetDeadline(time.Now().Add(socksHandshakeTime))

	// 1. SOCKS5 Negotiation / Auth handshake
	// [VER, NMETHODS, METHODS...]
	header := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, header); err != nil {
		return
	}

	if header[0] != 0x05 { // SOCKS version 5
		return
	}

	numMethods := int(header[1])
	methods := make([]byte, numMethods)
	if _, err := io.ReadFull(clientConn, methods); err != nil {
		return
	}

	// only NO AUTH (0x00); a client that does not offer it gets "no acceptable methods"
	if !bytes.Contains(methods, []byte{0x00}) {
		_, _ = clientConn.Write([]byte{0x05, 0xFF})
		return
	}
	if _, err := clientConn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// 2. Request Details
	// [VER 0x05, CMD, RSV 0x00, ATYP, DST.ADDR, DST.PORT]
	reqHeader := make([]byte, 3)
	if _, err := io.ReadFull(clientConn, reqHeader); err != nil || reqHeader[0] != 0x05 {
		return
	}
	targetAddr, rep, err := readSocksAddr(clientConn)
	if err != nil {
		if rep != 0 {
			s.sendReply(clientConn, rep)
		}
		return
	}

	switch reqHeader[1] {
	case 0x01: // CONNECT
	case 0x03: // UDP ASSOCIATE (the client's announced address is ignored: it is usually 0.0.0.0:0)
		if s.currentDialer() == nil {
			s.sendReply(clientConn, 0x01)
			return
		}
		s.handleUDPAssociate(clientConn)
		return
	default:
		s.sendReply(clientConn, 0x07) // Command not supported
		return
	}

	if fake := s.fakeDNS(); fake != nil {
		if isFakeResolver(targetAddr) {
			s.serveTCPDNS(clientConn, fake) // DNS over TCP to the TUN's resolver: answered here too
			return
		}
		t, ok := fake.TranslateHostPort(targetAddr)
		if !ok {
			s.sendReply(clientConn, 0x04) // Host unreachable: the fake IP is no longer mapped
			return
		}
		targetAddr = t
	}

	// Reset deadline for full stream duration
	_ = clientConn.SetDeadline(time.Time{})

	// 3. Dial through P2P Exit Gateway
	dialer := s.currentDialer()
	if dialer == nil {
		s.sendReply(clientConn, 0x01) // General failure
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	p2pStream, err := dialer.DialExitStream(ctx, targetAddr)
	if err != nil {
		s.noteRefusal(targetAddr, err)
		s.sendReply(clientConn, replyFor(err))
		return
	}
	defer p2pStream.Close()
	if v, ok := p2pStream.(VerdictNotifier); ok {
		// fast open: the exit answers while the application already talks. A refusal can no
		// longer become a SOCKS error code, so the connection is reset instead, and the reason
		// still reaches the log, the UI and the failover.
		v.OnVerdict(func(err error) {
			if err == nil {
				s.health.ok()
				return
			}
			s.noteRefusal(targetAddr, err)
			if tc, ok := clientConn.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
		})
	} else {
		s.health.ok()
	}

	// 4. Send SOCKS5 Success Reply
	// [VER 0x05, REP 0x00 (Success), RSV 0x00, ATYP 0x01, BND.ADDR (0.0.0.0), BND.PORT (0)]
	if err := s.sendReply(clientConn, 0x00); err != nil {
		return
	}

	// 5. Bidirectional Splicing
	s.emit("exit:session_started", map[string]any{"target": targetAddr})
	s.spliceP2P(clientConn, p2pStream)
	s.emit("exit:session_closed", map[string]any{"target": targetAddr})
}

func (s *SOCKS5Server) sendReply(clientConn net.Conn, repCode byte) error {
	reply := []byte{0x05, repCode, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	_, err := clientConn.Write(reply)
	return err
}

// sendReplyAddr replies with a bound IPv4 address (UDP ASSOCIATE tells the client where to send).
func (s *SOCKS5Server) sendReplyAddr(clientConn net.Conn, repCode byte, bound *net.UDPAddr) error {
	reply := []byte{0x05, repCode, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	copy(reply[4:8], bound.IP.To4())
	binary.BigEndian.PutUint16(reply[8:], uint16(bound.Port))
	_, err := clientConn.Write(reply)
	return err
}

// spliceP2P runs the connection until both sides are done, half-closes included.
func (s *SOCKS5Server) spliceP2P(clientConn net.Conn, p2pStream io.ReadWriteCloser) {
	_ = relay(s.ctx, clientConn, p2pStream,
		func(n int) { s.bytesTx.Add(uint64(n)) },
		func(n int) { s.bytesRx.Add(uint64(n)) })
}

// SetEmitter attaches the node event bus for exit-session telemetry
func (s *SOCKS5Server) SetEmitter(e events.Emitter) {
	s.emitter.Store(&e)
}

func (s *SOCKS5Server) emit(topic string, payload any) {
	if p := s.emitter.Load(); p != nil && *p != nil {
		(*p).Emit(topic, payload)
	}
}
