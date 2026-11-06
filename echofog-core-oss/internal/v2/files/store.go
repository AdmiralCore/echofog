package files

// FileStore keeps a download on disk: the verified chunks in "<name>.part", the resume bitmap in
// "<name>.bits", and on Commit the finished file under a safe, unused name.
type FileStore struct{}

// OpenFileStore opens (and resumes) the download of offer inside dir. free reports the free space
// of dir; nil uses the system's. It returns ErrDisk when the file does not fit.
func OpenFileStore(dir string, offer Offer, free func(dir string) (uint64, error)) (*FileStore, error) {
	return nil, ErrDisk
}

// Have implements Store.
func (s *FileStore) Have(i int) bool { return false }

// Count implements Store.
func (s *FileStore) Count() int { return 0 }

// Put implements Store.
func (s *FileStore) Put(i int, data []byte) error { return ErrDisk }

// Commit implements Store.
func (s *FileStore) Commit() error { return ErrDisk }

// Flush writes the resume bitmap.
func (s *FileStore) Flush() error { return ErrDisk }

// Close releases the files without finishing; the partial download stays for a resume.
func (s *FileStore) Close() error { return nil }

// Path is the final path after Commit.
func (s *FileStore) Path() string { return "" }
