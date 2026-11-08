package scenarios

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/gate"
)

// knownFinding is a row that is red because a real carrier does something the censor model or the
// prober catches. Marginal says the measurement sits at the edge of its threshold, so a run may
// come out green: the row may then be green, never anything but red or green.
type knownFinding struct {
	why      string
	marginal bool
}

// knownFindings are the red rows. A row that is red and not listed here fails the test: a new red
// is news. A listed row that is not red any more fails it too, unless it is marginal.
var knownFindings = map[string]knownFinding{
	"v2-censor/noise3/classifier": {why: "D10 (UDP shape) flags about a third of the noise3 flows: the default shape profile draws 0 to 2 junk " +
		"datagrams, and a flow with none opens with the Init (1271 bytes or more, random-looking), which D10 flags; a flow that opens with a " +
		"junk datagram is not flagged. The gate allows 10 %. A finding of the product (shape/noise3). The share is random (5 to 19 of 40 in runs), so the row is red in most runs and green in a few: marginal.", marginal: true},
}

var censorCarriers = []string{"noise3", "reality2-quic", "reality2-tcp", "ws2"}

// v2-censor: the censor model against the real carriers on loopback behind a capture tap.
func TestCensorScenarioRowsExistAndAreHonest(t *testing.T) {
	r := runModel(t, "v2-censor", 4*time.Minute)

	var ids []string
	for _, c := range censorCarriers {
		ids = append(ids, "v2-censor/"+c+"/classifier", "v2-censor/"+c+"/probe")
	}
	ids = append(ids, "v2-censor/control")
	for _, id := range ids {
		_, ok := rowOf(r, id)
		assert.True(t, ok, "row %s is missing", id)
	}
	assert.Len(t, r.Rows, len(ids), "no row beyond the thirteen")
	assert.Equal(t, "model", r.Mode)

	red := map[string]bool{}
	for _, row := range r.Rows {
		t.Logf("%-34s %-11s %s | limit %s | %s", row.ID, row.Status, row.Value, row.Limit, row.Note)
		switch row.Status {
		case gate.Green:
			assert.NotEmpty(t, row.Value, "%s is green without a value", row.ID)
			assert.NotEmpty(t, row.Limit, "%s is green without a limit", row.ID)
		case gate.NotMeasured:
			assert.NotEmpty(t, row.Note, "%s is not measured and does not say why", row.ID)
		case gate.Red:
			red[row.ID] = true
		}
	}
	for id := range red {
		k, known := knownFindings[id]
		assert.True(t, known, "%s is red and is not a known finding", id)
		assert.NotEmpty(t, k.why)
	}
	for id, k := range knownFindings {
		if !k.marginal {
			assert.True(t, red[id], "%s is listed as a known finding and is not red any more", id)
		}
	}
	if len(red) > 0 {
		assert.NotEmpty(t, r.Findings, "a red row comes with a finding")
	}
	for _, f := range r.Findings {
		t.Log("finding:", f)
	}
	for _, f := range r.NotChecked {
		t.Log("not checked:", f)
	}
	require.NotEmpty(t, r.Rows)
}
