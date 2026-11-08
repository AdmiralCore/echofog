package scenarios

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/gate"
)

// byName finds a registered scenario.
func byName(t *testing.T, name string) Scenario {
	t.Helper()
	for _, s := range Registry() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no scenario %s", name)
	return Scenario{}
}

// runModel runs a scenario once in the model, short, and returns the result.
func runModel(t *testing.T, name string, timeout time.Duration) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return byName(t, name).Run(ctx, Env{Seed: 7, Short: true, Dir: t.TempDir()})
}

func rowOf(r Result, id string) (gate.Row, bool) {
	for _, row := range r.Rows {
		if row.ID == id {
			return row, true
		}
	}
	return gate.Row{}, false
}

// requireGreen asserts that every named row exists and is green, and that no row is red or yellow:
// a limit is never loosened to turn a scenario green, so a failure here is a finding or a bug.
func requireGreen(t *testing.T, r Result, ids ...string) {
	t.Helper()
	for _, id := range ids {
		row, ok := rowOf(r, id)
		require.True(t, ok, "row %s is missing", id)
		assert.Equal(t, gate.Green, row.Status, "%s: %s against %s (%s)", id, row.Value, row.Limit, row.Note)
	}
	for _, row := range r.Rows {
		assert.NotEqual(t, gate.Red, row.Status, "%s: %s against %s (%s)", row.ID, row.Value, row.Limit, row.Note)
	}
	assert.Equal(t, "model", r.Mode)
}
