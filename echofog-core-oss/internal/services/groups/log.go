package groups

import (
	"log/slog"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Every change to a group (local or on the word of another member) is logged with its outcome,
// so a member list that differs between two nodes can be traced back to the change one of them
// refused. Group changes are rare, so nothing here is rate limited.

func (m *GroupManager) logger() *slog.Logger {
	if m.log == nil {
		return slog.Default().With("component", "groups")
	}
	return m.log
}

// logOp writes the outcome of one group change: Info when it was applied, Warn when refused.
func (m *GroupManager) logOp(op, groupID string, err error, args ...any) {
	args = append([]any{"op", op, "group_id", groupID}, args...)
	if err != nil {
		m.logger().Warn("Group change refused", append(args, "err", err)...)
		return
	}
	m.logger().Info("Group change applied", args...)
}

// shortPeer is the short form of a peer ID used in logs: the first 12 hex characters.
func shortPeer(id types.PeerID) string { return id.String()[:12] }
