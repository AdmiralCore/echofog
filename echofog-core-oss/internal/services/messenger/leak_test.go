package messenger

import (
	"testing"

	"go.uber.org/goleak"
)

// A test that leaves a goroutine behind fails the package (see the other leak_test.go files).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
