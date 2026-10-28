package swarm

import (
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/stretchr/testify/assert"
)

// "auto" stands down when told the node is in a country that does not censor; "on" does not.
func TestGuardModeAutoStandsDownInFreeCountry(t *testing.T) {
	s, _ := newTestSwarm(t)
	withGuardMode(s, config.GuardAuto)
	assert.True(t, s.GuardModeActive(), "auto, country not known: on")

	s.SetGuardsFree(true)
	assert.False(t, s.GuardModeActive(), "auto in a free country: off")
	assert.True(t, s.GuardsFree())

	withGuardMode(s, config.GuardOn)
	assert.True(t, s.GuardModeActive(), "on ignores the country")

	withGuardMode(s, config.GuardAuto)
	s.SetGuardsFree(false)
	assert.True(t, s.GuardModeActive(), "back in a censoring country: on")
}
