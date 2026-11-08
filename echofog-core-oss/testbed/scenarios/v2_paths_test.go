package scenarios

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/echofog/echofog-core/testbed/gate"
)

// v2-paths: the product's own session and path code over real noise3 on loopback behind the netem.
func TestPathsScenarioIsGreenInTheModel(t *testing.T) {
	r := runModel(t, "v2-paths", 3*time.Minute)
	requireGreen(t, r,
		"v2-paths/second-path",    // AddPath binds a second path, both are live
		"v2-paths/cut-failover",   // the first path is cut in the middle of a stream: the stream finishes whole, the pause is at most 10 s
		"v2-paths/address-change", // the client's address changes behind the NAT: the session goes on
		"v2-paths/h15-backoff",    // a block that is never lifted is retried by the H15 schedule (D24)
		"v2-paths/two-carriers",   // at most two carriers to one pair (D21)
	)
}

// The scenario must be able to fail: with a path that is cut for good and no second path, the
// stream does not finish and the row is red, not skipped.
func TestPathsScenarioCutRowIsRedWithoutASecondPath(t *testing.T) {
	row := pathsCutWithoutSecondPath(t.Context())
	assert.Equal(t, gate.Red, row.Status, "%s: %s", row.Value, row.Note)
}
