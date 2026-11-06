package chat

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// Limits, lifetimes and counters (AD19): the counters change only where an item enters or leaves the
// active set, and the same places that delete a record subtract it.

const (
	// itemOverhead is what an outbox record costs besides its plaintext and envelope.
	itemOverhead = 256

	// outboxLife: a message that has not been delivered after this long is failed (the longest a mailbox
	// would keep it, services.md 5.2). A receipt is worth much less: a day.
	outboxLife  = 7 * 24 * time.Hour
	receiptLife = 24 * time.Hour

	seenIndexPrefix  = "seenx:" // seenx:<expiry 8><hash 32> -> device
	seenDevicePrefix = "seend:" // seend:<device 32><expiry 8><hash 32>
)

func (c ServiceConfig) maxOutbox() int { return orDefault(c.MaxOutbox, DefaultMaxOutbox) }
func (c ServiceConfig) maxPerPeer() int {
	return orDefault(c.MaxOutboxPerPeer, DefaultMaxOutboxPerPeer)
}
func (c ServiceConfig) maxBytes() int { return orDefault(c.MaxOutboxBytes, DefaultMaxOutboxBytes) }
func (c ServiceConfig) maxSeen() int  { return orDefault(c.MaxSeen, DefaultMaxSeen) }
func (c ServiceConfig) maxSeenDev() int {
	return orDefault(c.MaxSeenPerDevice, DefaultMaxSeenPerDevice)
}

func orDefault(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

// active reports whether an item is still being sent.
func (it *item) active() bool { return it.State == StateWaiting || it.State == StateMailbox }

func (it *item) size() int { return len(it.Plain) + len(it.Env) + itemOverhead }

// addLocked starts counting an active item. s.mu is held.
func (s *Service) addLocked(it *item) {
	s.count++
	s.perPeer[it.To]++
	if it.Receipt {
		s.receipts[it.To]++
	}
	it.acct = it.size()
	s.bytes += it.acct
}

// dropLocked stops counting an item that was counted. s.mu is held.
func (s *Service) dropLocked(it *item) {
	if it.acct == 0 {
		return // not counted
	}
	s.count--
	if s.perPeer[it.To]--; s.perPeer[it.To] <= 0 {
		delete(s.perPeer, it.To)
	}
	if it.Receipt {
		if s.receipts[it.To]--; s.receipts[it.To] <= 0 {
			delete(s.receipts, it.To)
		}
	}
	s.bytes -= it.acct
	it.acct = 0
}

// resizeLocked follows a change of the plaintext or envelope of a counted item. s.mu is held.
func (s *Service) resizeLocked(it *item) {
	if it.acct == 0 {
		return
	}
	s.bytes += it.size() - it.acct
	it.acct = it.size()
}

// removeLocked deletes an item for good. s.mu is held.
func (s *Service) removeLocked(it *item) {
	s.dropLocked(it)
	delete(s.items, it.ID)
	_ = s.cfg.Store.Delete(outKey(it.ID))
}

// admitLocked says whether a new item fits the limits. s.mu is held.
func (s *Service) admitLocked(it *item) bool {
	switch {
	case s.count >= s.cfg.maxOutbox(),
		s.perPeer[it.To] >= s.cfg.maxPerPeer(),
		s.bytes+it.size() > s.cfg.maxBytes():
		return false
	case it.Receipt && s.receipts[it.To] >= MaxReceiptsPerPeer:
		return false
	}
	return true
}

// Stats returns the maintained counters.
func (s *Service) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Outbox: s.count, Bytes: s.bytes, Seen: s.seenN, PerPeer: map[[32]byte]int{}}
	for k, v := range s.perPeer {
		st.PerPeer[k] = v
	}
	return st
}

// Recount counts the same things again from the store; the two must always agree (AD19).
func (s *Service) Recount() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{PerPeer: map[[32]byte]int{}}
	_ = s.scan(outPrefix, func(e store.Entry) {
		var it item
		if json.Unmarshal(e.Value, &it) != nil || !it.active() {
			return
		}
		st.Outbox++
		st.PerPeer[it.To]++
		st.Bytes += it.size()
	})
	_ = s.scan(seenIndexPrefix, func(store.Entry) { st.Seen++ })
	return st
}

// scan walks every record under prefix.
func (s *Service) scan(prefix string, fn func(store.Entry)) error {
	var after []byte
	for {
		var (
			page []store.Entry
			err  error
		)
		if after == nil {
			page, err = s.cfg.Store.Scan([]byte(prefix), store.MaxScanLimit)
		} else {
			page, err = s.cfg.Store.ScanFrom([]byte(prefix), after, store.MaxScanLimit)
		}
		if err != nil {
			return err
		}
		for _, e := range page {
			fn(e)
		}
		if len(page) < store.MaxScanLimit {
			return nil
		}
		after = page[len(page)-1].Key
	}
}

// ---- remembered message IDs: a logical index of expiry times (AD19), a cap in all and per device ----

func seenHash(device [32]byte, id []byte) [32]byte {
	return sha256.Sum256(append(append([]byte(nil), device[:]...), id...))
}

func seenRecordKey(h [32]byte) []byte { return []byte(seenPrefix + hex.EncodeToString(h[:])) }

func seenKeys(device [32]byte, expiry uint64, h [32]byte) (index, dev []byte) {
	index = append(binary.BigEndian.AppendUint64([]byte(seenIndexPrefix), expiry), h[:]...)
	dev = append(binary.BigEndian.AppendUint64(append([]byte(seenDevicePrefix), device[:]...), expiry), h[:]...)
	return index, dev
}

// seenBefore reports whether the ID was accepted and is still remembered.
func (s *Service) seenBefore(device [32]byte, id []byte) bool {
	ok, _ := s.cfg.Store.Has(seenRecordKey(seenHash(device, id)))
	return ok
}

// rememberSeen records an accepted ID until its logical end, making room first when a cap is reached:
// the oldest record of the device goes before the oldest of all.
func (s *Service) rememberSeen(device [32]byte, id []byte) {
	h := seenHash(device, id)
	rec := seenRecordKey(h)
	if ok, _ := s.cfg.Store.Has(rec); ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.seenDev[device] >= s.cfg.maxSeenDev() {
		if !s.forgetOldestLocked([]byte(seenDevicePrefix + string(device[:]))) {
			break
		}
	}
	for s.seenN >= s.cfg.maxSeen() {
		if !s.forgetOldestLocked([]byte(seenIndexPrefix)) {
			break
		}
	}
	expiry := uint64(s.cfg.Now().Add(seenFor).Unix())
	index, dev := seenKeys(device, expiry, h)
	val := binary.BigEndian.AppendUint64(nil, expiry)
	val = append(val, device[:]...)
	if s.cfg.Store.Put(rec, val) != nil || s.cfg.Store.Put(index, device[:]) != nil || s.cfg.Store.Put(dev, nil) != nil {
		return
	}
	s.seenN++
	s.seenDev[device]++
}

// forgetOldestLocked drops the oldest record under an index prefix and reports whether there was one.
func (s *Service) forgetOldestLocked(prefix []byte) bool {
	page, err := s.cfg.Store.Scan(prefix, 1)
	if err != nil || len(page) == 0 {
		return false
	}
	k := page[0].Key
	var h [32]byte
	copy(h[:], k[len(k)-32:])
	s.forgetLocked(h)
	// a record whose main entry is gone (or never counted) must not stay in the index for ever
	_ = s.cfg.Store.Delete(k)
	return true
}

// forgetLocked deletes a remembered ID. The counters change only when the record was read and found
// (the read-and-delete rule of AD19).
func (s *Service) forgetLocked(h [32]byte) {
	rec := seenRecordKey(h)
	val, err := s.cfg.Store.Get(rec)
	if err != nil || len(val) != 8+32 {
		return
	}
	expiry := binary.BigEndian.Uint64(val[:8])
	var device [32]byte
	copy(device[:], val[8:])
	index, dev := seenKeys(device, expiry, h)
	_ = s.cfg.Store.Delete(rec)
	_ = s.cfg.Store.Delete(index)
	_ = s.cfg.Store.Delete(dev)
	if s.seenN > 0 {
		s.seenN--
	}
	if s.seenDev[device]--; s.seenDev[device] <= 0 {
		delete(s.seenDev, device)
	}
}

// expireSeen drops the remembered IDs whose logical end has passed.
func (s *Service) expireSeen(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		page, err := s.cfg.Store.Scan([]byte(seenIndexPrefix), 64)
		if err != nil || len(page) == 0 {
			return
		}
		done := false
		for _, e := range page {
			k := e.Key
			if len(k) != len(seenIndexPrefix)+8+32 {
				_ = s.cfg.Store.Delete(k)
				continue
			}
			expiry := binary.BigEndian.Uint64(k[len(seenIndexPrefix) : len(seenIndexPrefix)+8])
			if int64(expiry) > now.Unix() {
				done = true
				break
			}
			var h [32]byte
			copy(h[:], k[len(k)-32:])
			s.forgetLocked(h)
			_ = s.cfg.Store.Delete(k)
		}
		if done {
			return
		}
	}
}

// loadSeen counts the remembered IDs once at start.
func (s *Service) loadSeen() error {
	return s.scan(seenIndexPrefix, func(e store.Entry) {
		if len(e.Value) != 32 || !bytes.HasPrefix(e.Key, []byte(seenIndexPrefix)) {
			return
		}
		var device [32]byte
		copy(device[:], e.Value)
		s.seenN++
		s.seenDev[device]++
	})
}
