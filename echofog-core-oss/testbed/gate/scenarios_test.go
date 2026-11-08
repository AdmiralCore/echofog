package gate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scenarioFixture is every required scenario, green over three runs, in the given mode.
func scenarioFixture(mode string) []ScenarioResult {
	var out []ScenarioResult
	for _, n := range RequiredScenarios {
		out = append(out, ScenarioResult{Name: n, Mode: mode, Runs: 3, Rows: []Row{
			{ID: n + "/ok", Metric: "check", Value: "1", Limit: "1", Status: Green, Source: "T15.1"},
		}})
	}
	return out
}

func both() []ScenarioResult {
	return append(scenarioFixture("model"), scenarioFixture("live")...)
}

func TestNoScenarioReportIsNotMeasuredNeverGreen(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Now: now})
	r := row(res, "scenarios", "")
	assert.Equal(t, NotMeasured, r.Status)
}

func TestModelOnlyScenariosLeaveTheLiveStandNotMeasured(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Scenarios: scenarioFixture("model"), Now: now})
	for _, n := range RequiredScenarios {
		live := row(res, "scenario:"+n, "live")
		assert.Equal(t, NotMeasured, live.Status, n)
		assert.Contains(t, live.Note, "audit", "the reason is the audit phase, not a defect: %s", n)
		model := row(res, "scenario:"+n, "model runs")
		assert.Equal(t, Green, model.Status, n)
		assert.Equal(t, Green, row(res, "scenario:"+n+"/"+n+"/ok", "").Status)
	}
	assert.False(t, res.Blocks)
}

func TestBothModesGreenThreeRunsAreGreen(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Scenarios: both(), Now: now})
	for _, n := range RequiredScenarios {
		assert.Equal(t, Green, row(res, "scenario:"+n, "live").Status, n)
		assert.Equal(t, Green, row(res, "scenario:"+n, "model runs").Status, n)
	}
}

func TestFewerThanThreeRunsAreNotMeasured(t *testing.T) {
	list := scenarioFixture("model")
	list[0].Runs = 2
	res := Evaluate(Input{Run: goodRun(t), Scenarios: list, Now: now})
	r := row(res, "scenario:"+list[0].Name, "model runs")
	assert.Equal(t, NotMeasured, r.Status)
	assert.Contains(t, r.Value, "2")
}

func TestARedRowInAScenarioBlocksTheRelease(t *testing.T) {
	list := scenarioFixture("model")
	list[2].Rows[0].Status = Red
	res := Evaluate(Input{Run: goodRun(t), Scenarios: list, Now: now})
	assert.True(t, res.Blocks)
	assert.Equal(t, Red, res.Verdict)
	assert.Equal(t, Red, row(res, "scenario:"+list[2].Name, "model runs").Status, "three runs in a row are not green")
}

func TestARequiredScenarioThatIsMissingIsNotMeasured(t *testing.T) {
	list := scenarioFixture("model")[1:]
	res := Evaluate(Input{Run: goodRun(t), Scenarios: list, Now: now})
	assert.Equal(t, NotMeasured, row(res, "scenario:"+RequiredScenarios[0], "model runs").Status)
}

func TestAScenarioRowThatWasNotMeasuredStaysNotMeasured(t *testing.T) {
	list := scenarioFixture("model")
	list[1].Rows[0].Status = NotMeasured
	res := Evaluate(Input{Run: goodRun(t), Scenarios: list, Now: now})
	assert.Equal(t, NotMeasured, row(res, "scenario:"+list[1].Name+"/"+list[1].Name+"/ok", "").Status)
	assert.Equal(t, NotMeasured, row(res, "scenario:"+list[1].Name, "model runs").Status, "green means measured")
}

func TestStatusRoundTripsThroughText(t *testing.T) {
	for _, s := range []Status{Green, Yellow, Red, NotMeasured, Info} {
		b, err := s.MarshalText()
		require.NoError(t, err)
		var back Status
		require.NoError(t, back.UnmarshalText(b))
		assert.Equal(t, s, back)
	}
	var s Status
	assert.Error(t, s.UnmarshalText([]byte("purple")))
}

func TestLoadScenarios(t *testing.T) {
	ok := `{"schema":1,"commit":"abc","scenarios":[{"name":"v2-nat","mode":"model","runs":3,"rows":[{"ID":"x","Metric":"m","Status":"green"}]}]}`
	rep, err := LoadScenarios(strings.NewReader(ok))
	require.NoError(t, err)
	require.Len(t, rep.Scenarios, 1)
	assert.Equal(t, Green, rep.Scenarios[0].Rows[0].Status)
	for name, bad := range map[string]string{
		"empty":         ``,
		"no scenarios":  `{"schema":1,"scenarios":[]}`,
		"unknown field": `{"schema":1,"scenarios":[{"name":"v2-nat","mode":"model","runs":3,"rows":[],"extra":1}]}`,
		"bad mode":      `{"schema":1,"scenarios":[{"name":"v2-nat","mode":"dream","runs":3,"rows":[]}]}`,
		"unknown name":  `{"schema":1,"scenarios":[{"name":"v2-moon","mode":"model","runs":3,"rows":[]}]}`,
		"negative runs": `{"schema":1,"scenarios":[{"name":"v2-nat","mode":"model","runs":-1,"rows":[]}]}`,
		"trailing":      ok + `{}`,
		"bad status":    `{"schema":1,"scenarios":[{"name":"v2-nat","mode":"model","runs":3,"rows":[{"ID":"x","Status":"purple"}]}]}`,
	} {
		_, err := LoadScenarios(strings.NewReader(bad))
		assert.Error(t, err, name)
	}
}

func TestTheChecklistNamesTheScenarios(t *testing.T) {
	res := Evaluate(Input{Run: goodRun(t), Scenarios: both(), Now: now})
	found := false
	for _, it := range res.Checklist {
		if strings.Contains(it.Text, "T15.1") {
			found = true
			assert.Equal(t, Green, it.Status)
		}
	}
	assert.True(t, found, "a checklist line for the scenarios")
}

func FuzzLoadScenarios(f *testing.F) {
	f.Add(`{"schema":1,"commit":"abc","scenarios":[{"name":"v2-nat","mode":"model","runs":3,"rows":[{"ID":"x","Metric":"m","Status":"green"}]}]}`)
	f.Add(`{"schema":1,"scenarios":[]}`)
	f.Add(``)
	f.Add(`{"scenarios":[{"name":"v2-nat","mode":"live","runs":0,"rows":null}]}`)
	f.Fuzz(func(t *testing.T, in string) {
		rep, err := LoadScenarios(strings.NewReader(in))
		if err != nil {
			return
		}
		// whatever loads, the gate can read without panicking
		_ = scenarioRows(rep.Scenarios)
	})
}

// A scenario that measured nothing but information has measured nothing.
func TestAScenarioOfOnlyInfoRowsIsNotMeasured(t *testing.T) {
	list := scenarioFixture("model")
	list[0].Rows = []Row{{ID: "x", Metric: "m", Status: Info}}
	res := Evaluate(Input{Run: goodRun(t), Scenarios: list, Now: now})
	assert.Equal(t, NotMeasured, row(res, "scenario:"+list[0].Name, "model runs").Status)
}
