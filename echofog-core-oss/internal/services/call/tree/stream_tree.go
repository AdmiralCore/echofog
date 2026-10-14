package tree

import (
	"bytes"
	"errors"
	"sort"
	"sync"

	"github.com/echofog/echofog-core/pkg/api/types"
)

var (
	ErrStreamNotFound     = errors.New("stream not found")
	ErrStreamAlreadyExist = errors.New("stream already exists")
	ErrNotSubscribed      = errors.New("peer is not subscribed to this stream")
	ErrCannotAvoidSource  = errors.New("the broadcaster itself cannot be avoided")
)

const (
	// DefaultBroadcasterFanout applies when the broadcaster's upload is unknown.
	DefaultBroadcasterFanout = 3
	// MaxStreamFanout caps direct children of any node, whatever bandwidth it claims.
	MaxStreamFanout = 16
)

// StreamTreeEngine holds on-demand video/screen distribution trees. Only subscribers receive
// a stream's bitrate. The room coordinator is the single writer; other peers apply its manifests.
type StreamTreeEngine struct {
	mu       sync.RWMutex
	streams  map[string]*StreamManifest
	profiles map[types.PeerID]PeerNodeProfile
}

// NewStreamTreeEngine initializes an empty multi-stream tree engine.
func NewStreamTreeEngine() *StreamTreeEngine {
	return &StreamTreeEngine{
		streams:  make(map[string]*StreamManifest),
		profiles: make(map[types.PeerID]PeerNodeProfile),
	}
}

// RelayCapacity returns how many children a node can feed for a stream of the given bitrate:
// bounded by both its declared relay slots and its real upstream bandwidth.
func RelayCapacity(p PeerNodeProfile, bitrateKbps int, isBroadcaster bool) int {
	if isBroadcaster {
		if p.UploadKbps <= 0 || bitrateKbps <= 0 {
			return DefaultBroadcasterFanout
		}
		return clamp(p.UploadKbps/bitrateKbps, 1, MaxStreamFanout)
	}
	if p.IsMobile || p.MaxRelayChildren <= 0 {
		return 0
	}
	if bitrateKbps <= 0 {
		return min(p.MaxRelayChildren, MaxStreamFanout)
	}
	if p.UploadKbps <= 0 {
		return 0 // unknown upstream: never trust it with someone else's video
	}
	return clamp(min(p.UploadKbps/bitrateKbps, p.MaxRelayChildren), 0, MaxStreamFanout)
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

// RegisterStream creates a new active stream broadcast.
func (e *StreamTreeEngine) RegisterStream(streamID string, broadcaster types.PeerID, mediaType string, bitrate int, broadcasterProf PeerNodeProfile) (*StreamManifest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.streams[streamID]; exists {
		return nil, ErrStreamAlreadyExist
	}
	broadcasterProf.PeerID = broadcaster
	e.profiles[broadcaster] = broadcasterProf

	m := &StreamManifest{
		StreamID:         streamID,
		Broadcaster:      broadcaster,
		MediaType:        mediaType,
		BitrateKbps:      bitrate,
		Epoch:            1,
		Subscribers:      make([]types.PeerID, 0),
		DistributionTree: make(map[types.PeerID][]types.PeerID),
		ParentMap:        make(map[types.PeerID]types.PeerID),
		Profiles:         map[types.PeerID]PeerNodeProfile{broadcaster: broadcasterProf},
	}
	e.streams[streamID] = m
	return cloneManifest(m), nil
}

// UnregisterStream removes an ended stream broadcast.
func (e *StreamTreeEngine) UnregisterStream(streamID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.streams, streamID)
}

// UpdatePeerProfile updates telemetry metrics of a peer.
func (e *StreamTreeEngine) UpdatePeerProfile(profile PeerNodeProfile) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.profiles[profile.PeerID] = profile
}

// Subscribe attaches a subscriber without disturbing existing viewers.
func (e *StreamTreeEngine) Subscribe(streamID string, subscriber types.PeerID, profile PeerNodeProfile) (*StreamManifest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	s, ok := e.streams[streamID]
	if !ok {
		return nil, ErrStreamNotFound
	}
	profile.PeerID = subscriber
	e.profiles[subscriber] = profile
	s.Profiles[subscriber] = profile

	if subscriber == s.Broadcaster {
		return cloneManifest(s), nil
	}
	if _, dup := s.ParentMap[subscriber]; dup {
		return cloneManifest(s), nil
	}

	s.Subscribers = append(s.Subscribers, subscriber)
	s.Epoch++
	e.attachLocked(s, subscriber)
	return cloneManifest(s), nil
}

// Unsubscribe removes a subscriber. A leaf is simply detached; a relay triggers a rebuild so
// its orphaned subtree is re-homed.
func (e *StreamTreeEngine) Unsubscribe(streamID string, subscriber types.PeerID) (*StreamManifest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	s, ok := e.streams[streamID]
	if !ok {
		return nil, ErrStreamNotFound
	}
	if !e.removeSubscriberLocked(s, subscriber) {
		return cloneManifest(s), nil
	}
	s.Epoch++
	return cloneManifest(s), nil
}

// Reparent moves a subscriber (with its subtree) away from a relay it reported as dead, lossy or
// abusive. The relay stays on the subscriber's avoid list for later rebuilds too.
func (e *StreamTreeEngine) Reparent(streamID string, sub, avoid types.PeerID) (*StreamManifest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	s, ok := e.streams[streamID]
	if !ok {
		return nil, ErrStreamNotFound
	}
	parent, attached := s.ParentMap[sub]
	if !attached {
		return nil, ErrNotSubscribed
	}
	if avoid == s.Broadcaster {
		return nil, ErrCannotAvoidSource
	}
	if s.Avoid == nil {
		s.Avoid = make(map[types.PeerID][]types.PeerID)
	}
	if !containsPeer(s.Avoid[sub], avoid) {
		s.Avoid[sub] = append(s.Avoid[sub], avoid)
	}
	if parent != avoid && !e.isAncestorLocked(s, avoid, sub) {
		return cloneManifest(s), nil // already fed around that relay
	}

	s.DistributionTree[parent] = removePeer(s.DistributionTree[parent], sub)
	if len(s.DistributionTree[parent]) == 0 {
		delete(s.DistributionTree, parent)
	}
	delete(s.ParentMap, sub)
	s.Overflow = removePeer(s.Overflow, sub)
	e.attachLocked(s, sub)
	s.Epoch++
	return cloneManifest(s), nil
}

func (e *StreamTreeEngine) isAncestorLocked(s *StreamManifest, ancestor, node types.PeerID) bool {
	for i := 0; i <= len(s.Subscribers); i++ {
		p, ok := s.ParentMap[node]
		if !ok {
			return false
		}
		if p == ancestor {
			return true
		}
		node = p
	}
	return false
}

// excludedLocked lists nodes that may not feed sub: its own subtree (that would form a loop) and
// relays it asked to avoid.
func (e *StreamTreeEngine) excludedLocked(s *StreamManifest, sub types.PeerID) map[types.PeerID]bool {
	ex := map[types.PeerID]bool{sub: true}
	queue := []types.PeerID{sub}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, c := range s.DistributionTree[n] {
			if !ex[c] {
				ex[c] = true
				queue = append(queue, c)
			}
		}
	}
	for _, a := range s.Avoid[sub] {
		ex[a] = true
	}
	delete(ex, s.Broadcaster)
	return ex
}

// RemovePeer drops a departed peer from every stream. It returns the streams whose tree
// changed and the streams that ended because the peer was their broadcaster.
func (e *StreamTreeEngine) RemovePeer(peer types.PeerID) (changed []*StreamManifest, closed []string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.profiles, peer)
	for id, s := range e.streams {
		if s.Broadcaster == peer {
			delete(e.streams, id)
			closed = append(closed, id)
			continue
		}
		for sub, list := range s.Avoid {
			s.Avoid[sub] = removePeer(list, peer)
			if len(s.Avoid[sub]) == 0 {
				delete(s.Avoid, sub)
			}
		}
		if e.removeSubscriberLocked(s, peer) {
			s.Epoch++
			changed = append(changed, cloneManifest(s))
		}
	}
	sort.Strings(closed)
	return changed, closed
}

func (e *StreamTreeEngine) removeSubscriberLocked(s *StreamManifest, sub types.PeerID) bool {
	idx := -1
	for i, p := range s.Subscribers {
		if p == sub {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	s.Subscribers = append(s.Subscribers[:idx], s.Subscribers[idx+1:]...)
	delete(s.Profiles, sub)
	delete(s.Avoid, sub)

	if len(s.DistributionTree[sub]) > 0 {
		e.rebuildLocked(s)
		return true
	}
	parent := s.ParentMap[sub]
	delete(s.ParentMap, sub)
	s.DistributionTree[parent] = removePeer(s.DistributionTree[parent], sub)
	if len(s.DistributionTree[parent]) == 0 {
		delete(s.DistributionTree, parent)
	}
	s.Overflow = removePeer(s.Overflow, sub)
	return true
}

// ApplyManifest installs a coordinator-built manifest. Stale epochs are ignored.
func (e *StreamTreeEngine) ApplyManifest(m *StreamManifest) bool {
	if m == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, ok := e.streams[m.StreamID]; ok && cur.Broadcaster == m.Broadcaster && m.Epoch <= cur.Epoch {
		return false
	}
	c := cloneManifest(m)
	if c.Profiles == nil {
		c.Profiles = make(map[types.PeerID]PeerNodeProfile)
	}
	for id, p := range c.Profiles {
		e.profiles[id] = p
	}
	e.streams[m.StreamID] = c
	return true
}

// Snapshot returns copies of every stream manifest, used to resync joiners and new coordinators.
func (e *StreamTreeEngine) Snapshot() []*StreamManifest {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*StreamManifest, 0, len(e.streams))
	for _, s := range e.streams {
		out = append(out, cloneManifest(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StreamID < out[j].StreamID })
	return out
}

// Rebuild recomputes a stream tree from scratch (e.g. after a coordinator takeover).
func (e *StreamTreeEngine) Rebuild(streamID string) (*StreamManifest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.streams[streamID]
	if !ok {
		return nil, ErrStreamNotFound
	}
	s.Epoch++
	e.rebuildLocked(s)
	return cloneManifest(s), nil
}

// GetDownstream returns peers that selfID must forward this stream's packets to.
func (e *StreamTreeEngine) GetDownstream(streamID string, selfID types.PeerID) []types.PeerID {
	e.mu.RLock()
	defer e.mu.RUnlock()

	s, ok := e.streams[streamID]
	if !ok {
		return nil
	}
	children, ok := s.DistributionTree[selfID]
	if !ok {
		return nil
	}
	return append([]types.PeerID(nil), children...)
}

// RelayDuties maps every stream in which selfID feeds somebody to its direct children.
func (e *StreamTreeEngine) RelayDuties(selfID types.PeerID) map[string][]types.PeerID {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string][]types.PeerID)
	for id, s := range e.streams {
		if children := s.DistributionTree[selfID]; len(children) > 0 {
			out[id] = append([]types.PeerID(nil), children...)
		}
	}
	return out
}

// Upstreams maps every stream selfID is attached to onto its current feeder.
func (e *StreamTreeEngine) Upstreams(selfID types.PeerID) map[string]types.PeerID {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]types.PeerID)
	for id, s := range e.streams {
		if p, ok := s.ParentMap[selfID]; ok {
			out[id] = p
		}
	}
	return out
}

// GetUpstream returns the peer from which selfID receives the stream.
func (e *StreamTreeEngine) GetUpstream(streamID string, selfID types.PeerID) (types.PeerID, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	s, ok := e.streams[streamID]
	if !ok {
		return types.PeerID{}, false
	}
	parent, ok := s.ParentMap[selfID]
	return parent, ok
}

// GetStreamManifest returns a copy of the current stream manifest.
func (e *StreamTreeEngine) GetStreamManifest(streamID string) (*StreamManifest, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	s, ok := e.streams[streamID]
	if !ok {
		return nil, false
	}
	return cloneManifest(s), true
}

// ActiveStreams returns all currently active stream IDs.
func (e *StreamTreeEngine) ActiveStreams() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	list := make([]string, 0, len(e.streams))
	for id := range e.streams {
		list = append(list, id)
	}
	sort.Strings(list)
	return list
}

// SetStreamQuality updates quality metadata. A bitrate change alters every relay's capacity,
// so the tree is rebuilt and the epoch bumped.
func (e *StreamTreeEngine) SetStreamQuality(streamID string, q StreamQuality) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	s, ok := e.streams[streamID]
	if !ok {
		return ErrStreamNotFound
	}
	bitrateChanged := s.BitrateKbps != q.BitrateKbps
	s.BitrateKbps, s.Width, s.Height, s.FPS, s.Codec, s.QualityID = q.BitrateKbps, q.Width, q.Height, q.FPS, q.Codec, q.ID
	if bitrateChanged {
		s.Epoch++
		e.rebuildLocked(s)
	}
	return nil
}

func (e *StreamTreeEngine) profileLocked(s *StreamManifest, id types.PeerID) PeerNodeProfile {
	if p, ok := s.Profiles[id]; ok {
		return p
	}
	return e.profiles[id]
}

func (e *StreamTreeEngine) capacityLocked(s *StreamManifest, id types.PeerID) int {
	return RelayCapacity(e.profileLocked(s, id), s.BitrateKbps, id == s.Broadcaster)
}

func (e *StreamTreeEngine) depthLocked(s *StreamManifest, id types.PeerID) int {
	d := 0
	for id != s.Broadcaster && d <= len(s.Subscribers) {
		p, ok := s.ParentMap[id]
		if !ok {
			break
		}
		id = p
		d++
	}
	return d
}

// attachLocked hangs sub under the shallowest node with spare capacity (broadcaster first).
func (e *StreamTreeEngine) attachLocked(s *StreamManifest, sub types.PeerID) {
	excluded := e.excludedLocked(s, sub)
	candidates := make([]types.PeerID, 0, len(s.Subscribers))
	candidates = append(candidates, s.Broadcaster)
	for _, p := range s.Subscribers {
		if _, attached := s.ParentMap[p]; attached && !excluded[p] {
			candidates = append(candidates, p)
		}
	}

	var best types.PeerID
	bestDepth, bestSpare, found := 0, 0, false
	for _, c := range candidates {
		spare := e.capacityLocked(s, c) - len(s.DistributionTree[c])
		if spare <= 0 {
			continue
		}
		d := e.depthLocked(s, c)
		if !found || d < bestDepth || (d == bestDepth && spare > bestSpare) ||
			(d == bestDepth && spare == bestSpare && bytes.Compare(c[:], best[:]) < 0) {
			best, bestDepth, bestSpare, found = c, d, spare, true
		}
	}
	if !found && e.capacityLocked(s, sub) > 0 && e.spliceLocked(s, sub, excluded) {
		return
	}
	if !found {
		// No capacity anywhere: the broadcaster carries the extra viewer, and the UI can see the
		// overflow and suggest a lower quality.
		best = s.Broadcaster
		s.Overflow = append(s.Overflow, sub)
	}
	s.ParentMap[sub] = best
	s.DistributionTree[best] = append(s.DistributionTree[best], sub)
}

// spliceLocked lets a late relay-capable subscriber take the slot of the shallowest leaf, which
// then hangs below it. Without this, leaves that joined first would fill every slot near the
// root and strong relays arriving later would end up as overflow.
func (e *StreamTreeEngine) spliceLocked(s *StreamManifest, sub types.PeerID, excluded map[types.PeerID]bool) bool {
	var leaf, parent types.PeerID
	bestDepth, found := 0, false
	for p, children := range s.DistributionTree {
		if excluded[p] {
			continue
		}
		for _, c := range children {
			if excluded[c] || len(s.DistributionTree[c]) > 0 || e.capacityLocked(s, c) > 0 {
				continue // only pure leaves outside sub's subtree are moved
			}
			if containsPeer(s.Avoid[c], sub) {
				continue // that leaf asked not to be fed by sub
			}
			d := e.depthLocked(s, c)
			if !found || d < bestDepth || (d == bestDepth && bytes.Compare(c[:], leaf[:]) < 0) {
				leaf, parent, bestDepth, found = c, p, d, true
			}
		}
	}
	if !found {
		return false
	}
	siblings := s.DistributionTree[parent]
	for i, c := range siblings {
		if c == leaf {
			siblings[i] = sub
		}
	}
	s.ParentMap[sub] = parent
	s.ParentMap[leaf] = sub
	s.DistributionTree[sub] = append(s.DistributionTree[sub], leaf)
	return true
}

// rebuildLocked rebuilds the tree from scratch: strongest relays first so they sit near the root.
func (e *StreamTreeEngine) rebuildLocked(s *StreamManifest) {
	s.DistributionTree = make(map[types.PeerID][]types.PeerID)
	s.ParentMap = make(map[types.PeerID]types.PeerID)
	s.Overflow = nil

	order := append([]types.PeerID(nil), s.Subscribers...)
	sort.SliceStable(order, func(i, j int) bool {
		ci, cj := e.capacityLocked(s, order[i]), e.capacityLocked(s, order[j])
		if ci != cj {
			return ci > cj
		}
		return bytes.Compare(order[i][:], order[j][:]) < 0
	})
	for _, sub := range order {
		e.attachLocked(s, sub)
	}
}

func containsPeer(list []types.PeerID, p types.PeerID) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}

func removePeer(list []types.PeerID, p types.PeerID) []types.PeerID {
	out := list[:0]
	for _, x := range list {
		if x != p {
			out = append(out, x)
		}
	}
	return out
}

func cloneManifest(m *StreamManifest) *StreamManifest {
	c := *m
	c.Subscribers = append([]types.PeerID(nil), m.Subscribers...)
	c.Overflow = append([]types.PeerID(nil), m.Overflow...)
	c.DistributionTree = make(map[types.PeerID][]types.PeerID, len(m.DistributionTree))
	for k, v := range m.DistributionTree {
		c.DistributionTree[k] = append([]types.PeerID(nil), v...)
	}
	c.ParentMap = make(map[types.PeerID]types.PeerID, len(m.ParentMap))
	for k, v := range m.ParentMap {
		c.ParentMap[k] = v
	}
	c.Profiles = make(map[types.PeerID]PeerNodeProfile, len(m.Profiles))
	for k, v := range m.Profiles {
		c.Profiles[k] = v
	}
	c.Avoid = nil
	if len(m.Avoid) > 0 {
		c.Avoid = make(map[types.PeerID][]types.PeerID, len(m.Avoid))
		for k, v := range m.Avoid {
			c.Avoid[k] = append([]types.PeerID(nil), v...)
		}
	}
	return &c
}
