package network

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingHandler counts the log records whose message contains a phrase.
type countingHandler struct {
	phrase string
	n      *atomic.Int64
}

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.phrase) {
		h.n.Add(1)
	}
	return nil
}
func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(string) slog.Handler      { return h }

// Closing the transport while it accepts ends the accept loop, whatever socket it runs on and
// whichever goes first. On CI the loop once spun on "quic: transport closed: connection closed",
// logging it as fast as it could (millions of lines): the closed transport is now matched by kind,
// and any other repeated error is logged once a second with a pause between tries.
func TestAcceptLoopEndsWhenTransportCloses(t *testing.T) {
	for name, mk := range map[string]func() *QuicTransport{
		"plain":       func() *QuicTransport { return newTestQUIC(t, newTestSigner(t)) },
		"obfs muxed":  func() *QuicTransport { return newObfsNode(t, false, "").tr },
		"obfs direct": func() *QuicTransport { return newObfsNode(t, true, "").tr },
	} {
		t.Run(name, func(t *testing.T) {
			q := mk()
			var n atomic.Int64
			q.SetLogger(slog.New(countingHandler{phrase: "Accept error", n: &n}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			require.NoError(t, q.ListenEarly(ctx))
			time.Sleep(50 * time.Millisecond)
			_ = q.Close()
			time.Sleep(300 * time.Millisecond)
			assert.Less(t, n.Load(), int64(3), "the accept loop must stop on a closed transport, not log it again and again")
		})
	}
	// the socket under the transport goes first (the obfuscating mux closed): the transport
	// closes itself with that cause, "quic: transport closed: connection closed"
	t.Run("socket closed under it", func(t *testing.T) {
		q := newObfsNode(t, false, "").tr
		require.NotNil(t, q.Mux)
		var n atomic.Int64
		q.SetLogger(slog.New(countingHandler{phrase: "Accept error", n: &n}))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, q.ListenEarly(ctx))
		time.Sleep(50 * time.Millisecond)
		_ = q.Mux.Close()
		time.Sleep(300 * time.Millisecond)
		assert.Less(t, n.Load(), int64(3), "the accept loop must stop on a closed transport, not log it again and again")
	})
}
