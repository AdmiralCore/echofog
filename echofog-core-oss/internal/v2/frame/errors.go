package frame

import (
	"errors"
	"fmt"
)

var (
	// ErrShort: the input ends inside a varint, a frame header or a payload.
	ErrShort = errors.New("frame: short input")
	// ErrNonMinimal: a varint is not in its shortest form.
	ErrNonMinimal = errors.New("frame: non-minimal varint")
	// ErrMalformed is wrapped by every payload decoding error of a known type. Only these
	// errors may raise an abuse score (versioning.md §2 rule 2).
	ErrMalformed = errors.New("frame: malformed payload")
)

// TooLargeError: the declared length exceeds the type's maximum or 1 MiB (wire.md §6.1). The
// payload has not been read; the stream (or connection) cannot be resynchronised.
type TooLargeError struct {
	Type   uint64
	Length uint64
	Max    int
	Known  bool
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("frame: type %#x length %d exceeds maximum %d", e.Type, e.Length, e.Max)
}

// UnknownFrameError: a frame of an unknown type (or with a reserved flag bit) without
// IGNORABLE. Its payload has been consumed, so the reader is still in sync. It is never abuse.
type UnknownFrameError struct {
	Type  uint64
	Flags byte
}

func (e *UnknownFrameError) Error() string {
	return fmt.Sprintf("frame: unknown type %#x (flags %#x)", e.Type, e.Flags)
}

// IsTooLarge reports whether err is a TooLargeError.
func IsTooLarge(err error) bool {
	var t *TooLargeError
	return errors.As(err, &t)
}

// IsUnknown reports whether err is an UnknownFrameError.
func IsUnknown(err error) bool {
	var u *UnknownFrameError
	return errors.As(err, &u)
}

// IsMalformed reports whether err is a payload decoding error of a known type.
func IsMalformed(err error) bool { return errors.Is(err, ErrMalformed) }

func malformed(what string) error { return fmt.Errorf("%w: %s", ErrMalformed, what) }
