package tunmode

import (
	"context"
	"flag"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHelper plays the elevated helper: it connects back, authenticates and reports status.
func fakeHelper(t *testing.T, status string, token string, hold chan net.Conn) func(string, []string) error {
	return func(_ string, args []string) error {
		require.Equal(t, HelperFlag, args[0])
		fs := flag.NewFlagSet("h", flag.ContinueOnError)
		ctl := fs.String("ctl", "", "")
		tok := fs.String("token", "", "")
		fs.String("socks", "", "")
		fs.Int("bypass-udp-port", 0, "")
		fs.String("hold", "", "")
		require.NoError(t, fs.Parse(args[1:]))
		if token != "" {
			*tok = token
		}
		go func() {
			c, err := net.Dial("tcp", *ctl)
			if err != nil {
				return
			}
			io.WriteString(c, *tok+"\n"+status+"\n")
			if hold != nil {
				hold <- c
				return
			}
			io.Copy(io.Discard, c)
			c.Close()
		}()
		return nil
	}
}

func TestControllerStartStop(t *testing.T) {
	hold := make(chan net.Conn, 1)
	c := &Controller{launch: fakeHelper(t, lineUp, "", hold)}
	require.NoError(t, c.Start(context.Background(), "127.0.0.1:1080", 4242, ""))
	assert.True(t, c.Running())
	helperSide := <-hold

	c.Stop()
	assert.False(t, c.Running())
	helperSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(helperSide)
	require.NoError(t, err, "the helper must see the control connection close")
	assert.Equal(t, "STOP"+string(rune(10)), string(got), "a stop says so, so a kill switch does not hold")
}

func TestControllerReportsHelperError(t *testing.T) {
	c := &Controller{launch: fakeHelper(t, "ERR wintun.dll is missing next to EchoFog.exe", "", nil)}
	err := c.Start(context.Background(), "127.0.0.1:1080", 0, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wintun.dll")
	assert.False(t, c.Running())
}

func TestControllerIgnoresWrongToken(t *testing.T) {
	c := &Controller{launch: fakeHelper(t, lineUp, "not-the-token", nil)}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	assert.Error(t, c.Start(ctx, "127.0.0.1:1080", 0, ""))
	assert.False(t, c.Running())
}

func TestControllerNoticesHelperExit(t *testing.T) {
	hold := make(chan net.Conn, 1)
	exited := make(chan error, 1)
	c := &Controller{launch: fakeHelper(t, lineUp, "", hold), OnExit: func(err error) { exited <- err }}
	require.NoError(t, c.Start(context.Background(), "127.0.0.1:1080", 0, ""))
	(<-hold).Close() // helper crashed or was killed

	select {
	case err := <-exited:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("OnExit was not called")
	}
	assert.False(t, c.Running())
}
