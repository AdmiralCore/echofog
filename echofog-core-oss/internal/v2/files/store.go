package files

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/echofog/echofog-core/internal/services/filexfer"
)

// spaceMargin stays free on the disk besides the file being received (as in v1).
const spaceMargin = 64 << 20

const (
	leafLen   = 32
	bitsMagic = "EFB1"
)

// FileStore keeps a download on disk: the verified chunks in "<fid>.part" (preallocated to the
// size of the file), the SHA-256 leaf hash of each stored chunk in "<fid>.leaves", the resume
// bitmap in "<fid>.bits", and on Commit the finished file under a safe, unused name.
//
// Nothing on disk is believed at face value. The bitmap carries the root of the offer it belongs
// to, so an offer with the same fid and another root starts from nothing. On open every chunk the
// bitmap marks is read back and hashed against the leaf hash stored when it was verified; a chunk
// that changed while the download was off is dropped. Commit hashes the whole part file and checks
// the tree against the root of the offer before the file gets its final name.
//
// Put trusts its caller: the chunk has been checked against the root through its audit path
// (Fetch does that). The bitmap is written after the part and leaves files are synced, so it never
// claims what a crash could have lost.
type FileStore struct {
	mu       sync.Mutex
	dir      string
	offer    Offer
	n        int
	part     *os.File
	leaves   *os.File
	partPath string
	bitsPath string
	leafPath string
	bits     []byte
	have     int
	dirty    bool
	final    string
	closed   bool
}

// OpenFileStore opens (and resumes) the download of offer inside dir. free reports the free space
// of dir; nil uses the system's. It returns ErrDisk when what is still missing does not fit.
func OpenFileStore(dir string, offer Offer, free func(dir string) (uint64, error)) (*FileStore, error) {
	if err := offer.validateGeometry(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(offer.FID[:])
	s := &FileStore{dir: dir, offer: offer, n: offer.Chunks(),
		partPath: filepath.Join(dir, id+".part"), bitsPath: filepath.Join(dir, id+".bits"), leafPath: filepath.Join(dir, id+".leaves")}
	s.bits = make([]byte, (s.n+7)/8)
	trusted := s.loadResume()
	missing := int64(0)
	for i := 0; i < s.n; i++ {
		if !(trusted && s.bit(i)) {
			missing += int64(chunkLen(int64(offer.Size), i))
		}
	}
	if free == nil {
		free = filexfer.FreeSpace
	}
	avail, err := free(dir)
	if err != nil {
		return nil, fmt.Errorf("files: free space: %w", err)
	}
	if avail < uint64(missing)+uint64(s.n)*leafLen+spaceMargin {
		return nil, ErrDisk
	}
	if !trusted {
		s.bits = make([]byte, (s.n+7)/8)
		s.have = 0
		_ = os.Remove(s.bitsPath)
	}
	f, err := os.OpenFile(s.partPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(s.leafPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !trusted {
		sizes := []struct {
			f    *os.File
			size int64
		}{{f, int64(offer.Size)}, {lf, int64(s.n) * leafLen}}
		for _, t := range sizes {
			if err := t.f.Truncate(0); err != nil {
				f.Close()
				lf.Close()
				return nil, err
			}
			if err := t.f.Truncate(t.size); err != nil {
				f.Close()
				lf.Close()
				return nil, err
			}
		}
	}
	s.part, s.leaves = f, lf
	return s, nil
}

// loadResume reads the bitmap and checks it against the files and the offer. It fills s.bits and
// s.have with the chunks that are still good and reports whether any of the earlier download is
// kept (false: start from nothing).
func (s *FileStore) loadResume() bool {
	pfi, err := os.Stat(s.partPath)
	if err != nil || pfi.Size() != int64(s.offer.Size) {
		return false
	}
	lfi, err := os.Stat(s.leafPath)
	if err != nil || lfi.Size() != int64(s.n)*leafLen {
		return false
	}
	raw, err := os.ReadFile(s.bitsPath)
	if err != nil || len(raw) != len(bitsMagic)+32+len(s.bits) || string(raw[:len(bitsMagic)]) != bitsMagic ||
		!bytes.Equal(raw[len(bitsMagic):len(bitsMagic)+32], s.offer.Root[:]) {
		return false
	}
	bm := raw[len(bitsMagic)+32:]
	if !tailClear(bm, s.n) {
		return false
	}
	part, err := os.Open(s.partPath)
	if err != nil {
		return false
	}
	defer part.Close()
	leaves, err := os.Open(s.leafPath)
	if err != nil {
		return false
	}
	defer leaves.Close()
	buf := make([]byte, ChunkSize)
	var want [leafLen]byte
	for i := 0; i < s.n; i++ {
		if bm[i>>3]&(1<<(uint(i)&7)) == 0 {
			continue
		}
		l := chunkLen(int64(s.offer.Size), i)
		if _, err := part.ReadAt(buf[:l], int64(i)*ChunkSize); err != nil && !(errors.Is(err, io.EOF) && l == 0) {
			continue
		}
		if _, err := leaves.ReadAt(want[:], int64(i)*leafLen); err != nil {
			continue
		}
		if got := filexfer.LeafHash(buf[:l]); [leafLen]byte(got) == want {
			s.bits[i>>3] |= 1 << (uint(i) & 7)
			s.have++
		}
	}
	return true
}

// tailClear: the unused bits of the last byte are zero.
func tailClear(b []byte, n int) bool {
	if n%8 == 0 || len(b) == 0 {
		return true
	}
	return b[len(b)-1]>>(uint(n%8)) == 0
}

func chunkLen(size int64, i int) int {
	off := int64(i) * ChunkSize
	switch {
	case off >= size:
		return 0
	case size-off < ChunkSize:
		return int(size - off)
	}
	return ChunkSize
}

func (s *FileStore) bit(i int) bool { return s.bits[i>>3]&(1<<(uint(i)&7)) != 0 }

// Have implements Store.
func (s *FileStore) Have(i int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return i >= 0 && i < s.n && s.bit(i)
}

// Count implements Store.
func (s *FileStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.have
}

// Put implements Store. The chunk must be exactly the length chunk i has; a chunk stored already
// is left as it is. The caller has checked the chunk against the root of the offer.
func (s *FileStore) Put(i int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.part == nil {
		return errors.New("files: store closed")
	}
	if i < 0 || i >= s.n || len(data) != chunkLen(int64(s.offer.Size), i) {
		return fmt.Errorf("%w: chunk %d", ErrMalformed, i)
	}
	if s.bit(i) {
		return nil
	}
	if _, err := s.part.WriteAt(data, int64(i)*ChunkSize); err != nil {
		return err
	}
	leaf := filexfer.LeafHash(data)
	if _, err := s.leaves.WriteAt(leaf[:], int64(i)*leafLen); err != nil {
		return err
	}
	s.bits[i>>3] |= 1 << (uint(i) & 7)
	s.have++
	s.dirty = true
	return nil
}

// Flush syncs the part and leaf files and writes the resume bitmap.
func (s *FileStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *FileStore) flushLocked() error {
	if !s.dirty || s.part == nil {
		return nil
	}
	if err := s.part.Sync(); err != nil {
		return err
	}
	if err := s.leaves.Sync(); err != nil {
		return err
	}
	tmp := s.bitsPath + ".tmp"
	hdr := append(append([]byte(bitsMagic), s.offer.Root[:]...), s.bits...)
	if err := os.WriteFile(tmp, hdr, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.bitsPath); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Close releases the files without finishing; the partial download stays for a resume.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.flushLocked()
	for _, f := range []**os.File{&s.part, &s.leaves} {
		if *f != nil {
			if cerr := (*f).Close(); err == nil {
				err = cerr
			}
			*f = nil
		}
	}
	return err
}

// Commit proves the finished file and moves it to its final name. It needs every chunk. The part
// file is read back once: a chunk whose bytes no longer match the leaf hash stored with it is
// dropped (the next fetch asks for it again) and Commit fails with ErrCorrupt; when every chunk
// matches but the tree does not give the root of the offer, nothing in the download can be
// trusted and it starts over, also with ErrCorrupt.
func (s *FileStore) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.part == nil {
		return errors.New("files: store closed")
	}
	if s.have != s.n {
		return fmt.Errorf("files: %d of %d chunks", s.have, s.n)
	}
	if err := s.part.Sync(); err != nil {
		return err
	}
	hashes := make([]filexfer.Hash, s.n)
	buf := make([]byte, ChunkSize)
	var stored [leafLen]byte
	bad := 0
	for i := 0; i < s.n; i++ {
		l := chunkLen(int64(s.offer.Size), i)
		if _, err := s.part.ReadAt(buf[:l], int64(i)*ChunkSize); err != nil && !(errors.Is(err, io.EOF) && l == 0) {
			return err
		}
		hashes[i] = filexfer.LeafHash(buf[:l])
		if _, err := s.leaves.ReadAt(stored[:], int64(i)*leafLen); err != nil {
			return err
		}
		if [leafLen]byte(hashes[i]) != stored {
			s.bits[i>>3] &^= 1 << (uint(i) & 7)
			s.have--
			bad++
		}
	}
	if bad > 0 {
		s.dirty = true
		_ = s.flushLocked()
		return fmt.Errorf("%w: %d chunks changed on disk", ErrCorrupt, bad)
	}
	if [32]byte(filexfer.NewTree(hashes).Root()) != s.offer.Root {
		s.bits = make([]byte, len(s.bits))
		s.have = 0
		s.dirty = true
		_ = s.flushLocked()
		return fmt.Errorf("%w: the file does not match the offered root", ErrCorrupt)
	}
	if err := s.part.Close(); err != nil {
		return err
	}
	_ = s.leaves.Close()
	s.part, s.leaves, s.closed = nil, nil, true
	path, err := reserveName(s.dir, filexfer.SafeName(s.offer.Name))
	if err != nil {
		return err
	}
	if err := os.Rename(s.partPath, path); err != nil {
		_ = os.Remove(path)
		return err
	}
	_ = os.Remove(s.bitsPath)
	_ = os.Remove(s.leafPath)
	markFromInternet(path)
	s.final = path
	return nil
}

// reserveName creates an empty file under the first unused name "a.txt", "a (1).txt", … so that an
// existing file is never replaced; the caller renames over the empty placeholder.
func reserveName(dir, name string) (string, error) {
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	for k := 0; k < 1000; k++ {
		cand := name
		if k > 0 {
			cand = stem + " (" + strconv.Itoa(k) + ")" + ext
		}
		p := filepath.Join(dir, cand)
		if filepath.Dir(p) != filepath.Clean(dir) {
			return "", errors.New("files: unsafe file name")
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return p, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("files: no free file name")
}

// Path is the final path after Commit.
func (s *FileStore) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.final
}
