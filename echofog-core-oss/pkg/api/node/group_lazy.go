package node

import (
	"math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Lazy push in big groups (Plumtree's idea, on our logs). An event is pushed to a few members
// (pushFanout, then spreadFanout per hop): enough to reach everybody in a few hops, but a push
// lost on the way left a hole until the next anti-entropy round. So whoever pushes or passes an
// event on also announces, a moment later and for all the events of that moment at once, the
// newest seq it holds of their authors to more connected members (lazyFanout); one who lacks
// something asks the announcer (Want), at most once per group every askPause. An announcement is
// a few dozen bytes; the events themselves still travel once per member.

const (
	lazyFanout = 12              // connected members an announcement goes to
	lazyDelay  = 2 * time.Second // events announced together; also bounds the rate
	askPause   = 3 * time.Second // a group's lack asked of one announcer at a time
)

type lazyBatch struct {
	authors map[types.PeerID]bool
	timer   *time.Timer
}

// announce notes that we hold a new event of author in a big group; the announcement follows.
func (f *convsFacade) announce(groupID string, author types.PeerID) {
	gf, err := f.n.groupsFacade()
	if err != nil {
		return
	}
	members, err := gf.mgr.GetAuthorizedRecipients(groupID)
	if err != nil || len(members) <= meshLimit {
		return // a small group pushes to everybody
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lazy == nil {
		f.lazy = map[string]*lazyBatch{}
	}
	b := f.lazy[groupID]
	if b == nil {
		b = &lazyBatch{authors: map[types.PeerID]bool{}}
		f.lazy[groupID] = b
		b.timer = time.AfterFunc(lazyDelay, func() { f.flushAnnounce(groupID) })
	}
	b.authors[author] = true
}

func (f *convsFacade) flushAnnounce(groupID string) {
	f.mu.Lock()
	b := f.lazy[groupID]
	delete(f.lazy, groupID)
	f.mu.Unlock()
	if b == nil || len(b.authors) == 0 {
		return
	}
	gf, err := f.n.groupsFacade()
	if err != nil || gf.connected == nil {
		return
	}
	authors := make([]types.PeerID, 0, len(b.authors))
	for a := range b.authors {
		authors = append(authors, a)
	}
	msg := f.log.Announce(convlog.Group(groupID), authors)
	members, err := gf.mgr.GetAuthorizedRecipients(groupID)
	if err != nil {
		return
	}
	rand.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
	sent := 0
	for _, m := range members {
		if sent == lazyFanout {
			break
		}
		if m.PeerID == f.self || !gf.connected(m.PeerID) {
			continue
		}
		f.sendSync(m.PeerID, msg)
		sent++
	}
}

// askAnnouncer reports whether a lack an announcement showed in c may be asked of from now: once
// per askPause per group, so twelve announcements of one event bring one answer, not twelve.
func (f *convsFacade) askAnnouncer(c convlog.Conv) bool {
	k := "ihave/" + c.ID.String()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wantAsked == nil {
		f.wantAsked = map[string]time.Time{}
	}
	if time.Since(f.wantAsked[k]) < askPause {
		return false
	}
	f.wantAsked[k] = time.Now()
	return true
}
