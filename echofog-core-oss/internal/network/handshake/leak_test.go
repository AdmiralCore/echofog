package handshake

import (
	"testing"

	"go.uber.org/goleak"
)

// A test that leaves a goroutine behind (a connection never closed, a loop nobody stops) fails the
// package: such leaks are how a long-running node slowly runs out of memory.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
