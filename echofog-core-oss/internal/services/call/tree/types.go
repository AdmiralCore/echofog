package tree

import (
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// PeerNodeProfile represents capability and telemetry metrics of a peer in the call.
type PeerNodeProfile struct {
	PeerID            types.PeerID  `json:"peer_id"`
	UploadKbps        int           `json:"upload_kbps"`         // Available upstream bandwidth
	MaxRelayChildren  int           `json:"max_relay_children"`  // How many downstream peers this peer can relay to (0 = leaf only)
	RTT               time.Duration `json:"rtt"`                 // RTT to current coordinator
	PacketLossPercent float32       `json:"packet_loss_percent"` // Recent packet loss
	IsMobile          bool          `json:"is_mobile"`           // If true, should ideally be leaf
	// RelayOnly: reachable only through relay circuits (a symmetric NAT). It relays for nobody
	// and is fed by the coordinator (voice) or the broadcaster (a stream), never by a member that
	// would have to find a relay to it first.
	RelayOnly     bool      `json:"relay_only,omitempty"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	// Backdrop names the animated background the member chose for its call tile ("matrix",
	// "waves", ...; empty for the plain one). Cosmetic: the UI draws the ones it knows and the plain
	// look for the rest, and builds without the field drop it.
	Backdrop string `json:"backdrop,omitempty"`
	// DisplayHz is the refresh rate of the screen the member watches streams on (0: not known). It
	// goes with its subscriptions, and a broadcaster picks a frame rate its viewers' screens show
	// evenly; builds without the field drop it, and their viewers count as not known.
	DisplayHz int `json:"display_hz,omitempty"`
}

// MaxDisplayHz bounds a screen's refresh rate taken from the network.
const MaxDisplayHz = 1000

// MaxBackdropLen bounds a backdrop name; it rides in every voice manifest.
const MaxBackdropLen = 24

// CleanBackdrop returns name if it is a well-formed backdrop name (lowercase letters, digits and
// dashes, at most MaxBackdropLen), and "" (the plain look) otherwise. Members' profiles come from
// the network, so the name is checked before it is kept or passed on.
func CleanBackdrop(name string) string {
	if len(name) > MaxBackdropLen {
		return ""
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return ""
		}
	}
	return name
}

// ParentAssignment defines upstream connections for a peer.
type ParentAssignment struct {
	PrimaryParent types.PeerID `json:"primary_parent"` // Preferred upstream
	StandbyParent types.PeerID `json:"standby_parent"` // Warm standby upstream for instant failover
}

// TreeManifest is the coordinator-signed broadcast topology for the Voice Backbone Tree.
type TreeManifest struct {
	CallID              string                            `json:"call_id"`
	Epoch               uint64                            `json:"epoch"` // Increments on any topology rebuild or coordinator failover
	Coordinator         types.PeerID                      `json:"coordinator"`
	StandbyCoordinators []types.PeerID                    `json:"standby_coordinators"` // Priority order of backup coordinators
	ParentMap           map[types.PeerID]ParentAssignment `json:"parent_map"`           // Peer -> {Primary, Standby}
	ChildrenMap         map[types.PeerID][]types.PeerID   `json:"children_map"`         // Peer -> Downstream children
	Profiles            map[types.PeerID]PeerNodeProfile  `json:"profiles,omitempty"`   // Member telemetry, lets a new coordinator rebuild
	Timestamp           int64                             `json:"timestamp"`
}

// Members lists every peer in the voice tree, coordinator included.
func (m *TreeManifest) Members() []types.PeerID {
	if m == nil {
		return nil
	}
	out := []types.PeerID{m.Coordinator}
	for p := range m.ParentMap {
		if p != m.Coordinator {
			out = append(out, p)
		}
	}
	return out
}

// StreamManifest describes a specific active video/screen stream and its subscriber distribution tree.
type StreamManifest struct {
	StreamID         string                           `json:"stream_id"`
	Broadcaster      types.PeerID                     `json:"broadcaster"`
	MediaType        string                           `json:"media_type"` // "screen" | "camera"
	BitrateKbps      int                              `json:"bitrate_kbps"`
	Width            int                              `json:"width,omitempty"`
	Height           int                              `json:"height,omitempty"`
	FPS              int                              `json:"fps,omitempty"`
	Codec            string                           `json:"codec,omitempty"`
	QualityID        string                           `json:"quality_id,omitempty"`
	Epoch            uint64                           `json:"epoch"`
	Subscribers      []types.PeerID                   `json:"subscribers"`
	DistributionTree map[types.PeerID][]types.PeerID  `json:"distribution_tree"`  // Broadcaster/Relay -> Downstream subscriber peers
	ParentMap        map[types.PeerID]types.PeerID    `json:"parent_map"`         // Subscriber -> Upstream feeder
	Profiles         map[types.PeerID]PeerNodeProfile `json:"profiles,omitempty"` // Broadcaster and subscriber telemetry
	Overflow         []types.PeerID                   `json:"overflow,omitempty"` // Subscribers attached beyond any relay's capacity
	Avoid            map[types.PeerID][]types.PeerID  `json:"avoid,omitempty"`    // Subscriber -> relays it asked not to be fed by
}
