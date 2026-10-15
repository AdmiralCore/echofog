package mimicry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The self-signed camouflage certificate is padded toward a real certificate chain's size. In TLS
// 1.3 the Certificate message is encrypted, but a passive observer can still measure the record's
// length; a tiny fixed-size certificate would be a first-flight length fingerprint that the mimicked
// site does not have.
func TestCamouflageCertIsPaddedToARealisticSize(t *testing.T) {
	c, err := GenerateCamouflageCert("example.com")
	require.NoError(t, err)
	require.Len(t, c.Certificate, 1)
	n := len(c.Certificate[0])
	assert.GreaterOrEqual(t, n, padTargetMin, "the camouflage certificate must be padded up toward a real chain's size")
	assert.LessOrEqual(t, n, padTargetMax+512, "padding should stay within a realistic range")
}

// The padded size varies per node, so there is no single network-wide "EchoFog certificate size".
func TestCamouflageCertSizeVariesPerNode(t *testing.T) {
	sizes := map[int]bool{}
	for i := 0; i < 12; i++ {
		c, err := GenerateCamouflageCert("example.com")
		require.NoError(t, err)
		sizes[len(c.Certificate[0])] = true
	}
	assert.Greater(t, len(sizes), 1, "the padded size must vary between generations")
}

// Padding must not make the certificate unusable: it still serves a real TLS 1.3 handshake.
func TestCamouflageCertStillServesTLS(t *testing.T) {
	c, err := GenerateCamouflageCert("example.com")
	require.NoError(t, err)
	_, err = OwnServerProfile(BuildServerTLSConfig(c))
	require.NoError(t, err, "a padded camouflage certificate must still complete a TLS handshake")
}
