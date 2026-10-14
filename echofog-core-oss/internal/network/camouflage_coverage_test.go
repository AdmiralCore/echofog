package network

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVisionRandIntZero(t *testing.T) {
	if randInt(0) != 0 || randInt(-5) != 0 {
		t.Fatal("randInt of a non-positive bound must be 0")
	}
	for i := 0; i < 100; i++ {
		if v := randInt(10); v < 0 || v >= 10 {
			t.Fatalf("randInt(10) out of range: %d", v)
		}
	}
}

// After the padded budget the stream is a plain copy in both directions.
func TestVisionStreamRawTailAfterBudget(t *testing.T) {
	a, b := net.Pipe()
	w := newVisionStream(pipeMux{a})
	r := newVisionStream(pipeMux{b})
	payload := make([]byte, visionPadBudget+4096)
	for i := range payload {
		payload[i] = byte(i)
	}
	go func() {
		_, _ = w.Write(payload)
		_ = a.Close()
	}()
	got := make([]byte, 0, len(payload))
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	require.Equal(t, len(payload), len(got))
	assert.Equal(t, payload, got)
}

func TestWSConnAddrsAndDeadlines(t *testing.T) {
	srv, cli := wsPair(t)
	defer srv.Close()
	defer cli.Close()
	assert.NotNil(t, cli.LocalAddr())
	assert.NotNil(t, cli.RemoteAddr())
	require.NoError(t, cli.SetReadDeadline(time.Now().Add(time.Minute)))
	require.NoError(t, cli.SetWriteDeadline(time.Now().Add(time.Minute)))
	require.NoError(t, cli.SetDeadline(time.Now().Add(time.Minute)))
}

// A text/control message is skipped; the next binary message is read as the stream.
func TestWSConnSkipsNonBinary(t *testing.T) {
	srv, cli := wsPair(t)
	defer srv.Close()
	defer cli.Close()
	go func() {
		_ = srv.ws.WriteMessage(websocket.TextMessage, []byte("ignored"))
		_ = srv.ws.WriteMessage(websocket.BinaryMessage, []byte("payload"))
	}()
	buf := make([]byte, 16)
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := cli.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(buf[:n]))
}

// wsPair returns the two ends of a WebSocket connection as wsConns.
func wsPair(t *testing.T) (*wsConn, *wsConn) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srvCh := make(chan *websocket.Conn, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err == nil {
			srvCh <- ws
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	cws, _, err := websocket.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/", nil)
	require.NoError(t, err)
	return newWSConn(<-srvCh), newWSConn(cws)
}

func TestWSTransportSmallSetters(t *testing.T) {
	tr := newWS(t, newTestSigner(t), "/p", false)
	tr.SetLogger(nil) // no-op, must not panic
	tr.SetCaps(handshake.CapRelay)
	assert.Nil(t, tr.Addr(), "Addr is nil before Listen")
}

func TestWSTransportDialRejectsNonWSScheme(t *testing.T) {
	tr := newWS(t, newTestSigner(t), "/p", false)
	err := tr.Dial(context.Background(), "http://example.test/p")
	require.Error(t, err)
}

func TestWSTransportPerNodeDialNeedsExpectedID(t *testing.T) {
	tr := newWS(t, newTestSigner(t), "/p", true) // per-node keys on
	// a ws URL with no ExpectPeer: the per-node key is unknown
	err := tr.Dial(context.Background(), "ws://127.0.0.1:9/p")
	require.ErrorIs(t, err, ErrPeerUnknown)
}

func TestTCPTransportSmallSetters(t *testing.T) {
	tr := newTCP(t, newTestSigner(t), "")
	tr.SetLogger(nil)
	tr.SetCaps(handshake.CapRelay)
	assert.Nil(t, tr.Addr())
}

// A WebSocket visitor without a valid token is relayed to the real site over the WebSocket, and
// sees that site's genuine certificate (the REALITY path, over WS).
func TestWSStrangerRelayedToRealSite(t *testing.T) {
	site, siteCert := camouflageSite(t)
	cfg := MimicryConfigFrom(MimicryOptions{})
	cfg.FallbackAddr = site
	tr := NewWSTransport(testCert(t), cfg, NewDefaultPeerAuthenticator(newTestSigner(t), 101, 5*time.Second), "/p")
	t.Cleanup(func() { tr.Close() })
	require.NoError(t, tr.Listen("127.0.0.1:0"))

	ws, _, err := websocket.DefaultDialer.Dial("ws://"+tr.Addr().String()+"/p", nil)
	require.NoError(t, err)
	defer ws.Close()
	inner := tls.Client(newWSConn(ws), &tls.Config{InsecureSkipVerify: true})
	require.NoError(t, inner.Handshake())
	got := inner.ConnectionState().PeerCertificates[0]
	assert.True(t, got.Equal(siteCert), "the WS stranger did not get the real site's certificate")
}
