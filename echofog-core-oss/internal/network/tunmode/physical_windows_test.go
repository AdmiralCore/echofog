package tunmode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// With no EchoFog adapter up, the interface the default route goes out of is the one Internet
// traffic uses.
func TestPhysicalInterfaceIndexMatchesDefault(t *testing.T) {
	def, err := DefaultInterfaceIndex()
	if err != nil {
		t.Skip("no default interface here:", err)
	}
	got, err := PhysicalInterfaceIndex()
	require.NoError(t, err)
	assert.Equal(t, def, got)
}
