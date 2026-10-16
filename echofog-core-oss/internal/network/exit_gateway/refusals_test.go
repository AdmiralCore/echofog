package exit_gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogLimiterFoldsRepeatsIntoOneLine(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLogLimiter(30 * time.Second)
	l.now = func() time.Time { return now }

	ok, skipped := l.allow("denied")
	assert.True(t, ok)
	assert.Zero(t, skipped)
	for i := 0; i < 5; i++ {
		ok, _ = l.allow("denied")
		assert.False(t, ok, "held back inside the window")
	}
	ok, _ = l.allow("payment")
	assert.True(t, ok, "another reason has its own window")

	now = now.Add(31 * time.Second)
	ok, skipped = l.allow("denied")
	assert.True(t, ok)
	assert.Equal(t, 5, skipped, "the next line says how many were folded into it")
}

// captureLog sends the default logger to a buffer for the length of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestExitLogsAndCountsRefusalsByReason(t *testing.T) {
	logs := captureLog(t)
	svc := NewExitService()

	// exit off
	_, err := NewP2PDialer(serve(svc)).DialExitStream(context.Background(), "example.com:443")
	assert.ErrorIs(t, err, ErrExitDenied)
	// private target
	svc.SetEnabled(true)
	_, err = NewP2PDialer(serve(svc)).DialExitStream(context.Background(), "10.0.0.1:80")
	assert.ErrorIs(t, err, ErrExitDenied)
	// unresolvable
	svc.resolver = func(context.Context, string) ([]net.IP, error) { return nil, errors.New("nxdomain") }
	_, err = NewP2PDialer(serve(svc)).DialExitStream(context.Background(), "nowhere.example:443")
	assert.ErrorIs(t, err, ErrExitDialFailed)

	_, refused, _ := svc.Stats()
	assert.Equal(t, uint64(3), refused)
	assert.Equal(t, map[string]uint64{RefuseDisabled: 1, RefuseTarget: 1, RefuseDial: 1}, svc.RefusedByReason())

	out := logs.String()
	for _, r := range []string{RefuseDisabled, RefuseTarget, RefuseDial} {
		assert.Contains(t, out, `msg="refused stream"`)
		assert.Contains(t, out, `reason="`+r+`"`)
	}
	assert.NotContains(t, out, "nowhere.example", "the exit keeps no record of where clients go")
	assert.NotContains(t, out, "10.0.0.1")
}

func TestExitRefusalNames(t *testing.T) {
	assert.Equal(t, "denied", ExitRefusal(ErrExitDenied))
	assert.Equal(t, "payment", ExitRefusal(ErrPaymentRequired))
	assert.Equal(t, "dial failed", ExitRefusal(ErrExitDialFailed))
	assert.Equal(t, "timed out", ExitRefusal(context.DeadlineExceeded))
	assert.Equal(t, "exit unreachable", ExitRefusal(errors.New("open stream to exit peer: no route")))
}

type refusingDialer struct{ err error }

func (d refusingDialer) DialExitStream(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, d.err
}

// socksReply runs one SOCKS5 CONNECT for a domain target and returns the reply code.
func socksReply(t *testing.T, proxy, host string, port byte) byte {
	c, err := net.Dial("tcp", proxy)
	require.NoError(t, err)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Write([]byte{5, 1, 0})
	require.NoError(t, err)
	_, err = io.ReadFull(c, make([]byte, 2))
	require.NoError(t, err)
	req := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
	_, err = c.Write(append(req, 0, port))
	require.NoError(t, err)
	reply := make([]byte, 10)
	_, err = io.ReadFull(c, reply)
	require.NoError(t, err)
	return reply[1]
}

func TestSOCKSLogsWhatTheExitRefused(t *testing.T) {
	srv := NewSOCKS5Server("127.0.0.1:0", refusingDialer{err: ErrExitDenied})
	var logs bytes.Buffer
	srv.logger = slog.New(slog.NewTextHandler(&logs, nil))
	require.NoError(t, srv.Start())
	defer srv.Stop()

	assert.Equal(t, byte(0x05), socksReply(t, srv.Addr().String(), "example.com", 80))
	assert.Equal(t, byte(0x05), socksReply(t, srv.Addr().String(), "example.org", 80))

	n, last := srv.Refusals()
	assert.Equal(t, uint64(2), n)
	require.NotNil(t, last)
	assert.Equal(t, "denied", last.Reason)
	assert.Equal(t, "example.org:80", last.Host)

	out := logs.String()
	assert.Contains(t, out, "exit refused: denied for example.com:80")
	assert.Equal(t, 1, strings.Count(out, "exit refused"), "the second refusal is folded into the next line")
}

type okDialer struct{}

func (okDialer) DialExitStream(context.Context, string) (io.ReadWriteCloser, error) {
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, b) }()
	return a, nil
}

// The proxy moves to another exit without closing its port, and judges the new one afresh.
func TestSOCKSSwitchesExitAndTracksHealth(t *testing.T) {
	srv := NewSOCKS5Server("127.0.0.1:0", refusingDialer{err: ErrExitDialFailed})
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, srv.Start())
	defer srv.Stop()
	addr := srv.Addr().String()

	for _, h := range []string{"a.example", "b.example", "a.example"} {
		assert.Equal(t, byte(0x05), socksReply(t, addr, h, 80))
	}
	h := srv.Health()
	assert.Equal(t, 3, h.FailStreak)
	assert.Equal(t, 2, h.FailHosts)
	assert.True(t, h.LastOK.IsZero())

	srv.SetDialer(okDialer{})
	assert.Zero(t, srv.Health().FailStreak, "a new exit starts with a clean record")
	assert.Equal(t, byte(0x00), socksReply(t, addr, "a.example", 80), "same port, new exit")
	assert.False(t, srv.Health().LastOK.IsZero())

	// refusals that are about payment or policy are not counted as a broken exit
	srv.SetDialer(refusingDialer{err: ErrPaymentRequired})
	socksReply(t, addr, "a.example", 80)
	assert.Zero(t, srv.Health().FailStreak)
}
