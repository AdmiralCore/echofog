package files

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/echofog/echofog-core/internal/services/filexfer"
)

// spaceMargin stays free on the disk besides the file being received (as in v1).
const spaceMargin = 64 << 20

// FileStore keeps a download on disk: the verified chunks in "<fid>.part" (preallocated to the
// size of the file), the resume bitmap in "<fid>.bits", and on Commit the finished file under a
// safe, unused name. The bitmap is only a claim: it is trusted when the part file has exactly the
// expected size and the bitmap the expected length, and it is written after the part file is
// synced, so it never claims what a crash could have lost.
type FileStore struct {
	mu       sync.Mutex
	dir      string
	offer    Offer
	n        int
	part     *os.File
	partPath string
	bitsPath string
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
		partPath: filepath.Join(dir, id+".part"), bitsPath: filepath.Join(dir, id+".bits")}
	s.bits = make([]byte, (s.n+7)/8)
	trusted := false
	if fi, err := os.Stat(s.partPath); err == nil && fi.Size() == int64(offer.Size) {
		if raw, err := os.ReadFile(s.bitsPath); err == nil && len(raw) == len(s.bits) && tailClear(raw, s.n) {
			copy(s.bits, raw)
			trusted = true
		}
	}
	for i := 0; i < s.n; i++ {
		if trusted && s.bit(i) {
			s.have++
		}
	}
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
	if avail < uint64(missing)+spaceMargin {
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
	if !trusted {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Truncate(int64(offer.Size)); err != nil {
			f.Close()
			return nil, err
		}
	}
	s.part = f
	return s, nil
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
// is left as it is.
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
	s.bits[i>>3] |= 1 << (uint(i) & 7)
	s.have++
	s.dirty = true
	return nil
}

// Flush syncs the part file and writes the resume bitmap.
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
	tmp := s.bitsPath + ".tmp"
	if err := os.WriteFile(tmp, s.bits, 0o600); err != nil {
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
	if s.part != nil {
		if cerr := s.part.Close(); err == nil {
			err = cerr
		}
		s.part = nil
	}
	return err
}

// Commit moves the finished file to its final name. It needs every chunk.
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
	if err := s.part.Close(); err != nil {
		return err
	}
	s.part, s.closed = nil, true
	path, err := reserveName(s.dir, filexfer.SafeName(s.offer.Name))
	if err != nil {
		return err
	}
	if err := os.Rename(s.partPath, path); err != nil {
		_ = os.Remove(path)
		return err
	}
	_ = os.Remove(s.bitsPath)
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
