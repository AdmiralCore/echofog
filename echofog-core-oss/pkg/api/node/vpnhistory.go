package node

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// The exits this computer used, with what each one gave: when, how long until it served us,
// traffic, refusals, price. Kept in the sealed part of the database ("vpnmarket:" values are
// encrypted), so the list of exits and their addresses is no easier to read than the messages.
const (
	vpnHistoryKey      = "vpnmarket:history"
	maxVPNHistory      = 100
	vpnHistorySaveEach = 30 * time.Second
)

// VPNSession is one stretch of using one exit: from connecting (or switching to it) until
// disconnecting, switching away, or the app closing.
type VPNSession struct {
	PeerID    string `json:"peerId"`
	Addr      string `json:"addr,omitempty"` // host:port it was reached at, to reconnect without a lookup
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt,omitempty"` // 0 while it is the session in use
	UpdatedAt int64  `json:"updatedAt"`
	// ReadyMs is how long the exit took to confirm it serves us (0: it never did)
	ReadyMs    int64  `json:"readyMs,omitempty"`
	BytesRx    uint64 `json:"bytesRx"`
	BytesTx    uint64 `json:"bytesTx"`
	Refused    uint64 `json:"refused"`
	LastState  string `json:"lastState,omitempty"`
	PricePerMB int64  `json:"pricePerMb"` // micro-USD per MB from the exit's advert; 0 free or unknown
	// EndReason: "stopped" (disconnected), "switched" (moved to another exit), "app_closed"
	EndReason string `json:"endReason,omitempty"`
	Active    bool   `json:"active"` // the session in use right now (not stored)
}

// sessionTrack is the session in use, with the proxy's counters when it began: the proxy keeps
// counting across a switch, so each session takes the difference.
type sessionTrack struct {
	s                           VPNSession
	baseRx, baseTx, baseRefused uint64
	lastSave                    time.Time
}

func loadVPNHistory(st storage.Storage) []VPNSession {
	if st == nil {
		return nil
	}
	raw, err := st.Get([]byte(vpnHistoryKey))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var list []VPNSession
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	return list
}

func saveVPNHistory(st storage.Storage, list []VPNSession) error {
	if st == nil {
		return errors.New("node is not running")
	}
	if len(list) > maxVPNHistory {
		list = list[:maxVPNHistory]
	}
	for i := range list {
		list[i].Active = false
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return st.Set([]byte(vpnHistoryKey), raw)
}

// putSession stores s in place of the stored entry for the same session, or first.
func putSession(list []VPNSession, s VPNSession) []VPNSession {
	for i := range list {
		if list[i].PeerID == s.PeerID && list[i].StartedAt == s.StartedAt {
			list[i] = s
			return list
		}
	}
	return append([]VPNSession{s}, list...)
}

func (n *NodeApi) historyStorage() storage.Storage {
	if n == nil || n.node == nil {
		return nil
	}
	return n.node.Storage
}

// beginSessionLocked starts the record for the exit just taken into use. vpn.mu is held.
func (n *NodeApi) beginSessionLocked(peer string) {
	now := time.Now()
	t := &sessionTrack{s: VPNSession{PeerID: peer, StartedAt: now.Unix(), UpdatedAt: now.Unix()}}
	if n.vpn.server != nil {
		_, t.baseRx, t.baseTx = n.vpn.server.Stats()
		t.baseRefused, _ = n.vpn.server.Refusals()
	}
	if id, err := types.PeerIDFromString(peer); err == nil && n.node != nil {
		if n.node.Swarm != nil {
			if p := n.node.Swarm.GetPeer(id); p != nil {
				t.s.Addr = p.Addr()
			}
		}
		if n.node.VPNMarket != nil && n.node.VPNMarket.Directory != nil {
			if e, ok := n.node.VPNMarket.Directory.Get(id); ok {
				t.s.PricePerMB = e.Advert.PricePerMB
			}
		}
	}
	n.vpn.session = t
	n.saveSessionLocked(true)
}

// refreshSessionLocked brings the session's counters up to date from the proxy.
func (n *NodeApi) refreshSessionLocked() {
	t := n.vpn.session
	if t == nil || n.vpn.server == nil {
		return
	}
	_, rx, tx := n.vpn.server.Stats()
	refused, _ := n.vpn.server.Refusals()
	t.s.BytesRx, t.s.BytesTx, t.s.Refused = rx-t.baseRx, tx-t.baseTx, refused-t.baseRefused
	t.s.LastState = n.vpn.exitState
	t.s.UpdatedAt = time.Now().Unix()
}

// saveSessionLocked writes the session in use, at most every vpnHistorySaveEach unless force.
func (n *NodeApi) saveSessionLocked(force bool) {
	t := n.vpn.session
	if t == nil {
		return
	}
	n.refreshSessionLocked()
	if !force && time.Since(t.lastSave) < vpnHistorySaveEach {
		return
	}
	t.lastSave = time.Now()
	st := n.historyStorage()
	_ = saveVPNHistory(st, putSession(loadVPNHistory(st), t.s))
}

// endSessionLocked closes the session in use for reason and stores it.
func (n *NodeApi) endSessionLocked(reason string) {
	t := n.vpn.session
	if t == nil {
		return
	}
	n.refreshSessionLocked()
	t.s.EndedAt, t.s.EndReason = time.Now().Unix(), reason
	n.saveSessionLocked(true)
	n.vpn.session = nil
}

// noteSessionStateLocked records when the exit first confirmed it serves us.
func (n *NodeApi) noteSessionStateLocked(state string) {
	t := n.vpn.session
	if t == nil {
		return
	}
	t.s.LastState = state
	if state == ExitOK && t.s.ReadyMs == 0 {
		t.s.ReadyMs = max(1, time.Since(time.Unix(t.s.StartedAt, 0)).Milliseconds())
	}
}

// GetVPNHistory lists the exits used, newest first, the one in use with live counters. A session
// the app was closed in the middle of ends where it was last saved.
func (n *NodeApi) GetVPNHistory() []VPNSession {
	n.vpn.mu.Lock()
	var current *VPNSession
	if t := n.vpn.session; t != nil {
		n.refreshSessionLocked()
		cp := t.s
		cp.Active = true
		current = &cp
	}
	n.vpn.mu.Unlock()

	list := loadVPNHistory(n.historyStorage())
	if current != nil {
		list = putSession(list, *current)
	}
	for i := range list {
		if list[i].EndedAt == 0 && !list[i].Active {
			list[i].EndedAt, list[i].EndReason = list[i].UpdatedAt, "app_closed"
		}
	}
	return list
}

// ClearVPNHistory forgets the exits used; the session in use, if any, starts the new list.
func (n *NodeApi) ClearVPNHistory() error {
	st := n.historyStorage()
	if st == nil {
		return errors.New("node is not running")
	}
	n.vpn.mu.Lock()
	defer n.vpn.mu.Unlock()
	var keep []VPNSession
	if t := n.vpn.session; t != nil {
		n.refreshSessionLocked()
		keep = []VPNSession{t.s}
	}
	return saveVPNHistory(st, keep)
}

// ConnectLastExit connects through the exit used most recently: at the address it was reached at
// last time, else found again through the network.
func (n *NodeApi) ConnectLastExit() error {
	list := loadVPNHistory(n.historyStorage())
	if len(list) == 0 {
		return errors.New("no exit used yet")
	}
	last := list[0]
	if last.Addr != "" {
		if err := n.StartVPNVia(last.Addr + "/" + last.PeerID); err == nil {
			return nil
		}
	}
	return n.StartVPN(last.PeerID)
}

// keepSessionSaved stores the session in use now and then, so a crash or a killed app loses at
// most that much of its record.
func (n *NodeApi) keepSessionSaved(ctx context.Context) {
	t := time.NewTicker(vpnHistorySaveEach)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.vpn.mu.Lock()
			n.saveSessionLocked(false)
			n.vpn.mu.Unlock()
		}
	}
}
