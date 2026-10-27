package storage

import "github.com/dgraph-io/badger/v4"

// Batcher is a store that can write several keys at once: all of them or none.
type Batcher interface {
	ApplyBatch(sets []Entry, deletes [][]byte) error
}

// WriteBatch writes sets and deletes in one atomic step when st can, and otherwise one by one in
// the order given, so a caller that orders its writes from "data" to "marker" stays safe to
// re-run after a crash in between.
func WriteBatch(st Storage, sets []Entry, deletes [][]byte) error {
	if b, ok := st.(Batcher); ok {
		return b.ApplyBatch(sets, deletes)
	}
	for _, e := range sets {
		if err := st.Set(e.Key, e.Value); err != nil {
			return err
		}
	}
	for _, k := range deletes {
		if err := st.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// ApplyBatch writes sets and deletes in one transaction.
func (s *BadgerStorage) ApplyBatch(sets []Entry, deletes [][]byte) error {
	if !s.open() {
		return ErrClosed
	}
	defer s.mu.RUnlock()
	return s.db.Update(func(txn *badger.Txn) error {
		for _, e := range sets {
			if err := txn.Set(e.Key, e.Value); err != nil {
				return err
			}
		}
		for _, k := range deletes {
			if err := txn.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// ApplyBatch seals the values that need it and writes everything in one step when the storage
// underneath can.
func (s *Sealed) ApplyBatch(sets []Entry, deletes [][]byte) error {
	out := make([]Entry, 0, len(sets))
	for _, e := range sets {
		v := e.Value
		if isSealed(e.Key) {
			sealed, err := s.seal(e.Key, v)
			if err != nil {
				return err
			}
			v = sealed
		}
		out = append(out, Entry{Key: e.Key, Value: v})
	}
	return WriteBatch(s.inner, out, deletes)
}
