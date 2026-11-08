package scenarios

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/gate"
)

// knownFindings are the rows that are red because a real carrier does something the censor model
// or the prober catches. A row that is red and not listed here fails the test: a new red is news.
// Each entry says what the finding is.
var knownFindings = map[string]string{}

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

	var red []string
	for _, row := range r.Rows {
		t.Logf("%-34s %-11s %s | limit %s | %s", row.ID, row.Status, row.Value, row.Limit, row.Note)
		switch row.Status {
		case gate.Green:
			assert.NotEmpty(t, row.Value, "%s is green without a value", row.ID)
			assert.NotEmpty(t, row.Limit, "%s is green without a limit", row.ID)
		case gate.NotMeasured:
			assert.NotEmpty(t, row.Note, "%s is not measured and does not say why", row.ID)
		case gate.Red:
			red = append(red, row.ID)
		}
	}
	for _, id := range red {
		why, known := knownFindings[id]
		assert.True(t, known, "%s is red and is not a known finding", id)
		assert.NotEmpty(t, why)
	}
	for id := range knownFindings {
		assert.True(t, slices.Contains(red, id), "%s is listed as a known finding and is not red any more", id)
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
