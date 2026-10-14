package node

import (
	"testing"

	"go.uber.org/goleak"
)

// A test that leaves a goroutine behind (a connection never closed, a loop nobody stops) fails the
// package: such leaks are how a long-running node slowly runs out of memory.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// started by the package init of the TUN stack, for the life of the process
		goleak.IgnoreAnyFunction("github.com/xjasonlyu/tun2socks/v2/tunnel/statistic.(*Manager).handle"),
		goleak.IgnoreAnyFunction("github.com/xjasonlyu/tun2socks/v2/tunnel.(*Tunnel).process"),
	)
}
