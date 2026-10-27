package groups

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

var (
	ErrInvalidGenesis = errors.New("invalid group genesis manifest")
	ErrInvalidType    = errors.New("unknown group type")
)

type SignFunc func(digest []byte) ([]byte, error)

type GroupSummary struct {
	Genesis     *GroupGenesisManifest
	MemberCount int
}

// CreateGroupSigned signs the genesis through an external signer so the identity key stays in the keystore.
func (m *GroupManager) CreateGroupSigned(title, description string, gType GroupType, creatorID types.PeerID, creatorPub types.IdentityPublicKey, sign SignFunc) (g *GroupGenesisManifest, err error) {
	defer func() {
		id := ""
		if g != nil {
			id = g.GroupID
		}
		m.logOp("create", id, err, "type", gType, "creator", shortPeer(creatorID))
	}()
	if gType < GroupTypePrivateGroup || gType > GroupTypeOpenSwarm {
		return nil, ErrInvalidType
	}
	if sign == nil {
		return nil, errors.New("signer required")
	}

	var salt [16]byte
	if _, err := io.ReadFull(rand.Reader, salt[:]); err != nil {
		return nil, err
	}

	genesis := &GroupGenesisManifest{
		GroupID:     fmt.Sprintf("fog_grp_%s_%s", hex.EncodeToString(creatorID[:6]), hex.EncodeToString(salt[:4])),
		Title:       title,
		Description: description,
		Type:        gType,
		Creator:     creatorID,
		CreatorPub:  creatorPub,
		CreatedAt:   time.Now().UnixMilli(),
		Salt:        salt,
	}

	sig, err := sign(genesis.CanonicalHash())
	if err != nil {
		return nil, err
	}
	if len(sig) != len(genesis.Signature) {
		return nil, errors.New("unexpected signature size")
	}
	copy(genesis.Signature[:], sig)

	m.restore(genesis, []MemberProfile{{
		PeerID:   creatorID,
		PubKey:   creatorPub,
		Role:     RoleOwner,
		JoinedAt: genesis.CreatedAt,
	}})
	return genesis, nil
}

// VerifyGenesis checks the creator signature of a manifest received from another node.
func VerifyGenesis(g *GroupGenesisManifest) error {
	if g == nil || g.GroupID == "" || g.Title == "" {
		return ErrInvalidGenesis
	}
	if g.Type < GroupTypePrivateGroup || g.Type > GroupTypeOpenSwarm {
		return ErrInvalidType
	}
	if !ed25519.Verify(ed25519.PublicKey(g.CreatorPub[:]), g.CanonicalHash(), g.Signature[:]) {
		return ErrInvalidGenesis
	}
	if IDOfKey(g.CreatorPub) != g.Creator {
		return ErrInvalidGenesis // the creator id must belong to the creator key
	}
	return nil
}

// ImportGroup registers a verified foreign genesis; it is a no-op when the group is already known.
func (m *GroupManager) ImportGroup(g *GroupGenesisManifest) error {
	if err := VerifyGenesis(g); err != nil {
		id := ""
		if g != nil {
			id = g.GroupID
		}
		// a forged or corrupt invite, or one from a build with another manifest format
		m.logOp("import", id, err)
		return err
	}
	m.mu.RLock()
	_, exists := m.groups[g.GroupID]
	m.mu.RUnlock()
	if exists {
		return nil
	}
	// the creator is the owner from the start; everybody else is learned from invites and events
	m.restore(g, []MemberProfile{{PeerID: g.Creator, PubKey: g.CreatorPub, Role: RoleOwner, JoinedAt: g.CreatedAt}})
	m.logOp("import", g.GroupID, nil, "type", g.Type, "creator", shortPeer(g.Creator))
	return nil
}

// RestoreGroup rebuilds a persisted group with its member list.
func (m *GroupManager) RestoreGroup(g *GroupGenesisManifest, members []MemberProfile) error {
	if g == nil || g.GroupID == "" {
		m.logOp("restore", "", ErrInvalidGenesis)
		return ErrInvalidGenesis
	}
	m.restore(g, members)
	m.logger().Debug("Restored group", "group_id", g.GroupID, "members", len(members))
	return nil
}

func (m *GroupManager) restore(g *GroupGenesisManifest, members []MemberProfile) {
	state := &GroupState{
		Genesis:    g,
		Members:    make(map[types.PeerID]MemberProfile),
		Bans:       make(map[types.PeerID]string),
		Tombstones: make(map[[16]byte]*MessageTombstone),
		ActionLog:  make([]*GroupAction, 0),
	}
	for _, p := range members {
		state.Members[p.PeerID] = p
		if p.IsBanned {
			state.Bans[p.PeerID] = p.BanReason
		}
	}
	m.mu.Lock()
	m.groups[g.GroupID] = state
	m.mu.Unlock()
	if _, ok := m.keyEngine.GetCurrentKey(g.GroupID); !ok {
		if _, err := m.keyEngine.GenerateInitialKey(g.GroupID); err != nil {
			m.logger().Warn("Could not create the group key", "group_id", g.GroupID, "err", err)
		}
	}
}

// ListGroups returns every known group ordered by creation time.
func (m *GroupManager) ListGroups() []GroupSummary {
	m.mu.RLock()
	out := make([]GroupSummary, 0, len(m.groups))
	for _, st := range m.groups {
		st.mu.RLock()
		out = append(out, GroupSummary{Genesis: st.Genesis, MemberCount: len(st.Members)})
		st.mu.RUnlock()
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Genesis.CreatedAt < out[j].Genesis.CreatedAt })
	return out
}

// Bans returns who is banned from a group, with the reason.
func (m *GroupManager) Bans(groupID string) map[types.PeerID]string {
	m.mu.RLock()
	st, ok := m.groups[groupID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make(map[types.PeerID]string, len(st.Bans))
	for id, reason := range st.Bans {
		out[id] = reason
	}
	return out
}

// RestoreBans puts back the bans of a restored group: a ban outlives a restart, so the banned
// peer cannot be added again.
func (m *GroupManager) RestoreBans(groupID string, bans map[types.PeerID]string) {
	m.mu.RLock()
	st, ok := m.groups[groupID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, reason := range bans {
		if _, member := st.Members[id]; member {
			continue // added again by an admin since: the ban was lifted
		}
		st.Bans[id] = reason
	}
}

// GetGroup returns the genesis manifest of a group.
func (m *GroupManager) GetGroup(groupID string) (*GroupGenesisManifest, error) {
	m.mu.RLock()
	st, ok := m.groups[groupID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrGroupNotFound
	}
	return st.Genesis, nil
}

// ListMembers returns the members of a group ordered by join time.
func (m *GroupManager) ListMembers(groupID string) ([]MemberProfile, error) {
	m.mu.RLock()
	st, ok := m.groups[groupID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrGroupNotFound
	}
	st.mu.RLock()
	out := make([]MemberProfile, 0, len(st.Members))
	for _, p := range st.Members {
		out = append(out, p)
	}
	st.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].JoinedAt != out[j].JoinedAt {
			return out[i].JoinedAt < out[j].JoinedAt
		}
		return out[i].Role.Rank > out[j].Role.Rank
	})
	return out, nil
}
