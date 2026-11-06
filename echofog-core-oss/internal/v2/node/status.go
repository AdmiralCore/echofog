package node

import (
	"net/netip"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// SessionStatus is the diagnostic view of one live session (T0.3 `efv2 status`).
type SessionStatus struct {
	Node        NodeID
	Device      DeviceID
	SID         [16]byte
	Suite       uint16
	Path        mux.PathKind
	Remote      netip.AddrPort
	LocalDevice DeviceID
	// Paths are every path of the session; ActivePath is the one this side sends on and
	// Standby is the number of other paths kept alive (wire.md §8.3).
	Paths      []uint64
	ActivePath uint64
	Standby    int

	StreamsLocal int
	StreamsPeer  int
	LastActivity time.Time
	GoingAway    bool
	Stats        mux.SessionStats
	// Abuse is the persisted abuse count of the peer device, when the session has an
	// authenticated card.
	Abuse uint64
}

// AbuseStatus is one persisted abuse counter (session.AbuseLog, T3.2).
type AbuseStatus struct {
	Source [32]byte
	Count  uint64
}

// Status is a snapshot of the running node for the operator and the lab CLI. It is read-only and
// safe to call before Start or after Stop.
type Status struct {
	Node       NodeID
	Device     DeviceID
	Profile    Profile
	Background Background
	StartedAt  time.Time
	Sessions   []SessionStatus
	Abuse      []AbuseStatus
	// Birthday counts birthday punch attempts and the ones that found a direct path (T6.7).
	Birthday BirthdayStatus
}

// BirthdayStatus is the birthday punch counters of the connection report: counts only.
type BirthdayStatus struct{ Tried, OK int64 }

// Status returns a snapshot of the node: identity, profile, live sessions and persisted abuse
// counters. It never starts or stops anything and takes only the locks needed to copy the state.
func (r *Runtime) Status() Status {
	r.mu.Lock()
	st := Status{
		Node:       r.identity.NodeID,
		Device:     r.identity.DeviceID,
		Profile:    r.cfg.Profile,
		Background: r.background,
		StartedAt:  r.startedAt,
	}
	st.Profile.Roles = append([]Role(nil), st.Profile.Roles...)
	table := r.table
	r.mu.Unlock()

	if table != nil {
		for _, est := range table.Sessions() {
			if est == nil {
				continue
			}
			ss := SessionStatus{
				SID:    est.SID,
				Suite:  est.Suite,
				Path:   est.Kind,
				Remote: est.Remote,
			}
			if est.Peer != nil {
				ss.Node = NodeID(est.Peer.NodeID)
				ss.Device = DeviceID(est.Peer.DeviceID)
				if r.abuse != nil {
					ss.Abuse, _ = r.abuse.Count(est.Peer.DeviceID)
				}
			}
			if est.Local.Device != nil {
				ss.LocalDevice = DeviceID(est.Local.Device.ID())
			}
			if est.Session != nil {
				ids, active, ok := est.Session.Paths()
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				ss.Paths = ids
				if ok {
					ss.ActivePath = active
					if len(ids) > 0 {
						ss.Standby = len(ids) - 1
					}
				}
				ss.StreamsLocal, ss.StreamsPeer = est.Session.StreamCounts()
				ss.LastActivity = est.Session.LastActivity()
				ss.GoingAway = est.Session.GoingAway()
				ss.Stats = est.Session.Stats()
			}
			st.Sessions = append(st.Sessions, ss)
		}
		sort.Slice(st.Sessions, func(i, j int) bool {
			return string(st.Sessions[i].Device[:]) < string(st.Sessions[j].Device[:])
		})
	}

	if r.abuse != nil {
		snap, err := r.abuse.Snapshot()
		if err == nil {
			st.Abuse = make([]AbuseStatus, 0, len(snap))
			for _, c := range snap {
				st.Abuse = append(st.Abuse, AbuseStatus{Source: c.Source, Count: c.Count})
			}
		}
	}
	return st
}
