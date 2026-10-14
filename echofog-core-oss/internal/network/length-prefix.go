package network

import (
	"encoding/binary"
	"errors"
	"io"
)

// MaxFrameSize bounds a single length-prefixed frame. Without it a peer could announce a
// multi-gigabyte length and make us allocate it before a single payload byte arrives.
const MaxFrameSize = 64 << 20

var ErrFrameTooLarge = errors.New("frame exceeds the maximum size")

func WriteLengthPrefix(w io.Writer, data []byte) error {
	buf := make([]byte, 4+len(data))

	binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))

	copy(buf[4:], data)

	_, err := w.Write(buf)
	return err
}

func ReadLengthPrefix(r io.Reader) ([]byte, error) {
	header := make([]byte, 4)

	_, err := io.ReadFull(r, header)
	if err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(header)
	if length > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	payload := make([]byte, length)

	_, err = io.ReadFull(r, payload)
	if err != nil {
		return nil, err
	}

	return payload, nil
}
