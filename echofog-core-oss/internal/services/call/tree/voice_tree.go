package tree

import (
	"bytes"
	"sort"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// DefaultCoordinatorFanout is used when the coordinator did not declare its relay capacity.
// Voice is ~64 kbps per speaker, so a coordinator can feed far more children than a video relay.
const DefaultCoordinatorFanout = 8

func relayScore(p PeerNodeProfile) float64 {
	return float64(p.UploadKbps) - float64(p.RTT.Milliseconds()*10) - float64(p.PacketLossPercent*100)
}

// BuildVoiceTopology constructs a shallow Voice Backbone Tree with primary and standby parents.
// Every peer is attached at the smallest available depth, which keeps per-hop latency low.
func BuildVoiceTopology(callID string, epoch uint64, coordinator types.PeerID, profiles []PeerNodeProfile) *TreeManifest {
	manifest := &TreeManifest{
		CallID:              callID,
		Epoch:               epoch,
		Coordinator:         coordinator,
		StandbyCoordinators: make([]types.PeerID, 0),
		ParentMap:           make(map[types.PeerID]ParentAssignment),
		ChildrenMap:         make(map[types.PeerID][]types.PeerID),
		Profiles:            make(map[types.PeerID]PeerNodeProfile),
		Timestamp:           time.Now().UnixNano(),
	}

	for _, p := range profiles {
		manifest.Profiles[p.PeerID] = p
	}
	coordProfile, ok := manifest.Profiles[coordinator]
	if !ok {
		coordProfile = PeerNodeProfile{PeerID: coordinator}
	}
	if coordProfile.MaxRelayChildren < 2 {
		coordProfile.MaxRelayChildren = DefaultCoordinatorFanout
	}
	manifest.Profiles[coordinator] = coordProfile

	var relays, leaves []PeerNodeProfile
	seen := map[types.PeerID]bool{coordinator: true}
	for _, p := range profiles {
		if seen[p.PeerID] {
			continue
		}
		seen[p.PeerID] = true
		if p.IsMobile || p.MaxRelayChildren <= 0 {
			leaves = append(leaves, p)
		} else {
			relays = append(relays, p)
		}
	}
	byScore := func(list []PeerNodeProfile) {
		sort.Slice(list, func(i, j int) bool {
			si, sj := relayScore(list[i]), relayScore(list[j])
			if si != sj {
				return si > sj
			}
			return bytes.Compare(list[i].PeerID[:], list[j].PeerID[:]) < 0
		})
	}
	byScore(relays)
	byScore(leaves)

	for _, r := range relays {
		manifest.StandbyCoordinators = append(manifest.StandbyCoordinators, r.PeerID)
	}
	for _, l := range leaves {
		manifest.StandbyCoordinators = append(manifest.StandbyCoordinators, l.PeerID)
	}

	capacity := map[types.PeerID]int{coordinator: coordProfile.MaxRelayChildren}
	depth := map[types.PeerID]int{coordinator: 0}
	attached := []types.PeerID{coordinator}

	for _, node := range append(relays, leaves...) {
		child := node.PeerID
		primary := findBestParent(attached, capacity, depth, manifest.Profiles, coordinator)
		capacity[primary]--
		depth[child] = depth[primary] + 1

		manifest.ParentMap[child] = ParentAssignment{
			PrimaryParent: primary,
			StandbyParent: findStandbyParent(attached, depth, manifest.ParentMap, primary, coordinator),
		}
		manifest.ChildrenMap[primary] = append(manifest.ChildrenMap[primary], child)

		if node.MaxRelayChildren > 0 && !node.IsMobile {
			capacity[child] = node.MaxRelayChildren
			attached = append(attached, child)
		}
	}

	return manifest
}

// findBestParent picks the shallowest attached node with free capacity, best link quality first.
func findBestParent(attached []types.PeerID, capacity map[types.PeerID]int, depth map[types.PeerID]int, profiles map[types.PeerID]PeerNodeProfile, coord types.PeerID) types.PeerID {
	var best types.PeerID
	found := false
	for _, id := range attached {
		if capacity[id] <= 0 {
			continue
		}
		if !found || depth[id] < depth[best] ||
			(depth[id] == depth[best] && relayScore(profiles[id]) > relayScore(profiles[best])) {
			best, found = id, true
		}
	}
	if found {
		return best
	}
	// Everyone is saturated: the coordinator absorbs the overflow rather than dropping a member.
	return coord
}

// findStandbyParent picks a warm backup that does not depend on the primary parent, so it
// keeps receiving audio when the primary dies. Earlier-attached nodes are never descendants
// of the child, which keeps the failover graph loop-free. Every node descends from the
// coordinator, so children of the coordinator get a sibling relay: it covers a broken link,
// and a dead coordinator is handled by election anyway.
func findStandbyParent(attached []types.PeerID, depth map[types.PeerID]int, parents map[types.PeerID]ParentAssignment, primary, coord types.PeerID) types.PeerID {
	var best types.PeerID
	found := false
	for _, id := range attached {
		if id == primary || (primary != coord && isDescendant(id, primary, parents)) {
			continue
		}
		if !found || depth[id] < depth[best] {
			best, found = id, true
		}
	}
	if found {
		return best
	}
	return coord
}

func isDescendant(node, ancestor types.PeerID, parents map[types.PeerID]ParentAssignment) bool {
	for i := 0; i < 64; i++ {
		ass, ok := parents[node]
		if !ok {
			return false
		}
		if ass.PrimaryParent == ancestor {
			return true
		}
		node = ass.PrimaryParent
	}
	return false
}

// ElectNewCoordinator deterministically picks the highest-ranking alive candidate.
func ElectNewCoordinator(standbyList []types.PeerID, alivePeers map[types.PeerID]bool) (types.PeerID, bool) {
	for _, candidate := range standbyList {
		if alivePeers[candidate] {
			return candidate, true
		}
	}

	var aliveList []types.PeerID
	for p, alive := range alivePeers {
		if alive {
			aliveList = append(aliveList, p)
		}
	}
	if len(aliveList) == 0 {
		return types.PeerID{}, false
	}
	sort.Slice(aliveList, func(i, j int) bool {
		return bytes.Compare(aliveList[i][:], aliveList[j][:]) < 0
	})
	return aliveList[0], true
}
