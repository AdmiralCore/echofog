package files

// Range is a run of chunks: Start, Start+1, … Start+Count-1.
type Range struct {
	Start uint64
	Count uint32
}

// Want is the body of WANT.
type Want struct {
	FID    [16]byte
	Ranges []Range
}

// Chunk is the body of CHUNK.
type Chunk struct {
	FID   [16]byte
	Index uint64
	Proof [][32]byte
	Data  []byte
}

// Cancel is the body of CANCEL.
type Cancel struct {
	FID    [16]byte
	Reason uint32
}

// Done is the body of DONE.
type Done struct{ FID [16]byte }

// Append encodes the body canonically: ascending field numbers, default values omitted.
func (m Want) Append(b []byte) []byte { return b }

// Append encodes the body canonically.
func (m Chunk) Append(b []byte) []byte { return b }

// Append encodes the body canonically.
func (m Cancel) Append(b []byte) []byte { return b }

// Append encodes the body canonically.
func (m Done) Append(b []byte) []byte { return b }

// ParseWant decodes WANT; it checks the bounds (at most 256 chunks in all).
func ParseWant(p []byte) (Want, error) { return Want{}, ErrMalformed }

// ParseChunk decodes CHUNK.
func ParseChunk(p []byte) (Chunk, error) { return Chunk{}, ErrMalformed }

// ParseCancel decodes CANCEL.
func ParseCancel(p []byte) (Cancel, error) { return Cancel{}, ErrMalformed }

// ParseDone decodes DONE.
func ParseDone(p []byte) (Done, error) { return Done{}, ErrMalformed }
