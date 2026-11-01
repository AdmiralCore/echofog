package node

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/tunmode"
	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHeldHelper plays a TUN helper that holds the traffic (the kill switch): it writes the hold
// file, answers ADOPT with UP and reports the control lines it gets afterwards.
func fakeHeldHelper(t *testing.T, holdFile string) <-chan string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	st := tunmode.HoldState{Port: ln.Addr().(*net.TCPAddr).Port, Token: "tok", Socks: "127.0.0.1:1080"}
	b, _ := json.Marshal(st)
	require.NoError(t, os.MkdirAll(filepath.Dir(holdFile), 0o755))
	require.NoError(t, os.WriteFile(holdFile, b, 0o600))
	lines := make(chan string, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(c)
			tok, _ := r.ReadString('\n')
			cmd, _ := r.ReadString('\n')
			if strings.TrimSpace(tok) != "tok" || !strings.HasPrefix(cmd, "ADOPT") {
				c.Close() // Held() only checks that something answers
				continue
			}
			_ = os.Remove(holdFile)
			_, _ = c.Write([]byte("UP\n"))
			go func() {
				for {
					l, err := r.ReadString('\n')
					if l = strings.TrimSpace(l); l != "" {
						lines <- l
					}
					if err != nil {
						close(lines)
						return
					}
				}
			}()
			return
		}
	}()
	return lines
}

func nextLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case l := <-lines:
		return l
	case <-time.After(3 * time.Second):
		t.Fatal("the helper got no control line")
		return ""
	}
}

func killSwitchAPI(t *testing.T) *NodeApi {
	cfg := config.DefaultConfig("t")
	cfg.Storage.DatabasePath = filepath.Join(t.TempDir(), "db")
	api := NewNodeApi(node.NewNode(t.Context(), cfg, "t"))
	api.tun.holdFile = filepath.Join(t.TempDir(), "EchoFog", "killswitch.json")
	return api
}

// On by default, saved, and turned off on the running helper too.
func TestKillSwitchSetting(t *testing.T) {
	api := killSwitchAPI(t)
	assert.True(t, api.KillSwitchOn(), "on by default")

	lines := fakeHeldHelper(t, api.holdPath())
	require.NoError(t, api.tun.ctl.Adopt(api.holdPath(), 0))
	require.NoError(t, api.SetKillSwitch(false))
	assert.False(t, api.KillSwitchOn())
	assert.Equal(t, "HOLD 0", nextLine(t, lines), "the running helper hears it")

	again := NewNodeApi(node.NewNode(t.Context(), api.node.Cfg, "t"))
	assert.False(t, again.KillSwitchOn(), "kept for the next start")
	api.tun.ctl.Release()
}

// Switching exits with the kill switch on leaves the TUN up and blocking; the status says so. A
// real stop says STOP to the helper, so the kill switch does not hold after it.
func TestKillSwitchHoldsAcrossAnExitSwitch(t *testing.T) {
	api := killSwitchAPI(t)
	lines := fakeHeldHelper(t, api.holdPath())
	require.NoError(t, api.tun.ctl.Adopt(api.holdPath(), 0))

	require.NoError(t, api.StopVPNForSwitch())
	assert.True(t, api.tun.ctl.Running(), "the TUN stays while the next exit connects")
	st := api.GetVPNStatus()
	assert.True(t, st.WholeComputer)
	assert.True(t, st.KillSwitch)
	assert.True(t, st.Blocked, "no exit serves us: blocked, not leaking")

	api.StopNode() // the app is closed on purpose
	assert.Equal(t, "STOP", nextLine(t, lines))
	assert.False(t, api.tun.ctl.Running())
}

// With the kill switch off, a switch of exits takes the TUN down as before.
func TestNoKillSwitchSwitchStopsTheTun(t *testing.T) {
	api := killSwitchAPI(t)
	api.tun.killSwitch.Store(false)
	lines := fakeHeldHelper(t, api.holdPath())
	require.NoError(t, api.tun.ctl.Adopt(api.holdPath(), 0))
	require.NoError(t, api.StopVPNForSwitch())
	assert.Equal(t, "STOP", nextLine(t, lines))
	assert.False(t, api.tun.ctl.Running())
}
