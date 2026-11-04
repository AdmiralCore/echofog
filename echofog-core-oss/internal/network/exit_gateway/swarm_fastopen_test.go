package exit_gateway

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Over the real QUIC path: a refused target ends the app's connection with the reason recorded.
func TestFastOpenRefusalThroughRealSwarm(t *testing.T) {
	proxy, svc := proxyPair(t)
	svc.AllowPrivateTargets(false) // loopback targets are refused now
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
