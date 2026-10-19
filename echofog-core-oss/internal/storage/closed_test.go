package storage

import (
	"errors"
	"sync"
	"testing"

	"github.com/dgraph-io/badger/v4"
)

// After Close every operation fails with ErrClosed; none panics, even while Close happens in the
// middle of them (badger itself panics when an iterator is opened on a closed database).
func TestStorageAfterCloseFailsCleanly(t *testing.T) {
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := NewBadgerStorage(db)
	if err := s.Set([]byte("p:1"), []byte("v")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = s.FindValues([]byte("p:"), 10)
				_, _ = s.FindValuesSeek([]byte("p:"), []byte("p:"), 10)
				_ = s.Set([]byte("p:2"), []byte("v"))
				_, _ = s.Get([]byte("p:1"))
			}
		}()
	}
	s.Close()
	close(stop)
	wg.Wait()

	if _, err := s.FindValues([]byte("p:"), 10); !errors.Is(err, ErrClosed) {
		t.Fatalf("FindValues after Close: %v", err)
	}
	if err := s.Set([]byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Set after Close: %v", err)
	}
	if s.Exists([]byte("p:1")) {
		t.Fatal("nothing exists in a closed storage")
	}
	s.Close() // a second Close is harmless
}
