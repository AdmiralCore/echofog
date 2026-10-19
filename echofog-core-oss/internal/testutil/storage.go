package testutil

import (
	"sync"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/storage"
)

type TestingBadgerStorage struct {
	St   *storage.BadgerStorage
	once sync.Once
}

func NewTestingBadgerStorage(t *testing.T) *TestingBadgerStorage {
	path := t.TempDir()
	opts := badger.DefaultOptions(path)
	opts.Logger = nil

	db, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("failed to open badger db: %v", err)
	}

	st := storage.NewBadgerStorage(db)

	return &TestingBadgerStorage{
		St: st,
	}
}

func (t *TestingBadgerStorage) CloseAndClear() {
	t.once.Do(func() {
		// through the wrapper, so a goroutine still running reads ErrClosed instead of panicking
		t.St.Close()
	})
}
