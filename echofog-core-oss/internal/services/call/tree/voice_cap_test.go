package tree

import (
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// A member's relay capacity is what it claims. One that claimed a thousand children became the
// parent of most of the voice tree, and could then mute whom it liked by dropping their sound. No
// member feeds more than MaxVoiceFanout others now, whatever it claims.
func TestBuildVoiceTopology_ClaimedCapacityIsCapped(t *testing.T) {
	coord := types.PeerID{1}
	greedy := types.PeerID{2}
	profiles := []PeerNodeProfile{
		{PeerID: coord, UploadKbps: 1000, MaxRelayChildren: 2},
		{PeerID: greedy, UploadKbps: 1_000_000, MaxRelayChildren: 1000},
	}
	for i := 0; i < 40; i++ {
		profiles = append(profiles, PeerNodeProfile{PeerID: types.PeerID{byte(10 + i)}, UploadKbps: 5000, MaxRelayChildren: 4})
	}
	m := BuildVoiceTopology("c", 1, coord, profiles)
	assert.LessOrEqual(t, len(m.ChildrenMap[greedy]), MaxVoiceFanout)
	for id, kids := range m.ChildrenMap {
		if id != coord {
			assert.LessOrEqual(t, len(kids), MaxVoiceFanout, "member %s", id)
		}
	}
	assert.Len(t, m.ParentMap, len(profiles)-1, "everybody still has a place")
}
