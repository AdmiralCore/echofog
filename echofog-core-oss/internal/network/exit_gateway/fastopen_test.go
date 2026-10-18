package exit_gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowExit is an exit that takes delay to answer each tunnel with result, then echoes.
func slowExit(delay time.Duration, result byte) StreamOpener {
	return func(context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			defer server.Close()
			if _, _, err := server.ReadFrame(); err != nil {
				return
			}
			time.Sleep(delay)
			_ = server.WriteFrame(net_types.TypeExitConnectResult, []byte{result})
			if result == resultOK {
				_, _ = io.Copy(server, server)
			}
		}()
		return client, nil
	}
}

// The SOCKS reply does not wait for the exit: the app's first bytes leave at once and the exit's
// answer comes with the first reply. Before, every connection paid the round trip to the exit
// plus the exit's own dial before the app could send anything.
func TestFastOpenRepliesBeforeTheExitAnswers(t *testing.T) {
	srv := quietServer(t, NewP2PDialer(slowExit(400*time.Millisecond, resultOK)))
	start := time.Now()
	c, err := socksDial(srv.Addr().String(), "203.0.113.5:80")
	require.NoError(t, err)
	defer c.Close()
	assert.Less(t, time.Since(start), 300*time.Millisecond, "SOCKS success must not wait for the exit")

	c.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Write([]byte("early"))
	require.NoError(t, err, "the app writes before the exit has answered")
	got := make([]byte, 5)
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, "early", string(got))
	require.Eventually(t, func() bool { return !srv.Health().LastOK.IsZero() }, 2*time.Second, 10*time.Millisecond,
		"the exit's yes still counts as a healthy stream")
}

// A refusal that arrives after the SOCKS success still reaches the log, the UI and the
// failover, and the app's connection is torn down instead of hanging.
func TestFastOpenRefusalIsStillNoted(t *testing.T) {
	srv := quietServer(t, NewP2PDialer(slowExit(50*time.Millisecond, resultPayment)))
	c, err := socksDial(srv.Addr().String(), "203.0.113.5:80")
	require.NoError(t, err)
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(make([]byte, 1))
	require.Error(t, err)
	assert.False(t, isTimeout(err), "the refused connection must end, not hang")
	require.Eventually(t, func() bool { n, _ := srv.Refusals(); return n == 1 }, 2*time.Second, 10*time.Millisecond)
	_, last := srv.Refusals()
	assert.Equal(t, "payment", last.Reason)
	assert.Equal(t, "203.0.113.5:80", last.Host)
}

// An exit that never answers fails the stream once the dial context ends, as "timed out", so
// the failover still sees a dead exit.
func TestFastOpenVerdictTimesOut(t *testing.T) {
	open := func(context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() { _, _, _ = server.ReadFrame() }() // reads the request, never answers
		return &closingPipe{client}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	st, err := NewP2PDialer(open).DialExitStream(ctx, "203.0.113.5:80")
	require.NoError(t, err)
	verdict := make(chan error, 1)
	st.(VerdictNotifier).OnVerdict(func(err error) { verdict <- err })
	_, err = st.Read(make([]byte, 1))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, "timed out", ExitRefusal(<-verdict))
}

// Closing a stream ourselves before the exit answered is not a refusal.
func TestFastOpenLocalCloseIsNotARefusal(t *testing.T) {
	open := func(context.Context) (FramedStream, error) {
		client, server := newPipePair()
		go func() {
			_, _, _ = server.ReadFrame()
			<-server.in // until the client end goes away: never answers
		}()
		// a real stream's pending read fails when it is closed; the pipe's frames do not
		return &closingPipe{client}, nil
	}
	st, err := NewP2PDialer(open).DialExitStream(context.Background(), "203.0.113.5:80")
	require.NoError(t, err)
	called := make(chan error, 1)
	st.(VerdictNotifier).OnVerdict(func(err error) { called <- err })
	require.NoError(t, st.Close())
	select {
	case err := <-called:
		t.Fatalf("a local close was reported as %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// closingPipe ends a pending ReadFrame when closed, like a QUIC stream does.
type closingPipe struct{ *pipeStream }

func (c *closingPipe) Close() error {
	defer func() { recover() }() // closing twice
	close(c.in)
	return c.pipeStream.Close()
}

// The confirmed dial keeps the old contract for checks that need the verdict first.
func TestConfirmedDialReturnsTheRefusal(t *testing.T) {
	_, err := NewP2PDialer(slowExit(0, resultDialFailed)).DialExitStreamConfirmed(context.Background(), "x.example:443")
	assert.ErrorIs(t, err, ErrExitDialFailed)
	st, err := NewP2PDialer(slowExit(0, resultOK)).DialExitStreamConfirmed(context.Background(), "x.example:443")
	require.NoError(t, err)
	st.Close()
}

// Over the real QUIC path: a refused target ends the app's connection with the reason recorded.
func TestFastOpenRefusalThroughRealSwarm(t *testing.T) {
	proxy, svc := proxyPair(t)
	svc.allowPrivate = false // loopback targets are refused now
	c, err := socksDial(proxy, echoServer(t))
	require.NoError(t, err, "fast open: the SOCKS reply is success")
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("hi"))
	_, err = c.Read(make([]byte, 1))
	require.Error(t, err)
	var ne net.Error
	assert.False(t, errors.As(err, &ne) && ne.Timeout(), "refused, not hung")
}
