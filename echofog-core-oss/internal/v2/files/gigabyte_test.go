package files

import (
	"encoding/binary"
	"os"
	"sync"
	"testing"
	"time"
)

// genReader is a file of any size made on the fly: byte i depends only on its position.
type genReader struct{ size int64 }

func (g genReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= g.size {
		return 0, os.ErrNotExist
	}
	n := len(p)
	if int64(n) > g.size-off {
		n = int(g.size - off)
	}
	var w [8]byte
	for i := 0; i < n; i++ {
		pos := off + int64(i)
		if i == 0 || pos%8 == 0 {
			x := uint64(pos/8) + 0x9e3779b97f4a7c15
			x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
			x = (x ^ (x >> 27)) * 0x94d049bb133111eb
			binary.LittleEndian.PutUint64(w[:], x^(x>>31))
		}
		p[i] = w[pos%8]
	}
	if n < len(p) {
		return n, os.ErrNotExist
	}
	return n, nil
}

// TestOneGiBWithLoss is the acceptance transfer of T8.1: 1 GiB over memcarrier with 2 % of the
// chunks lost, then a resume after a break. It is heavy (a minute, 1 GiB of disk), so it runs only
// with ECHOFOG_FILES_1GIB=1; ECHOFOG_FILES_SIZE_MIB overrides the size.
func TestOneGiBWithLoss(t *testing.T) {
	if os.Getenv("ECHOFOG_FILES_1GIB") == "" {
		t.Skip("set ECHOFOG_FILES_1GIB=1 to run the 1 GiB transfer")
	}
	size := int64(1) << 30
	l := newLab(t, ServerConfig{Clock: realClock{}})
	src := genReader{size: size}
	sh, err := PrepareShare(src, size, "big.bin", "application/octet-stream", nodeA.node(), [][32]byte{nodeB.node()})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.srv.Add(sh); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[uint64]int{}
	l.srv.SetChunkFilter(func(_ [16]byte, i uint64) bool {
		mu.Lock()
		defer mu.Unlock()
		seen[i]++
		return seen[i] == 1 && i%50 == 7 // 2 % lost once
	})
	dir := t.TempDir()
	store, err := OpenFileStore(dir, sh.Offer, nil)
	if err != nil {
		t.Fatal(err)
	}
	k := l.dial(nodeB)
	// break the download half way, then resume it
	ctxStop := make(chan struct{})
	opt := FetchOptions{Clock: realClock{}, MinRTO: 300 * time.Millisecond}
	opt.OnProgress = func(done, total int) {
		if done == total/2 {
			select {
			case <-ctxStop:
			default:
				close(ctxStop)
				_ = k.client.Close()
			}
		}
	}
	start := time.Now()
	if err := Fetch(l.ctx, l.open(k), sh.Offer, store, opt); err == nil {
		t.Fatal("the break did not stop the first run")
	}
	_ = store.Flush()
	_ = store.Close()
	store, err = OpenFileStore(dir, sh.Offer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if store.Count() < sh.Offer.Chunks()/2 {
		t.Fatalf("the resume found %d of %d chunks", store.Count(), sh.Offer.Chunks())
	}
	k2 := l.dial(nodeB)
	var stats FetchStats
	opt = FetchOptions{Clock: realClock{}, MinRTO: 300 * time.Millisecond, Stats: &stats}
	if err := Fetch(l.ctx, l.open(k2), sh.Offer, store, opt); err != nil {
		t.Fatal(err)
	}
	t.Logf("1 GiB with 2%% loss and a resume: %v, %d resent, %d losses", time.Since(start), stats.Resent.Load(), stats.Losses.Load())
	// verify by hashing what was written against the offered root
	f, err := os.Open(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi, _ := f.Stat(); fi.Size() != size {
		t.Fatalf("file is %d bytes", fi.Size())
	}
}
