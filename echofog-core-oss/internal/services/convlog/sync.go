package convlog

import (
	"bytes"
	"encoding/hex"

	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

const (
	// syncPageBytes bounds one Events message (it travels as one chat message, possibly through
	// a relay mailbox, whose items are capped at 1 MB).
	syncPageBytes = 200 << 10
	// syncAnswerBytes bounds the answer to one Want; the asker asks again for the rest.
	syncAnswerBytes = 2 << 20
)

// Resolver maps a conversation id a peer names to the local conversation, for groups (direct
// chats are resolved by the log itself). It reports false for a conversation we are not in.
type Resolver func(ConvID) (Conv, bool)

// EncodeSync serialises a sync message.
func EncodeSync(m *Sync) ([]byte, error) { return proto.Marshal(m) }

// DecodeSync parses a sync message.
func DecodeSync(raw []byte) (*Sync, error) {
	var m Sync
	if err := proto.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Hello is the opening of a sync with a peer: one fingerprint per conversation we share.
func (l *Log) Hello(convs []Conv) *Sync {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := &Hello{}
	for _, c := range convs {
		bc := l.bc(c.ID)
		meta := l.loadMeta(bc, c)
		id := c.ID
		h.Items = append(h.Items, &HelloItem{Conv: id[:], Digest: meta.Digest()})
	}
	return &Sync{M: &Sync_Hello{Hello: h}}
}

func (l *Log) resolve(from types.PeerID, id []byte, groups Resolver) (Conv, bool) {
	var cid ConvID
	if len(id) != convIDLen {
		return Conv{}, false
	}
	copy(cid[:], id)
	if dm := DMConv(l.self, from); dm == cid {
		return DM(l.self, from), true
	}
	if groups != nil {
		if c, ok := groups(cid); ok && c.Kind == KindGroup {
			return c, true
		}
	}
	return Conv{}, false
}

// HandleSync answers a sync message from a peer. The replies go back to that peer.
func (l *Log) HandleSync(from types.PeerID, m *Sync, groups Resolver) []*Sync {
	switch x := m.M.(type) {
	case *Sync_Hello:
		var out []*Sync
		for _, it := range x.Hello.GetItems() {
			c, ok := l.resolve(from, it.Conv, groups)
			if !ok || !l.Participant(c, from) {
				continue
			}
			l.mu.Lock()
			meta := l.loadMeta(l.bc(c.ID), c)
			same := bytes.Equal(meta.Digest(), it.Digest)
			want := l.wantLocked(c, meta)
			l.mu.Unlock()
			if !same {
				out = append(out, want)
			}
		}
		return out
	case *Sync_Ihave:
		c, ok := l.resolve(from, x.Ihave.GetConv(), groups)
		if !ok || !l.Participant(c, from) {
			return nil
		}
		if l.Behind(c, x.Ihave.GetHave()) {
			return []*Sync{l.Want(c)}
		}
		return nil
	case *Sync_Want:
		c, ok := l.resolve(from, x.Want.GetConv(), groups)
		if !ok || !l.Participant(c, from) {
			return nil
		}
		return l.answer(c, x.Want.GetHave())
	case *Sync_Events:
		c, ok := l.resolve(from, x.Events.GetConv(), groups)
		if !ok || !l.Participant(c, from) {
			return nil
		}
		l.mu.Lock()
		before := l.loadMeta(l.bc(c.ID), c).Digest()
		l.mu.Unlock()
		for _, raw := range x.Events.GetEvents() {
			_, _, _ = l.ingest(c, raw, false) // a bad event does not stop the others
		}
		l.RebuildIfNeeded(c)
		if x.Events.GetMore() {
			l.mu.Lock()
			meta := l.loadMeta(l.bc(c.ID), c)
			progressed := !bytes.Equal(meta.Digest(), before)
			want := l.wantLocked(c, meta)
			l.mu.Unlock()
			if !progressed {
				// the page held nothing we could take: asking again would bring the same page
				return nil
			}
			return []*Sync{want}
		}
	}
	return nil
}

// Want asks a peer for what we lack in c.
func (l *Log) Want(c Conv) *Sync {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wantLocked(c, l.loadMeta(l.bc(c.ID), c))
}

func (l *Log) wantLocked(c Conv, meta *Meta) *Sync {
	w := &Want{Conv: append([]byte(nil), c.ID[:]...)}
	for author, seq := range meta.VV {
		b, err := hex.DecodeString(author)
		if err != nil || seq == 0 {
			continue
		}
		w.Have = append(w.Have, &Have{Author: b, Seq: seq})
	}
	return &Sync{M: &Sync_Want{Want: w}}
}

// answer sends the asker every event we hold above what it has, author by author.
func (l *Log) answer(c Conv, have []*Have) []*Sync {
	theirs := map[string]uint64{}
	for _, h := range have {
		theirs[hex.EncodeToString(h.GetAuthor())] = h.GetSeq()
	}
	l.mu.Lock()
	bc := l.bc(c.ID)
	meta := l.loadMeta(bc, c)
	l.mu.Unlock()

	var out []*Sync
	page := &Events{Conv: append([]byte(nil), c.ID[:]...)}
	size, total := 0, 0
	flush := func() {
		if len(page.Events) > 0 {
			out = append(out, &Sync{M: &Sync_Events{Events: page}})
		}
		page = &Events{Conv: append([]byte(nil), c.ID[:]...)}
		size = 0
	}
	add := func(raw []byte) bool {
		if total+len(raw) > syncAnswerBytes {
			return false
		}
		if size+len(raw) > syncPageBytes {
			flush()
		}
		page.Events = append(page.Events, raw)
		size += len(raw)
		total += len(raw)
		return true
	}

	// membership first: it decides what the other events may do, so they apply right the first time
	// instead of waiting for a rebuild
	sent := map[string]bool{}
	full := false
	if c.Kind == KindGroup {
		if entries, err := l.st.FindValues(memberPrefix(bc), 0); err == nil {
			for _, en := range entries {
				e, _, err := Decode(en.Value)
				if err != nil || e.Seq <= theirs[peerHex(AuthorID(e))] {
					continue
				}
				if !add(en.Value) {
					full = true
					break
				}
				sent[string(en.Value)] = true
			}
		}
	}
	for author, max := range meta.Max {
		if full {
			break
		}
		from := theirs[author]
		if max <= from {
			continue
		}
		ba := l.ba(author)
		entries, err := l.st.FindValuesSeek(eventPrefix(bc, ba), eventKey(bc, ba, from+1), 0)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if sent[string(e.Value)] {
				continue
			}
			if !add(e.Value) {
				full = true
				break
			}
		}
		if full {
			break
		}
	}
	if !full && len(meta.Forks) > 0 {
		if forks, err := l.st.FindValues(forkPrefix(bc), 0); err == nil {
			for _, e := range forks {
				if !add(e.Value) {
					full = true
					break
				}
			}
		}
	}
	flush()
	if full && len(out) > 0 {
		out[len(out)-1].GetEvents().More = true
	}
	return out
}

// Announce is an IHave of c: the newest seq held of each of authors (peer ids).
func (l *Log) Announce(c Conv, authors []types.PeerID) *Sync {
	l.mu.Lock()
	defer l.mu.Unlock()
	meta := l.loadMeta(l.bc(c.ID), c)
	ih := &IHave{Conv: append([]byte(nil), c.ID[:]...)}
	for _, a := range authors {
		if seq := meta.Max[peerHex(a)]; seq > 0 {
			ih.Have = append(ih.Have, &Have{Author: append([]byte(nil), a[:]...), Seq: seq})
		}
	}
	return &Sync{M: &Sync_Ihave{Ihave: ih}}
}

// Behind reports whether a peer holds events of c we lack, by what it announced.
func (l *Log) Behind(c Conv, have []*Have) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	meta := l.loadMeta(l.bc(c.ID), c)
	for _, h := range have {
		if len(h.GetAuthor()) != len(types.PeerID{}) {
			continue
		}
		if meta.VV[hex.EncodeToString(h.GetAuthor())] < h.GetSeq() {
			return true
		}
	}
	return false
}
