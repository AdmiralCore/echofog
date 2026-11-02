package call

import (
	"time"

	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/videolayer"
)

// Temporal layers per child (media/layers.go does the cutting). Every node that sends a stream on
// keeps, for each child, the highest layer step its path carries, from the child's own path
// reports (feedback.go): reports heavy again and again while this node's own path is not mean
// the hop to the child cannot carry the stream, and the child loses the top layer, then the
// middle one, down to the base at a quarter of the frame rate. After a while clean it gets a
// layer back; a layer it loses again soon after makes the next try wait twice as long.
//
// Only a child that says in its reports that it reads layer caps, and whose own children all do,
// is ever cut: an older node, or one with an older node below it, gets the whole stream.
const (
	layerHeavyOf   = 3               // reports kept per child ...
	layerHeavy     = 2               // ... of which this many heavy or worse take a layer away
	layerHold      = time.Second     // after a change, before the next layer goes: it takes a base frame and a report to show
	layerUpFirst   = 4 * time.Second // clean this long gives a layer back
	layerUpMax     = 32 * time.Second
	layerUpFailed  = 5 * time.Second  // a layer lost again this soon after it came back doubles the wait
	layerUpForgive = 30 * time.Second // a layer kept this long sets the wait back to layerUpFirst
	layerStale     = 2 * time.Second  // a child silent this long gets every layer back
)

// childLayers is what a node keeps of one child's layers on one stream.
type childLayers struct {
	aware    bool // the child reads caps, and every child of its does
	reportAt time.Time
	grades   [layerHeavyOf]media.Congestion
	n        int
	fresh    bool // a report came since the last step

	cap        uint8
	changedAt  time.Time
	raisedAt   time.Time
	cleanSince time.Time
	upWait     time.Duration
}

func newChildLayers() *childLayers {
	return &childLayers{cap: videolayer.All, upWait: layerUpFirst}
}

// note takes the child's own report.
func (c *childLayers) note(level media.Congestion, aware bool, now time.Time) {
	c.grades[c.n%layerHeavyOf] = level
	c.n++
	c.aware, c.reportAt, c.fresh = aware, now, true
	if level > media.PathHold {
		c.cleanSince = time.Time{}
	} else if c.cleanSince.IsZero() {
		c.cleanSince = now
	}
}

// step moves the cap at most one step and reports whether it changed. own is this node's own
// path on the stream (media.PathClean for its broadcaster); top is the highest step the stream
// has (media's StreamLayers).
func (c *childLayers) step(own media.Congestion, top uint8, now time.Time) bool {
	if !c.fresh || c.n == 0 {
		return false
	}
	c.fresh = false
	heavy := 0
	for _, g := range c.grades[:min(c.n, layerHeavyOf)] {
		if g >= media.PathHeavy {
			heavy++
		}
	}
	latest := c.grades[(c.n-1)%layerHeavyOf]
	if c.upWait > layerUpFirst && !c.raisedAt.IsZero() && now.Sub(c.raisedAt) > layerUpForgive {
		c.upWait = layerUpFirst
	}
	switch {
	case heavy >= layerHeavy && latest >= media.PathHeavy && own < media.PathHeavy && c.cap != 0 && top > 0 &&
		now.Sub(c.changedAt) >= layerHold:
		// the hop to the child is what is short: the trouble is not ours as well
		if !c.raisedAt.IsZero() && now.Sub(c.raisedAt) < layerUpFailed {
			c.upWait = min(2*c.upWait, layerUpMax)
		}
		c.cap = down(c.cap, top)
		c.changedAt, c.cleanSince, c.raisedAt = now, time.Time{}, time.Time{}
		c.n = 0
		return true
	case c.cap != videolayer.All && !c.cleanSince.IsZero() && now.Sub(c.cleanSince) >= c.upWait &&
		now.Sub(c.changedAt) >= c.upWait:
		c.cap = up(c.cap, top)
		c.changedAt, c.raisedAt, c.cleanSince = now, now, now
		return true
	}
	return false
}

// down and up move a cap one step of the ladder of a stream whose highest step is top: a stream of
// two layers goes from all of them to the base at once.
func down(c, top uint8) uint8 {
	if c == videolayer.All && top >= 2 {
		return 1
	}
	return 0
}

func up(c, top uint8) uint8 {
	if c == 0 && top >= 2 {
		return 1
	}
	return videolayer.All
}

// noteLayersLocked keeps what a child's report says of its layers: whether it reads caps, and its
// own path; m.mu must be held.
func (m *CallManager) noteLayersLocked(room *ActiveRoomState, from types.PeerID, fp signaling.StreamFeedbackPayload, now time.Time) {
	for _, r := range fp.Reports {
		if r.Viewer != from {
			continue
		}
		kids := room.layers[fp.StreamID]
		if kids == nil {
			kids = make(map[types.PeerID]*childLayers)
			room.layers[fp.StreamID] = kids
		}
		c := kids[from]
		if c == nil {
			c = newChildLayers()
			kids[from] = c
		}
		c.note(min(media.Congestion(r.Level), media.PathFlood), fp.Layered, now)
		return
	}
}

// layeredBelowLocked reports whether every child of ours on a stream reads caps: only then may our
// parent cut our layers, as what it leaves out we cannot give them.
func (m *CallManager) layeredBelowLocked(room *ActiveRoomState, streamID string, now time.Time) bool {
	if m.beforeLayers {
		return false
	}
	kids := room.layers[streamID]
	for _, child := range room.StreamEngine.GetDownstream(streamID, m.selfID) {
		c := kids[child]
		if c == nil || !c.aware || now.Sub(c.reportAt) > layerStale {
			return false
		}
	}
	return true
}

// layersTickLocked steps the cap of every child of every stream this node sends on and hands it
// to the dispatcher.
func (m *CallManager) layersTickLocked(room *ActiveRoomState, now time.Time) {
	d := room.Dispatcher
	if d == nil {
		return
	}
	for id, kids := range room.layers {
		children := room.StreamEngine.GetDownstream(id, m.selfID)
		if len(children) == 0 {
			delete(room.layers, id)
			continue
		}
		isChild := make(map[types.PeerID]bool, len(children))
		for _, c := range children {
			isChild[c] = true
		}
		top, _ := d.StreamLayers(id)
		own := room.ownLevel[id]
		if _, local := room.localStreams[id]; local {
			own = media.PathClean
		}
		for child, c := range kids {
			if !isChild[child] {
				delete(kids, child)
				continue
			}
			aware := c.aware && now.Sub(c.reportAt) <= layerStale
			if !aware && c.cap != videolayer.All {
				c.cap, c.upWait = videolayer.All, layerUpFirst
				m.logger.Info("A child of a stream stopped reporting: it gets every layer from the next base frame",
					"stream", id, "child", calllog.Peer(child))
			}
			if aware && c.step(own, top, now) {
				m.logger.Info("Temporal layers of a child of a stream changed", "stream", id, "child", calllog.Peer(child),
					"cap", capName(c.cap), "next_try_s", int(c.upWait/time.Second))
			}
			d.SetChildLayers(id, child, aware, c.cap)
		}
	}
}

// capName names a cap in logs.
func capName(c uint8) string {
	switch c {
	case 0:
		return "base"
	case 1:
		return "base+middle"
	}
	return "all"
}
