package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCensoringCountry(t *testing.T) {
	assert.True(t, censoringCountry(""), "not known yet: guards stay on")
	assert.True(t, censoringCountry("RU"))
	assert.True(t, censoringCountry(" ir "), "case and spaces do not matter")
	assert.True(t, censoringCountry("by"))
	assert.False(t, censoringCountry("DE"))
	assert.False(t, censoringCountry("US"))
	assert.False(t, censoringCountry("FI"))
}
