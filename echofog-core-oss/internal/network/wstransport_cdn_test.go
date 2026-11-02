package network

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A wss:// link opens its outer TLS as Chrome opens a WebSocket (GREASE, only http/1.1 offered),
// not with Go's own ClientHello, and the upgrade still goes through with the certificate checked.
func TestCDNTLSLooksLikeChrome(t *testing.T) {
	hello := make(chan *tls.ClientHelloInfo, 1)
	up := websocket.Upgrader{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err == nil {
			_ = c.WriteMessage(websocket.TextMessage, []byte("hi"))
			_ = c.Close()
		}
	}))
	srv.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case hello <- h:
		default:
		}
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	cdnRoots = pool
	defer func() { cdnRoots = nil }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := websocket.Dialer{NetDialTLSContext: cdnTLS}
	url := "wss://" + strings.TrimPrefix(srv.URL, "https://")
	ws, _, err := d.DialContext(ctx, url, nil)
	require.NoError(t, err)
	_, msg, err := ws.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "hi", string(msg))
	_ = ws.Close()

	h := <-hello
	assert.Equal(t, []string{"http/1.1"}, h.SupportedProtos)
	grease := false
	for _, c := range h.CipherSuites {
		if c&0x0f0f == 0x0a0a {
			grease = true
		}
	}
	assert.True(t, grease, "Chrome puts a GREASE cipher suite first; Go's own TLS sends none")

	// a certificate nobody vouches for is still refused
	cdnRoots = x509.NewCertPool()
	_, _, err = d.DialContext(ctx, url, nil)
	assert.Error(t, err)
}
