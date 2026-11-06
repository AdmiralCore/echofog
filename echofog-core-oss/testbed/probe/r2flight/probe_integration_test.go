package r2flight

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/probe"
)

// The prober against the real reality2-tcp listener of internal/v2, not a stand-in: a replay of an
// admitted first flight (at once, after the token's window, from another address), malformed
// first records, a suspicious source and a foreign token all get what the cover gives. A fresh
// valid token is the one thing the node answers on its own.
func TestProbesAgainstTheRealReality2Listener(t *testing.T) {
	r := newRig(t)
	now := time.Unix(0, r.clock.Load())
	pub := r.mask.PublicKey().Bytes()

	first, err := Flight(pub, r.addr, "cover.example", now)
	require.NoError(t, err)
	// the legitimate client arrives: the node admits it and begins its own handshake
	got := send(t, r.addr, first)
	require.GreaterOrEqual(t, len(got), 5)
	covers := r.covers.Load()
	send(t, r.addr, first) // the same flight again
	assert.EqualValues(t, covers+1, r.covers.Load(), "the replay went to the cover: the node did not admit it twice")
	covers = r.covers.Load()

	valid, err := Flight(pub, r.addr, "cover.example", now)
	require.NoError(t, err)
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	foreign, err := Flight(other.PublicKey().Bytes(), r.addr, "cover.example", now)
	require.NoError(t, err)

	tg := probe.Target{
		Name: "ordinary", Network: "tcp", Addr: r.addr, Cover: r.coverAddr, SNI: "cover.example",
		AltDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}}).DialContext(ctx, network, addr)
		},
	}
	in := probe.Inputs{
		FirstFlight: first, Valid: valid, Foreign: foreign,
		Window: 20 * time.Minute, Listen: 800 * time.Millisecond,
		Wait: func(d time.Duration) { r.clock.Add(int64(d)) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rep := probe.Run(ctx, tg, in)

	for _, o := range rep.Outcomes {
		if o.Skipped != "" {
			t.Logf("skipped %s/%s: %s", o.Detector, o.Case, o.Skipped)
			continue
		}
		t.Logf("%s/%-18s own=%-5v node=%+v cover=%+v", o.Detector, o.Case, o.Own, o.Node, o.Cover)
	}
	for _, o := range rep.Outcomes {
		if o.Detector == "D15" && o.Case == "valid token" {
			continue
		}
		assert.False(t, o.Own && o.Skipped == "", "%s/%s: the node answered on its own: %+v against the cover's %+v", o.Detector, o.Case, o.Node, o.Cover)
	}
	assert.Equal(t, 0, rep.OwnAnswers())
	assert.Greater(t, r.covers.Load(), covers, "the strangers' probes reached the cover")
}
