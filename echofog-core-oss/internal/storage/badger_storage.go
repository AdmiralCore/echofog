package storage

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
)

type BadgerStorage struct {
	db        *badger.DB
	done      chan struct{} // closed by Close: stops the garbage collection loop
	closeOnce sync.Once
}

func NewBadgerStorage(db *badger.DB) *BadgerStorage {
	bs := &BadgerStorage{
		db:   db,
		done: make(chan struct{}),
	}
	bs.GCWorker()
	return bs
}

func (s *BadgerStorage) Update(key, value []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, value)
	})
}

func (s *BadgerStorage) UpdateTTL(key, value []byte, ttl time.Duration) error {
	return s.db.Update(func(txn *badger.Txn) error {
		e := badger.NewEntry(key, value).WithTTL(ttl)

		return txn.SetEntry(e)
	})
}

func (s *BadgerStorage) Read(key []byte) ([]byte, error) {
	var data []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			data = make([]byte, len(val))
			copy(data, val)
			return nil
		})
	})
	return data, err
}

func (s *BadgerStorage) Delete(key []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
}

func (s *BadgerStorage) GCWorker() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
			}
			for s.db.RunValueLogGC(0.5) == nil {
			}
		}
	}()
}

func (s *BadgerStorage) Set(key, value []byte) error {
	return s.Update(key, value)
}

func (s *BadgerStorage) Get(key []byte) ([]byte, error) {
	return s.Read(key)
}

type Entry struct {
	Key   []byte
	Value []byte
}

func (s *BadgerStorage) FindValues(prefix []byte, limit int) ([]Entry, error) {
	var entries []Entry

	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()

		count := 0
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if limit > 0 && count >= limit {
				break
			}

			item := it.Item()
			key := item.KeyCopy(nil)
			val, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			entries = append(entries, Entry{Key: key, Value: val})
			count++
		}
		return nil
	})

	return entries, err
}

func (s *BadgerStorage) Exists(key []byte) bool {
	err := s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		if err != nil {
			return err
		}
		return nil
	})
	return err == nil
}

func (s *BadgerStorage) GetNextID(chatID uint64) (uint64, error) {
	key := make([]byte, 16)
	copy(key[0:8], "seq:msg:")
	binary.BigEndian.PutUint64(key[8:16], chatID)

	var nextID uint64

	err := s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(key)

		if errors.Is(err, badger.ErrKeyNotFound) {
			nextID = 1
		} else if err != nil {
			return err
		} else {
			val, _ := item.ValueCopy(nil)
			currentID := binary.BigEndian.Uint64(val)
			nextID = currentID + 1
		}

		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, nextID)
		return txn.Set(key, buf)
	})

	return nextID, err
}

func (s *BadgerStorage) FindValuesSeek(prefix []byte, startKey []byte, limit int) ([]Entry, error) {
	var entries []Entry

	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()

		count := 0
		for it.Seek(startKey); it.ValidForPrefix(prefix); it.Next() {
			if limit > 0 && count >= limit {
				break
			}

			item := it.Item()
			key := item.KeyCopy(nil)
			val, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			entries = append(entries, Entry{Key: key, Value: val})
			count++
		}
		return nil
	})

	return entries, err
}

func (s *BadgerStorage) Close() {
	s.closeOnce.Do(func() { close(s.done) })
	s.db.Close()
}
