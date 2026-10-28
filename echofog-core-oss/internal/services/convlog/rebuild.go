package convlog

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"sort"

	"github.com/echofog/echofog-core/internal/storage"
)

// A rebuild computes a conversation's views again from its stored events, in log order. It runs
// when a membership change arrives that sits before events already applied: what those events
// were allowed to do may have changed. The events themselves (and the legacy history) are kept;
// only what is derived from them is written again. Membership changes are rare, so is this.

// rebuildBatch bounds the writes of one storage transaction during a rebuild.
const rebuildBatch = 2000

type storedEvent struct {
	e    *Event
	id   EventID
	raw  []byte
	fork bool
}

// RebuildIfNeeded runs a pending rebuild of c (one that sync deferred to the end of its page).
func (l *Log) RebuildIfNeeded(c Conv) {
	l.mu.Lock()
	bc := l.bc(c.ID)
	var ch *Change
	if l.st.Exists(metaKey(bc)) && l.loadMeta(bc, c).Rebuild {
		ch, _ = l.rebuildLocked(c, bc)
	}
	l.mu.Unlock()
	l.notify(ch)
}

func (l *Log) writeChunked(sets []storage.Entry, dels [][]byte) error {
	for len(dels) > 0 {
		n := min(len(dels), rebuildBatch)
		if err := storage.WriteBatch(l.st, nil, dels[:n]); err != nil {
			return err
		}
		dels = dels[n:]
	}
	for len(sets) > 0 {
		n := min(len(sets), rebuildBatch)
		if err := storage.WriteBatch(l.st, sets[:n], nil); err != nil {
			return err
		}
		sets = sets[n:]
	}
	return nil
}

// rebuildLocked recomputes every view of c. Call with l.mu held. The meta keeps Rebuild set until
// the last write, so a crash in between rebuilds again on the next membership event or sync.
func (l *Log) rebuildLocked(c Conv, bc string) (*Change, error) {
	old := l.loadMeta(bc, c)
	old.Rebuild = true
	if raw, err := json.Marshal(old); err == nil {
		_ = l.st.Set(metaKey(bc), raw)
	}

	// the stored events, main and forked
	var evs []storedEvent
	collect := func(prefix []byte, fork bool) error {
		entries, err := l.st.FindValues(prefix, 0)
		if err != nil {
			return err
		}
		for _, en := range entries {
			e, id, err := Decode(en.Value)
			if err != nil {
				continue
			}
			evs = append(evs, storedEvent{e: e, id: id, raw: en.Value, fork: fork})
		}
		return nil
	}
	if err := collect(bs(pfxEvent+bc+"/"), false); err != nil {
		return nil, err
	}
	if err := collect(forkPrefix(bc), true); err != nil {
		return nil, err
	}
	sort.Slice(evs, func(i, j int) bool {
		a, b := pos{evs[i].e.Lamport, evs[i].id.String()}, pos{evs[j].e.Lamport, evs[j].id.String()}
		return a.less(b)
	})

	// the legacy history has no events: keep its messages and their places, without the
	// operations on them (the replay applies those again)
	legacy := map[string]*View{}
	if entries, err := l.st.FindValues(bs(pfxView+bc+"/"), 0); err == nil {
		for _, en := range entries {
			var v View
			if json.Unmarshal(en.Value, &v) == nil && v.Legacy && v.Present {
				v.Edits, v.EditRefs, v.Deletes, v.Reacts, v.Pin = nil, nil, nil, nil, nil
				v.Replies, v.LastReplyTS = 0, 0
				legacy[v.ID] = &v
			}
		}
	}
	var dels [][]byte
	var legacyOrder []storage.Entry
	for _, pfx := range [][]byte{bs(pfxView + bc + "/"), bs(pfxThread + bc + "/"), bs(pfxPin + bc + "/"), bs(pfxIndex + bc + "/"),
		bs(pfxChanO + bc + "/"), channelPrefix(bc)} {
		entries, err := l.st.FindValues(pfx, 0)
		if err != nil {
			return nil, err
		}
		for _, en := range entries {
			dels = append(dels, en.Key)
		}
	}
	if entries, err := l.st.FindValues(orderPrefix(bc), 0); err == nil {
		for _, en := range entries {
			if _, ok := legacy[string(en.Value)]; ok {
				legacyOrder = append(legacyOrder, en)
				continue
			}
			dels = append(dels, en.Key)
		}
	}
	if err := l.writeChunked(nil, dels); err != nil {
		return nil, err
	}

	meta := newMeta(c)
	meta.ReadLamport, meta.OwnSeq = old.ReadLamport, old.OwnSeq
	for id, st := range old.Chans {
		meta.chan_(id).ReadLamport = st.ReadLamport
	}
	t := l.newTxn(c, bc, meta)
	t.synced = true // the unread count is computed afresh below
	for id, v := range legacy {
		t.views[id] = v
		t.order = append(t.order, id)
		if v.Lamport > meta.MaxLamport {
			meta.MaxLamport = v.Lamport
		}
	}
	delete(l.members, bc) // loaded again from the stored membership events
	for _, se := range evs {
		mode := modeReplay
		if se.fork {
			mode = modeReplayFork
		}
		err := l.apply(t, se.e, se.id, se.raw, false, mode)
		rejected := err != nil
		if rejected {
			l.keep(t, se.e, se.id.String(), se.raw, se.raw, mode)
			if err != ErrClock && se.e.Lamport > meta.HeldLamport {
				meta.HeldLamport = se.e.Lamport
			}
		}
		t.put(indexKey(bc, se.id.String()), l.marker(se.e, rejected))
	}
	// pins from the final views: one transaction of puts and deletes would not keep their order
	kept := t.sets[:0]
	for _, en := range t.sets {
		if !hasPrefix(en.Key, pinPrefix(bc)) {
			kept = append(kept, en)
		}
	}
	t.sets = append(kept, legacyOrder...)
	for _, id := range t.order {
		v := t.views[id]
		if del, _ := v.deleted(); v.Present && !del && v.Pin != nil && v.Pin.On {
			t.put(pinKey(bc, id), []byte(id))
		}
	}
	t.dels = nil
	if meta.LastID == "" {
		for id, v := range legacy {
			if meta.LastID == "" || v.TS > meta.LastTS {
				meta.LastID, meta.LastTS = id, v.TS
			}
		}
	}
	meta.Rebuild = false

	sets := append([]storage.Entry(nil), t.sets...)
	for _, id := range t.order {
		raw, err := json.Marshal(t.views[id])
		if err != nil {
			return nil, err
		}
		sets = append(sets, storage.Entry{Key: viewKey(bc, id), Value: raw})
	}
	if err := l.writeChunked(sets, t.dels); err != nil {
		return nil, err
	}
	l.countUnread(bc, meta)
	for id, st := range meta.Chans {
		st.Unread = l.countAbove(chanOrderPrefix(bc, id), st.ReadLamport, bc, meta.Conv)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err := l.st.Set(metaKey(bc), raw); err != nil {
		return nil, err
	}
	// the app reloads the whole conversation: every message may have changed
	return &Change{Conv: c, Meta: *meta, Rebuilt: true, Rev: l.nextRev()}, nil
}

func hasPrefix(b, p []byte) bool { return bytes.HasPrefix(b, p) }

// countUnread sets meta.Unread to the top-level messages of others above the read point.
func (l *Log) countUnread(bc string, meta *Meta) {
	entries, err := l.st.FindValuesSeek(orderPrefix(bc), orderPrefix(bc), 10000)
	if err != nil {
		return
	}
	n := 0
	p := len(orderPrefix(bc))
	for _, e := range entries {
		if len(e.Key) < p+8 {
			continue
		}
		if ^binary.BigEndian.Uint64(e.Key[p:p+8]) <= meta.ReadLamport {
			break
		}
		if m, ok := l.render(bc, meta.Conv, string(e.Value)); ok && !m.Mine {
			n++
		}
	}
	meta.Unread = n
}
