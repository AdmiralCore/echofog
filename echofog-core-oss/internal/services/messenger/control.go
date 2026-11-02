package messenger

import (
	"encoding/hex"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A session reset ("fogreset1:") and a knock ("fogknock") change our session or keys with their
// sender, and they travel unencrypted: a reset is sent exactly when the session is broken, a knock
// before there is one. Their envelope is signed with the sender's identity key (the dispatcher
// checks that the origin signed it), so nobody can make one up; but every relay and mailbox that
// carried one can read it and hand it over again, to us later or to anybody else, and each
// replayed reset wiped the session with its sender. So one is acted on only when its signed
// envelope names us as its target, it was sent within controlWindow (by its signed time), and we
// have not seen its id from that sender before (kept 8 days, longer than the window). Older
// versions fill the same signed fields, so their resets and knocks still count; one from a device
// whose clock is off by more than the window, or that waited longer in a mailbox, is ignored,
// and the session heals the usual way at the next message that cannot be read.
const controlWindow = 24 * time.Hour

// controlAccepted reports whether a reset or knock from origin may be acted on.
func (s *MessageBroker) controlAccepted(origin types.PeerID, msg *internal_pb.MessageData, inner *PersonalMessageProto) bool {
	if s.myIDHexed == "" || hex.EncodeToString(msg.GetTargetId()) != s.myIDHexed {
		return false // for somebody else: replayed to us
	}
	sent := time.Unix(int64(msg.GetTimestamp()), 0)
	if d := time.Since(sent); msg.GetTimestamp() > uint64(1<<62) || d > controlWindow || d < -controlWindow {
		return false
	}
	if inner.GetId() == "" {
		return false
	}
	s.mu.RLock()
	filter := s.filter
	s.mu.RUnlock()
	if filter != nil && filter(origin) == Drop {
		return false // blocked
	}
	// scoped by sender: nobody else can spend a reset's id before it arrives
	return s.firstSight(inner.GetId() + "@" + hex.EncodeToString(origin[:]))
}
