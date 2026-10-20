package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
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
	ID        string `json:"id"` // unique per session: two can start within the same second
	PeerID    string `json:"peerId"`
	Addr      string `json:"addr,omitempty"` // host:port it was reached at, to reconnect without a lookup
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt,omitempty"` // 0 while it is the session in use
	UpdatedAt int64  `json:"updatedAt"`
	// ReadyMs is how long the exit took to confirm it serves us (0: it never did)
	ReadyMs int64  `json:"readyMs,omitempty"`
	BytesRx uint64 `json:"bytesRx"`
	BytesTx uint64 `json:"bytesTx"`
	Refused uint64 `json:"refused"`
	// RefusedBy splits Refused by why: "dial failed" (the site, not the exit), "timed out",
	// "exit unreachable", "payment", "denied"
	RefusedBy  map[string]uint64 `json:"refusedBy,omitempty"`
	LastState  string            `json:"lastState,omitempty"`
	PricePerMB int64             `json:"pricePerMb"` // micro-USD per MB; 0 free (PriceKnown) or unknown
	PriceKnown bool              `json:"priceKnown"` // the exit itself told us the price
	// Remaining is the traffic left on the exit's account for us (paid, vouchers, free today), as
	// the exit last reported it at RemainingAt; it stays with that exit after we leave it.
	Remaining   int64 `json:"remaining,omitempty"`
	RemainingAt int64 `json:"remainingAt,omitempty"` // 0: the exit never reported its account
	// EndReason: "stopped" (disconnected), "switched" (moved to another exit), "app_closed"
	EndReason string `json:"endReason,omitempty"`
	Active    bool   `json:"active"` // the session in use right now (not stored)
}

// sessionTrack is the session in use, with the proxy's counters when it began: the proxy keeps
// counting across a switch, so each session takes the difference.
type sessionTrack struct {
	s                           VPNSession
	baseRx, baseTx, baseRefused uint64
	baseRefusedBy               map[string]uint64
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
		same := list[i].ID != "" && list[i].ID == s.ID
		if s.ID == "" || list[i].ID == "" { // stored by an older build
			same = list[i].PeerID == s.PeerID && list[i].StartedAt == s.StartedAt
		}
		if same {
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
	t := &sessionTrack{s: VPNSession{ID: newSessionID(), PeerID: peer, StartedAt: now.Unix(), UpdatedAt: now.Unix()}}
	if n.vpn.server != nil {
		_, t.baseRx, t.baseTx = n.vpn.server.Stats()
		t.baseRefused, _ = n.vpn.server.Refusals()
		t.baseRefusedBy = n.vpn.server.RefusalsByReason()
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
	t.s.RefusedBy = nil
	for reason, c := range n.vpn.server.RefusalsByReason() {
		if d := c - t.baseRefusedBy[reason]; d > 0 {
			if t.s.RefusedBy == nil {
				t.s.RefusedBy = map[string]uint64{}
			}
			t.s.RefusedBy[reason] = d
		}
	}
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

// noteSessionStateLocked records when the exit first confirmed it serves us, and from its
// account answer (nil: none) the price it asks and what is left on it for us.
func (n *NodeApi) noteSessionStateLocked(state string, info *exit_gateway.AccountInfo) {
	t := n.vpn.session
	if t == nil {
		return
	}
	t.s.LastState = state
	if info != nil {
		t.s.PricePerMB, t.s.PriceKnown = info.PricePerMB, true // what the exit asks, not what an advert said
		if info.Enabled {
			t.s.Remaining, t.s.RemainingAt = info.Remaining(), time.Now().Unix()
		}
	}
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

// --- exits as the user knows them: one entry per exit, with a name and a star ---

const (
	vpnExitLabelsKey = "vpnmarket:exit_labels"
	maxExitNameLen   = 40
	maxExitLabels    = 200
)

// exitLabel is what the user set on an exit.
type exitLabel struct {
	Name     string `json:"name,omitempty"`
	Favorite bool   `json:"favorite,omitempty"`
}

// SavedExit is one exit with everything the history says about it, newest facts first.
type SavedExit struct {
	PeerID      string            `json:"peerId"`
	Name        string            `json:"name,omitempty"`
	Favorite    bool              `json:"favorite"`
	Addr        string            `json:"addr,omitempty"` // the last address it was reached at
	Sessions    int               `json:"sessions"`
	BytesRx     uint64            `json:"bytesRx"`
	BytesTx     uint64            `json:"bytesTx"`
	Refused     uint64            `json:"refused"`
	RefusedBy   map[string]uint64 `json:"refusedBy,omitempty"`
	LastUsed    int64             `json:"lastUsed"` // 0: starred but never used
	LastReadyMs int64             `json:"lastReadyMs,omitempty"`
	LastState   string            `json:"lastState,omitempty"`
	PricePerMB  int64             `json:"pricePerMb"`
	PriceKnown  bool              `json:"priceKnown"`
	// Remaining is the traffic left on this exit for us when it last told (RemainingAt, 0: never)
	Remaining   int64  `json:"remaining,omitempty"`
	RemainingAt int64  `json:"remainingAt,omitempty"`
	Active      bool   `json:"active"`
	CountryCode string `json:"countryCode,omitempty"` // ISO 3166 code from a GeoIP lookup of Addr
	City        string `json:"city,omitempty"`
}

func loadExitLabels(st storage.Storage) map[string]exitLabel {
	out := map[string]exitLabel{}
	if st == nil {
		return out
	}
	if raw, err := st.Get([]byte(vpnExitLabelsKey)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func saveExitLabels(st storage.Storage, m map[string]exitLabel) error {
	if st == nil {
		return errors.New("node is not running")
	}
	for id, l := range m {
		if l.Name == "" && !l.Favorite {
			delete(m, id)
		}
	}
	if len(m) > maxExitLabels {
		return errors.New("too many named exits")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return st.Set([]byte(vpnExitLabelsKey), raw)
}

func (n *NodeApi) updateExitLabel(peer string, change func(*exitLabel)) error {
	id, err := types.PeerIDFromString(strings.ToLower(strings.TrimSpace(peer)))
	if err != nil {
		return errors.New("invalid exit ID")
	}
	st := n.historyStorage()
	n.vpn.mu.Lock() // one writer for the labels at a time
	defer n.vpn.mu.Unlock()
	m := loadExitLabels(st)
	l := m[id.String()]
	change(&l)
	m[id.String()] = l
	return saveExitLabels(st, m)
}

// SetExitName gives an exit a name of the user's choice ("" removes it).
func (n *NodeApi) SetExitName(peer, name string) error {
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > maxExitNameLen {
		name = string(r[:maxExitNameLen])
	}
	return n.updateExitLabel(peer, func(l *exitLabel) { l.Name = name })
}

// SetExitFavorite stars an exit or takes the star away.
func (n *NodeApi) SetExitFavorite(peer string, on bool) error {
	return n.updateExitLabel(peer, func(l *exitLabel) { l.Favorite = on })
}

// ForgetExit removes an exit from the history, with its name and star.
func (n *NodeApi) ForgetExit(peer string) error {
	id, err := types.PeerIDFromString(strings.ToLower(strings.TrimSpace(peer)))
	if err != nil {
		return errors.New("invalid exit ID")
	}
	st := n.historyStorage()
	n.vpn.mu.Lock()
	defer n.vpn.mu.Unlock()
	var keep []VPNSession
	for _, s := range loadVPNHistory(st) {
		if s.PeerID != id.String() {
			keep = append(keep, s)
		}
	}
	if err := saveVPNHistory(st, keep); err != nil {
		return err
	}
	m := loadExitLabels(st)
	delete(m, id.String())
	return saveExitLabels(st, m)
}

// GetSavedExits lists every exit used or starred: favourites first, then the most recently used.
func (n *NodeApi) GetSavedExits() []SavedExit {
	sessions := n.GetVPNHistory() // newest first, the one in use included
	labels := loadExitLabels(n.historyStorage())
	byID := map[string]*SavedExit{}
	var order []string
	for _, s := range sessions {
		e := byID[s.PeerID]
		if e == nil {
			// the newest session of this exit sets what is "last"
			e = &SavedExit{PeerID: s.PeerID, Addr: s.Addr, LastUsed: s.StartedAt, LastReadyMs: s.ReadyMs,
				LastState: s.LastState, PricePerMB: s.PricePerMB, PriceKnown: s.PriceKnown}
			byID[s.PeerID] = e
			order = append(order, s.PeerID)
		}
		if e.Addr == "" {
			e.Addr = s.Addr
		}
		if !e.PriceKnown && s.PriceKnown {
			e.PricePerMB, e.PriceKnown = s.PricePerMB, true
		}
		if s.RemainingAt > e.RemainingAt {
			e.Remaining, e.RemainingAt = s.Remaining, s.RemainingAt
		}
		e.Sessions++
		e.BytesRx += s.BytesRx
		e.BytesTx += s.BytesTx
		e.Refused += s.Refused
		for reason, c := range s.RefusedBy {
			if e.RefusedBy == nil {
				e.RefusedBy = map[string]uint64{}
			}
			e.RefusedBy[reason] += c
		}
		e.Active = e.Active || s.Active
	}
	for id := range labels {
		if byID[id] == nil {
			byID[id] = &SavedExit{PeerID: id}
			order = append(order, id)
		}
	}
	out := make([]SavedExit, 0, len(order))
	for _, id := range order {
		e := byID[id]
		e.Name, e.Favorite = labels[id].Name, labels[id].Favorite
		out = append(out, *e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Favorite != out[j].Favorite {
			return out[i].Favorite
		}
		return out[i].LastUsed > out[j].LastUsed
	})
	n.locateExits(out)
	return out
}

// newSessionID is random: the clock alone repeats on Windows when sessions start back to back.
func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
