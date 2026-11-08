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
		// a new v1 identity wipes its phrase after two minutes and keeps the memguard coffer for the life of the
		// process: the tests of the v2 runtime register profiles (T11.7)
		goleak.IgnoreAnyFunction("github.com/echofog/echofog-core/internal/protection/core/identity.(*identityManager).GenerateNewIdentity.func1"),
		goleak.IgnoreAnyFunction("github.com/awnumar/memguard/core.NewCoffer.func1"),
	)
}
