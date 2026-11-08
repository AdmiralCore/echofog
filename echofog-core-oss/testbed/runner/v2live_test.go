package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTheSixLiveScenariosAreNamedAndTheOthersAreNot(t *testing.T) {
	for _, n := range []string{"v2-nat", "v2-paths", "v2-hidden", "v2-messaging", "v2-call", "v2-exit"} {
		assert.True(t, isV2LiveScenario(n), n)
	}
	for _, n := range []string{"v2-basic", "v2-censor", "v2-carrier", "smoke", ""} {
		assert.False(t, isV2LiveScenario(n), n)
	}
}

// The live mode needs commands the efv2 CLI does not have yet; until it has them the runner says so
// and exits 4 ("prepared, not runnable") without starting a container.
func TestALiveScenarioThatTheCLICannotDriveSaysWhatItNeedsAndExitsFour(t *testing.T) {
	var out bytes.Buffer
	assert.Equal(t, 4, runV2Live("v2-messaging", &out))
	s := out.String()
	assert.Contains(t, s, "v2-messaging")
	assert.Contains(t, s, "efv2 chat")
	assert.Contains(t, s, "not verified")
	assert.Contains(t, s, "launchpad-scenarios", "points at the model mode that does run")
	assert.False(t, strings.Contains(s, "docker compose up"), "it starts nothing")
}

func TestAnUnknownLiveScenarioIsABadArgument(t *testing.T) {
	var out bytes.Buffer
	assert.Equal(t, 3, runV2Live("v2-moon", &out))
}

func TestTheLaunchpadFlagsDoNotRejectALiveScenario(t *testing.T) {
	for _, n := range v2LiveScenarios {
		assert.NoError(t, validateV2Flags(n), n)
	}
	assert.Error(t, validateV2Flags("v2-moon"), "an unknown v2 scenario is still refused")
}
