package files

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/filexfer"
)

// offerOf is a real offer for data: its root is the root of the data's tree.
func offerOf(t *testing.T, name string, data []byte, f [16]byte) Offer {
	t.Helper()
	tree, err := filexfer.HashFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	o := goodOffer()
	o.FID, o.Name, o.Size, o.Root = f, name, uint64(len(data)), [32]byte(tree.Root())
	return o
}

func chunkOf(data []byte, i int) []byte {
	return data[i*ChunkSize : min((i+1)*ChunkSize, len(data))]
}

func partPath(dir string, o Offer) string {
	return filepath.Join(dir, hex.EncodeToString(o.FID[:])+".part")
}

// A download is bound to the root of the offer, not only to its fid and size: an offer with the
// same fid and size and another root (any chat member can send one) finds nothing of the other's.
func TestResumeIsBoundToTheRoot(t *testing.T) {
	dir := t.TempDir()
	dataA, dataB := content(5*ChunkSize, 1), content(5*ChunkSize, 2)
	a, b := offerOf(t, "a.bin", dataA, fid(7)), offerOf(t, "a.bin", dataB, fid(7))
	if a.Root == b.Root {
		t.Fatal("the test needs two roots")
	}
	s, err := openStore(t, dir, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Put(i, chunkOf(dataA, i)); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Flush()
	_ = s.Close()

	s2, err := openStore(t, dir, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Count() != 0 {
		t.Fatalf("an offer with another root picked up %d chunks of the first download", s2.Count())
	}
	for i := 0; i < 5; i++ {
		if err := s2.Put(i, chunkOf(dataB, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s2.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s2.Path()); !bytes.Equal(got, dataB) {
		t.Fatal("the finished file holds bytes of the other download")
	}
	// the same offer resumes as before
	dir2 := t.TempDir()
	s3, _ := openStore(t, dir2, a, nil)
	_ = s3.Put(0, chunkOf(dataA, 0))
	_ = s3.Flush()
	_ = s3.Close()
	s4, err := openStore(t, dir2, a, nil)
	if err != nil || s4.Count() != 1 || !s4.Have(0) {
		t.Fatalf("the same offer lost its progress: %v", err)
	}
}

// After a restart the chunks written earlier are checked again: a part file changed while the
// download was off (bit rot, another program, an attacker) does not count as downloaded.
func TestResumeRechecksStoredChunks(t *testing.T) {
	dir := t.TempDir()
	data := content(6*ChunkSize-50, 3)
	o := offerOf(t, "r.bin", data, fid(8))
	s, _ := openStore(t, dir, o, nil)
	for i := 0; i < 5; i++ {
		_ = s.Put(i, chunkOf(data, i))
	}
	_ = s.Flush()
	_ = s.Close()

	f, err := os.OpenFile(partPath(dir, o), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xAA, 0xBB}, 2*ChunkSize+100); err != nil { // inside chunk 2
		t.Fatal(err)
	}
	_ = f.Close()

	s2, err := openStore(t, dir, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Have(2) {
		t.Fatal("a chunk whose bytes changed on disk still counts as downloaded")
	}
	for _, i := range []int{0, 1, 3, 4} {
		if !s2.Have(i) {
			t.Fatalf("the intact chunk %d was dropped", i)
		}
	}
	if s2.Count() != 4 {
		t.Fatalf("Count = %d, want 4", s2.Count())
	}
	_ = s2.Put(2, chunkOf(data, 2))
	_ = s2.Put(5, chunkOf(data, 5))
	if err := s2.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s2.Path()); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

// Commit proves the whole file against the root of the offer. A chunk the caller stored without
// checking, or one changed behind the store's back, makes Commit fail, drops that chunk and
// restarts or repairs the download (a changed chunk is told apart, a wrong one stored as it came is not).
func TestCommitProvesTheRoot(t *testing.T) {
	data := content(4*ChunkSize, 4)
	o := offerOf(t, "c.bin", data, fid(9))

	s, _ := openStore(t, t.TempDir(), o, nil)
	for i := 0; i < 4; i++ {
		chunk := chunkOf(data, i)
		if i == 2 {
			chunk = bytes.Repeat([]byte{1}, ChunkSize) // wrong bytes of the right length
		}
		_ = s.Put(i, chunk)
	}
	err := s.Commit()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit of a file that is not the offered one: %v", err)
	}
	if s.Path() != "" {
		t.Fatal("a file that failed the proof was moved to its final name")
	}
	// the leaf hashes agree with the bytes, so the bad chunk cannot be told from the good ones:
	// the download starts over (fail closed)
	if s.Count() != 0 {
		t.Fatalf("after the failed proof Count=%d, want a fresh start", s.Count())
	}
	for i := 0; i < 4; i++ {
		if err := s.Put(i, chunkOf(data, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("Commit after repairing the chunk: %v", err)
	}
	if got, _ := os.ReadFile(s.Path()); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}

	// a change behind the store's back while it is open
	dir := t.TempDir()
	s2, _ := openStore(t, dir, o, nil)
	for i := 0; i < 4; i++ {
		_ = s2.Put(i, chunkOf(data, i))
	}
	f, _ := os.OpenFile(partPath(dir, o), os.O_RDWR, 0)
	_, _ = f.WriteAt([]byte{0x55}, 3*ChunkSize+1)
	_ = f.Close()
	if err := s2.Commit(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit after the part file was changed: %v", err)
	}
	if s2.Have(3) {
		t.Fatal("the changed chunk still counts")
	}
}

// A store that is already complete is proved too, not committed on its word (the file may have
// been damaged while the app was off), and the fetch repairs it by asking for the bad chunk again.
func TestFetchOfACompleteButDamagedStoreRepairsIt(t *testing.T) {
	l := newLab(t, ServerConfig{})
	data := content(8*ChunkSize, 5)
	o := l.share(data, nodeB)
	dir := t.TempDir()
	s, _ := openStore(t, dir, o, nil)
	for i := 0; i < 8; i++ {
		_ = s.Put(i, chunkOf(data, i))
	}
	_ = s.Flush()
	_ = s.Close()
	f, _ := os.OpenFile(partPath(dir, o), os.O_RDWR, 0)
	_, _ = f.WriteAt([]byte{0x77}, 4*ChunkSize+9)
	_ = f.Close()

	s2, err := openStore(t, dir, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stats FetchStats
	opt := fast()
	opt.Stats = &stats
	if err := l.fetch(l.dial(nodeB), o, s2, opt); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got, _ := os.ReadFile(s2.Path()); !bytes.Equal(got, data) {
		t.Fatal("the damaged chunk was committed")
	}
	if stats.Requested.Load() != 1 {
		t.Fatalf("asked for %d chunks, only the damaged one was missing", stats.Requested.Load())
	}
}

// The sender is told the file is done only after it has been proved and committed.
type failingCommit struct {
	*memStore
	err error
}

func (s failingCommit) Commit() error { return s.err }

func TestDoneIsSentOnlyAfterACommit(t *testing.T) {
	l := newLab(t, ServerConfig{})
	o := l.share(content(3*ChunkSize, 6), nodeB)
	st := failingCommit{newMemStore(o), errors.New("disk full")}
	if err := l.fetch(l.dial(nodeB), o, st, fast()); err == nil {
		t.Fatal("a failed Commit was reported as success")
	}
	time.Sleep(300 * time.Millisecond)
	if n := l.srv.Stats().Done.Load(); n != 0 {
		t.Fatalf("the sender counted %d deliveries of a file that was never committed", n)
	}
}

// A bitmap written by the first version (no root in it) is not trusted.
func TestOldBitmapFormatIsNotTrusted(t *testing.T) {
	dir := t.TempDir()
	data := content(3*ChunkSize, 7)
	o := offerOf(t, "o.bin", data, fid(10))
	s, _ := openStore(t, dir, o, nil)
	for i := 0; i < 3; i++ {
		_ = s.Put(i, chunkOf(data, i))
	}
	_ = s.Flush()
	_ = s.Close()
	_ = os.WriteFile(filepath.Join(dir, hex.EncodeToString(o.FID[:])+".bits"), []byte{0x07}, 0o600)
	s2, err := openStore(t, dir, o, nil)
	if err != nil || s2.Count() != 0 {
		t.Fatalf("headerless bitmap: count %d, err %v", s2.Count(), err)
	}
}
