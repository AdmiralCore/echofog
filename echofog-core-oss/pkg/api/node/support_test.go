package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// With no node (or no project address) the support feature must be fully invisible and inert.
func TestSupportHiddenWithoutProjectAddress(t *testing.T) {
	n := &NodeApi{}
	s := n.GetSupport()
	assert.False(t, s.Available)
	assert.Zero(t, s.Percent)
	assert.Error(t, n.SetSupportPercent(5), "nothing to configure without an address")
	assert.Equal(t, "", n.GetOnRampURL())
}
