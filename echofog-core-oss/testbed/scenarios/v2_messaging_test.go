package scenarios

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/echofog/echofog-core/testbed/gate"
)

// v2-messaging: two real nodes meet over noise3 and chat/2 sends messages through the real outbox.
// The delivery rows are green when the directory resolves and are NOT MEASURED when it does not:
// never green on a thing that was not measured. In the loopback world the DHT does not resolve (a
// finding of T15.1, see the report), so the scenario says so, and the test pins that honesty.
func TestMessagingScenarioIsGreenOrSaysWhatItCouldNotMeasure(t *testing.T) {
	r := runModel(t, "v2-messaging", 4*time.Minute)
	requireGreen(t, r, "v2-messaging/session")
	for _, id := range []string{"v2-messaging/exactly-once", "v2-messaging/offline-queue", "v2-messaging/state"} {
		row, ok := rowOf(r, id)
		if !ok {
			t.Fatalf("row %s is missing", id)
		}
		switch row.Status {
		case gate.Green:
		case gate.NotMeasured:
			assert.NotEmpty(t, row.Note, "%s: a row that was not measured says why", id)
			assert.NotEmpty(t, r.NotChecked)
		default:
			t.Errorf("%s: %s (%s %s)", id, row.Status, row.Value, row.Note)
		}
	}
}
