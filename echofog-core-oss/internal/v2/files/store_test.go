package files

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/services/filexfer"
)

// openStore opens a store and closes it when the test ends (Windows cannot remove an open file).
func openStore(t *testing.T, dir string, o Offer, free func(string) (uint64, error)) (*FileStore, error) {
	t.Helper()
	s, err := OpenFileStore(dir, o, free)
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

func storeOffer(name string, chunks int) (Offer, []byte) {
	data := content(chunks*ChunkSize-100, int64(chunks))
	tree, err := filexfer.HashFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		panic(err)
	}
	o := goodOffer()
	o.Name, o.Size, o.Root = name, uint64(len(data)), [32]byte(tree.Root())
	return o, data
}

func putAll(t *testing.T, s *FileStore, data []byte, only func(i int) bool) {
	t.Helper()
	for i := 0; i*ChunkSize < len(data); i++ {
		if only != nil && !only(i) {
			continue
		}
		end := min((i+1)*ChunkSize, len(data))
		if err := s.Put(i, data[i*ChunkSize:end]); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
}

func TestFileStoreWritesAndCommits(t *testing.T) {
	dir := t.TempDir()
	o, data := storeOffer("отчёт.txt", 5)
	s, err := openStore(t, dir, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 || s.Have(0) {
		t.Fatal("a new store is not empty")
	}
	putAll(t, s, data, nil)
	if s.Count() != 5 {
		t.Fatalf("Count = %d", s.Count())
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(s.Path())
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("final file: %v, equal=%v", err, bytes.Equal(got, data))
	}
	if filepath.Dir(s.Path()) != dir || filepath.Base(s.Path()) != "отчёт.txt" {
		t.Fatalf("path %s", s.Path())
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	bits, _ := filepath.Glob(filepath.Join(dir, "*.bits"))
	if len(left)+len(bits) != 0 {
		t.Fatalf("temporary files left: %v %v", left, bits)
	}
}

func TestFileStoreResumesFromTheBitmap(t *testing.T) {
	dir := t.TempDir()
	o, data := storeOffer("a.bin", 8)
	s, err := openStore(t, dir, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, s, data, func(i int) bool { return i%2 == 0 })
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2, err := openStore(t, dir, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if s2.Have(i) != (i%2 == 0) {
			t.Fatalf("chunk %d: Have = %v after reopening", i, s2.Have(i))
		}
	}
	if s2.Count() != 4 {
		t.Fatalf("Count = %d", s2.Count())
	}
	putAll(t, s2, data, func(i int) bool { return i%2 == 1 })
	if err := s2.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s2.Path()); !bytes.Equal(got, data) {
		t.Fatal("content differs after the resume")
	}
}

// A bitmap is only a claim: if the part file is shorter than the bitmap says, nothing is trusted.
func TestFileStoreDoesNotTrustAStaleBitmap(t *testing.T) {
	dir := t.TempDir()
	o, data := storeOffer("b.bin", 6)
	s, _ := openStore(t, dir, o, nil)
	putAll(t, s, data, nil)
	_ = s.Flush()
	_ = s.Close()
	parts, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	if len(parts) != 1 {
		t.Fatalf("part files: %v", parts)
	}
	if err := os.Truncate(parts[0], 2*ChunkSize); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(t, dir, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Count() != 0 {
		t.Fatalf("Count = %d: chunks past the end of the part file counted as present", s2.Count())
	}
	// a bitmap of the wrong length is dropped too
	_ = s2.Close()
	_ = os.WriteFile(filepath.Join(dir, hex.EncodeToString(o.FID[:])+".bits"), []byte{0xff, 0xff, 0xff, 0xff, 0xff}, 0o600)
	s3, err := openStore(t, dir, o, nil)
	if err != nil || s3.Count() != 0 {
		t.Fatalf("garbage bitmap: count %d, err %v", s3.Count(), err)
	}
}

func TestFileStoreCommitNeedsEveryChunk(t *testing.T) {
	o, data := storeOffer("c.bin", 4)
	s, _ := openStore(t, t.TempDir(), o, nil)
	putAll(t, s, data, func(i int) bool { return i != 2 })
	if err := s.Commit(); err == nil {
		t.Fatal("committed an incomplete file")
	}
}

func TestFileStoreNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "same.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, data := storeOffer("same.txt", 2)
	s, _ := openStore(t, dir, o, nil)
	putAll(t, s, data, nil)
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(s.Path()) == "same.txt" {
		t.Fatal("an existing file was replaced")
	}
	if old, _ := os.ReadFile(filepath.Join(dir, "same.txt")); string(old) != "old" {
		t.Fatal("the existing file changed")
	}
	// a name that would leave the directory is made safe; Offer.Validate refuses it earlier, the
	// store must not rely on that
	o2, data2 := storeOffer("x.bin", 2)
	o2.Name = `..\..\evil.exe`
	s2, err := openStore(t, dir, o2, nil)
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, s2, data2, nil)
	if err := s2.Commit(); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, s2.Path())
	if err != nil || strings.Contains(rel, "..") || strings.ContainsAny(rel, `/\`) {
		t.Fatalf("final path %s escapes %s", s2.Path(), dir)
	}
}

func TestFileStoreChecksTheDisk(t *testing.T) {
	o, _ := storeOffer("d.bin", 4)
	small := func(string) (uint64, error) { return 1 << 20, nil }
	if _, err := openStore(t, t.TempDir(), o, small); !errors.Is(err, ErrDisk) {
		t.Fatalf("1 MiB free for 256 KiB + margin: err = %v", err)
	}
	// resuming needs room only for what is missing
	dir := t.TempDir()
	s, _ := openStore(t, dir, o, nil)
	_, data := storeOffer("d.bin", 4)
	putAll(t, s, data, func(i int) bool { return i < 3 })
	_ = s.Flush()
	_ = s.Close()
	fits := func(string) (uint64, error) { return 64<<20 + 2*ChunkSize, nil }
	if _, err := openStore(t, dir, o, fits); err != nil {
		t.Fatalf("one chunk missing, room for it: %v", err)
	}
	err := func() error {
		_, e := openStore(t, t.TempDir(), o, func(string) (uint64, error) { return 0, errors.New("statfs") })
		return e
	}()
	if err == nil {
		t.Fatal("a failing free-space check must not pass silently")
	}
}

func TestFileStorePutValidatesTheIndexAndLength(t *testing.T) {
	o, data := storeOffer("e.bin", 3)
	s, _ := openStore(t, t.TempDir(), o, nil)
	if err := s.Put(-1, data[:10]); err == nil {
		t.Error("negative index")
	}
	if err := s.Put(3, data[:10]); err == nil {
		t.Error("index past the end")
	}
	if err := s.Put(0, data[:10]); err == nil {
		t.Error("a short chunk that is not the last")
	}
	if err := s.Put(2, make([]byte, ChunkSize)); err == nil {
		t.Error("a last chunk of the wrong length")
	}
	if s.Count() != 0 {
		t.Fatalf("Count = %d after refused puts", s.Count())
	}
}

// FuzzFileStoreOpen: whatever a crash or a hand edit left in the part and bitmap files, opening
// never panics and never claims more chunks than the file has.
func FuzzFileStoreOpen(f *testing.F) {
	f.Add([]byte{0xff}, 3*ChunkSize)
	f.Add([]byte{}, 0)
	f.Add(bytes.Repeat([]byte{0xff}, 40), 10)
	f.Fuzz(func(t *testing.T, bits []byte, partLen int) {
		if partLen < 0 || partLen > 8*ChunkSize {
			return
		}
		dir := t.TempDir()
		o, _ := storeOffer("f.bin", 5)
		id := hex.EncodeToString(o.FID[:])
		_ = os.WriteFile(filepath.Join(dir, id+".bits"), bits, 0o600)
		_ = os.WriteFile(filepath.Join(dir, id+".part"), make([]byte, partLen), 0o600)
		s, err := OpenFileStore(dir, o, func(string) (uint64, error) { return 1 << 40, nil })
		if err != nil {
			return
		}
		defer s.Close()
		if s.Count() < 0 || s.Count() > o.Chunks() {
			t.Fatalf("Count = %d of %d", s.Count(), o.Chunks())
		}
		if int64(partLen) != int64(o.Size) && s.Count() != 0 {
			t.Fatalf("a part file of %d bytes (expected %d) was trusted", partLen, o.Size)
		}
	})
}
