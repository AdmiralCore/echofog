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
	"v2-censor/noise3/classifier": {why: "D10 (UDP shape) flags about a third of the noise3 flows: a flow whose first datagram is the Init " +
		"(1271 bytes or more, random-looking) is flagged, a flow that opens with the short junk datagram of the shape profile is not; " +
		"the gate allows 10 %"},
	"v2-censor/reality2-tcp/classifier": {why: "D7 (record sizes and timings against the browser control) flags a part of the reality2-tcp " +
		"flows: the record sizes are inside the control's spread, the gaps between records are at the edge of it (the carrier sends small " +
		"mux frames between pages); the share varies from run to run around the 10 % limit", marginal: true},
	"v2-censor/ws2/classifier": {why: "D4 flags every ws2 flow: the outer ClientHello offers only http/1.1 (as a WebSocket dial must), " +
		"its JA4 has h1 where the profile file of browsers has h2, and the file has no Chrome WebSocket hello (needs a real capture); " +
		"D7 flags every flow too (record sizes and gaps inside the WebSocket framing differ from the control; not analysed further)"},
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
