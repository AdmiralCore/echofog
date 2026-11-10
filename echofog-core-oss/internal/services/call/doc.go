// Package call is the group call service: who is in a call, who relays whose media, and the media
// itself (voice, screens, cameras), end to end encrypted, over the node's transport.
//
// # The shape of a call
//
// A call is a room. One member, the coordinator, builds the voice tree (tree.BuildVoiceTopology):
// everybody's voice is flooded along it. Each shared screen or camera is a stream with a tree of its
// own (tree.StreamTreeEngine): the broadcaster feeds a few viewers, and viewers with upload to spare
// relay to the rest, so a stream costs its broadcaster a few copies, not one per viewer. Only
// viewers that pressed "watch" are in a stream's tree.
//
// The coordinator decides trees and membership (manager_coordinator.go), members follow its
// manifests (manager_signaling.go), a member that loses the coordinator holds an election, and rooms
// that split find each other again (splitbrain_test.go shows how). All of it travels as signals
// (signaling/): signed JSON, sealed end to end between members that both announced a key
// (signalseal.go).
//
// # Files
//
//	manager.go              CallManager and the room's state; timing of the room protocol
//	manager_rooms.go        joining and leaving a room
//	manager_coordinator.go  coordinator duties: members, trees, keyframes, subscribe/reparent
//	manager_signaling.go    every inbound signal, flooded or unicast, and what each does
//	manager_streams.go      starting, changing and watching streams; SetDisplayRate
//	manager_media.go        the hooks the UI/bridge and a local encoder use (PublishLocalRTP, ...)
//	mediakey.go             the media key and its rotation when members change
//	signalseal.go           end to end sealing of signals
//	feedback.go, rate.go, layers.go   what viewers report and what the broadcaster and relays do
//	bridge/                 the local WebRTC bridge to the webview (captured tracks in, playback out)
//	media/                  fragments, relays, loss recovery, the per-peer outbox
//	tree/                   the voice and stream trees and the quality presets
//	signaling/              the signal types
//	transport/              the port to the node's network: the call never names the network stack
//
// # How a screen stream is kept smooth on a bad path
//
// The pieces below act at different distances from the problem, and each assumes the ones before it
// did what they could:
//
//  1. FEC (media/fec.go): one parity fragment per small group of fragments, so a single lost
//     fragment is rebuilt at once, by a viewer or a relay, without asking anybody.
//  2. NACK (media/nack.go): a viewer that still finds a unit missing asks its parent, which resends
//     from a ring of what it sent (or asks its own parent). The ring holds sealed fragments; relays
//     never decrypt.
//  3. Keyframe request: the last resort when a gap outlives the reorder window, rate limited per
//     hop (manager_coordinator.go).
//  4. Viewers' reports (media/feedback.go, feedback.go): every FeedbackInterval a viewer measures
//     its path (units it had to NACK, queueing delay from the timestamp every fragment carries,
//     what arrived) and reports to its parent; relays pass their subtree's reports up. The
//     broadcaster knows every viewer's path.
//  5. Rate control (rate.go): the broadcaster takes the worse of its own send queue and the worst
//     strong viewer's report, cuts or holds or climbs the bitrate of the encoder, and caps it at
//     what reaches the viewer, so the encoder, not a router's queue, decides what is dropped.
//  6. Weak viewers (feedback.go, tree.SetWeak): a viewer whose own path is bad again and again
//     while another viewer's is clean is weak. The coordinator hangs it at the bottom of the tree,
//     the rate no longer follows it, and its keyframe requests are slowed down.
//  7. Temporal layers (layers.go, media/layers.go): a relay leaves the upper layers of the frames
//     out for a child whose hop cannot carry the whole stream, so the child gets fewer frames
//     instead of losing packets of every frame.
//  8. Sound (media/repeat.go): sound has no time for a resend, so every unit goes twice on every
//     hop, the second time with the next unit; the replay window drops the copy that comes second.
//
// Above all of this the desktop's own capture steps down the quality presets when the bitrate does
// not cover the chosen one, and picks a frame rate the viewers' screens show evenly (see package
// nativecap in the desktop app).
//
// # Conventions
//
//   - Methods of CallManager whose names end in Locked expect m.mu held. A callback to the UI or a
//     capture is queued with m.later and runs from unlockAndFlush after the lock is released, so it
//     may call back into the manager; never call such a callback with the lock held.
//   - Anything a remote peer sends is untrusted: signals are bounded and checked against the tree
//     before they change anything (see noteFeedbackLocked and weakLocked for how).
//   - A new signal or a new field must be harmless to older nodes: an unknown action is rejected
//     and logged at debug level, an unknown JSON field is dropped. Say in the signal's comment what
//     an older node does with it.
//   - The package must not import the v1 network: it sends and receives through the transport port.
//     boundary_test.go checks that.
package call
