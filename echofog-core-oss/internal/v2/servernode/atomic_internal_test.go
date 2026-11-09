package servernode

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// T12.4a acceptance M2: the identity file is replaced whole or not at all. A reader (or a crash)
// must never find it empty or half written.
func TestWritePrivateIsAtomic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename over an open file is a Linux server concern; the server runs on Linux")
	}
	path := filepath.Join(t.TempDir(), "server-identity.json")
	a, b := bytes.Repeat([]byte("A"), 1<<16), bytes.Repeat([]byte("B"), 1<<16)
	if err := writePrivate(path, a); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var bad atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				got, err := os.ReadFile(path)
				if err != nil {
					bad.Add(1)
					continue
				}
				if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
					bad.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 300; i++ {
		data := a
		if i%2 == 0 {
			data = b
		}
		if err := writePrivate(path, data); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("a reader saw an empty or partial file %d times", n)
	}
	left, _ := filepath.Glob(path + "*")
	if len(left) != 1 {
		t.Fatalf("leftovers next to the file: %v", left)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}
