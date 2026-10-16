package call

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNextRate_CutsOnCongestionAndClimbsBackToThePreset(t *testing.T) {
	r := nextRate(0, 10000, 600, false)
	assert.Equal(t, 10000.0, r, "starts at the preset")

	r = nextRate(r, 10000, 600, true)
	assert.Equal(t, 8000.0, r)
	for i := 0; i < 40; i++ {
		r = nextRate(r, 10000, 600, true)
	}
	assert.Equal(t, 600.0, r, "never below the floor")

	for i := 0; i < 200; i++ {
		r = nextRate(r, 10000, 600, false)
	}
	assert.Equal(t, 10000.0, r, "never above the preset")
}
