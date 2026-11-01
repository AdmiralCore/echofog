package tunmode

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldHelper runs a helper session (as HelperMain does once the TUN is up) on one end of a
// loopback connection and hands back the node's end, the session's end channel and its counters.
type heldHelper struct {
	node     net.Conn
	done     chan struct{}
	file     string
	flushes  atomic.Int32
	bypassed atomic.Int32
}

func startHelperSession(t *testing.T, holdFile string) *heldHelper {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	node, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	helper, err := ln.Accept()
	require.NoError(t, err)
	h := &heldHelper{node: node, done: make(chan struct{}), file: holdFile}
	s := &helperSession{
		holdFile: holdFile, hold: holdFile != "", socks: "127.0.0.1:1080",
		flush:    func() { h.flushes.Add(1) },
		rebypass: func(p int) error { h.bypassed.Store(int32(p)); return nil },
		logf:     func(string, ...any) {},
	}
	stop := make(chan struct{})
	go func() { s.run(helper, stop); close(h.done) }()
	t.Cleanup(func() {
		select {
		case <-h.done:
		default:
			close(stop)
			<-h.done
		}
	})
	return h
}

func (h *heldHelper) ended(t *testing.T) bool {
	t.Helper()
	select {
	case <-h.done:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func (h *heldHelper) waitHeld(t *testing.T) HoldState {
	t.Helper()
	var st HoldState
	require.Eventually(t, func() bool {
		var ok bool
		st, ok = ReadHold(h.file)
		return ok
	}, 3*time.Second, 10*time.Millisecond, "the helper should hold and write the hold file")
	return st
}

func holdPath(t *testing.T) string { return filepath.Join(t.TempDir(), "EchoFog", "killswitch.json") }

// STOP takes the TUN down even with the kill switch on: the user turned the VPN off.
func TestHelperStopsOnStop(t *testing.T) {
	h := startHelperSession(t, holdPath(t))
	_, _ = io.WriteString(h.node, "STOP\n")
	assert.True(t, h.ended(t))
	_, err := os.Stat(h.file)
	assert.True(t, os.IsNotExist(err), "no hold file after a stop")
}

// The node goes away without STOP (a crash): the helper holds, and a node that comes back adopts
// it without an administrator prompt; a stop from that node then ends it.
func TestHelperHoldsThenANodeAdopts(t *testing.T) {
	h := startHelperSession(t, holdPath(t))
	h.node.Close() // the app crashed
	st := h.waitHeld(t)
	assert.Equal(t, "127.0.0.1:1080", st.Socks)
	assert.Equal(t, os.Getpid(), st.PID)
	assert.True(t, Held(h.file))
	select {
	case <-h.done:
		t.Fatal("the helper must hold, not take the TUN down")
	default:
	}

	var c Controller
	require.NoError(t, c.Adopt(h.file, 5151))
	assert.True(t, c.Running())
	assert.EqualValues(t, 5151, h.bypassed.Load(), "the new node's UDP port is kept off the TUN")
	assert.EqualValues(t, 1, h.flushes.Load(), "the old node's fake IPs are flushed")
	_, err := os.Stat(h.file)
	assert.True(t, os.IsNotExist(err), "adopted: the hold file goes")

	c.Stop()
	assert.True(t, h.ended(t))
}

// Release (switching exits, or the app going away on purpose with the kill switch on) holds as a
// crash does; ReleaseHold then lets the computer back on its own network.
func TestReleaseHoldLetsTrafficOut(t *testing.T) {
	h := startHelperSession(t, holdPath(t))
	h.node.Close()
	h.waitHeld(t)
	require.NoError(t, ReleaseHold(h.file))
	assert.True(t, h.ended(t))
	assert.False(t, Held(h.file))
}

// Without a hold file, or with the kill switch turned off while running, the TUN goes with the
// control connection, as before.
func TestHelperWithoutKillSwitchEndsWithTheNode(t *testing.T) {
	h := startHelperSession(t, "")
	h.node.Close()
	assert.True(t, h.ended(t))

	h2 := startHelperSession(t, holdPath(t))
	_, _ = io.WriteString(h2.node, "HOLD 0\n")
	time.Sleep(50 * time.Millisecond)
	h2.node.Close()
	assert.True(t, h2.ended(t))
	_, err := os.Stat(h2.file)
	assert.True(t, os.IsNotExist(err))
}

// A held helper answers only to the token from the hold file.
func TestHeldHelperIgnoresWrongToken(t *testing.T) {
	h := startHelperSession(t, holdPath(t))
	h.node.Close()
	st := h.waitHeld(t)
	st.Token = "not-the-token"
	bad := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, writeHold(bad, st))
	assert.Error(t, ReleaseHold(bad), "a wrong token is not obeyed")
	select {
	case <-h.done:
		t.Fatal("a wrong token stopped the helper")
	default:
	}
	assert.True(t, Held(h.file), "still holding")
}

// A hold file left by a helper that is gone (the computer restarted) is cleared, not obeyed.
func TestStaleHoldFile(t *testing.T) {
	file := holdPath(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	require.NoError(t, writeHold(file, HoldState{Port: port, Token: "x"}))
	assert.False(t, Held(file))
	var c Controller
	assert.ErrorIs(t, c.Adopt(file, 0), errNoHold)
	_, err = os.Stat(file)
	assert.True(t, os.IsNotExist(err), "the stale file is removed")
	require.NoError(t, ReleaseHold(file))
}
