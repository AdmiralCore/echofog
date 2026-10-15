package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"github.com/echofog/echofog-core/internal/network/net_types"
)

// MaxFrameSize bounds a single length-prefixed frame. Without it a peer could announce a
// multi-gigabyte length and make us allocate it before a single payload byte arrives.
const MaxFrameSize = 64 << 20

// HandshakeMaxFrameSize bounds a single frame on the handshake stream. Handshake messages are a
// few hundred bytes, so a few KiB is generous; it stops an unauthenticated peer from announcing a
// large frame before it has proven anything.
const HandshakeMaxFrameSize = 4 << 10

// readChunk is how much of a frame body we reserve up front. The buffer then grows only as bytes
// actually arrive, so a peer that announces a huge length but sends nothing costs us a few KiB,
// not the announced size.
const readChunk = 64 << 10

var ErrFrameTooLarge = errors.New("frame exceeds the maximum size")

func WriteLengthPrefix(w io.Writer, data []byte) error {
	buf := make([]byte, 4+len(data))

	binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))

	copy(buf[4:], data)

	_, err := w.Write(buf)
	return err
}

func ReadLengthPrefix(r io.Reader) ([]byte, error) {
	return ReadLengthPrefixLimit(r, MaxFrameSize)
}

// ReadLengthPrefixLimit reads a length-prefixed frame, rejecting any whose announced length
// exceeds max. The body is read incrementally: memory follows the bytes that actually arrive, not
// the announced length, so a peer that announces a large frame and then stalls or closes cannot
// make us commit that much memory up front.
func ReadLengthPrefixLimit(r io.Reader, max uint32) ([]byte, error) {
	header := make([]byte, 4)

	_, err := io.ReadFull(r, header)
	if err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(header)
	if length > max {
		return nil, ErrFrameTooLarge
	}
	if length == 0 {
		return []byte{}, nil
	}

	// Reserve only a small chunk (capped at the announced length) and let bytes.Buffer grow as
	// data arrives. io.CopyN stops at exactly length bytes; if the peer sends fewer and then EOFs,
	// CopyN reports it and we surface io.ErrUnexpectedEOF without ever allocating the full length.
	initial := uint32(readChunk)
	if length < initial {
		initial = length
	}

	var buf bytes.Buffer
	buf.Grow(int(initial))

	n, err := io.CopyN(&buf, r, int64(length))
	if err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if n != int64(length) {
		return nil, io.ErrUnexpectedEOF
	}

	return buf.Bytes(), nil
}

// ReadFrameLimit reads a typed frame whose announced length may not exceed max. It is ReadFrame
// with a caller-supplied ceiling, used on the handshake stream where frames are small and the peer
// is not yet authenticated.
func ReadFrameLimit(r io.Reader, max uint32) (net_types.MessageType, []byte, error) {
	data, err := ReadLengthPrefixLimit(r, max)
	if err != nil {
		return net_types.TypeUnknown, nil, err
	}
	if len(data) == 0 {
		return net_types.TypeUnknown, nil, ErrEmptyFrame // a remote could otherwise crash the reader
	}
	return net_types.MessageType(data[0]), data[1:], nil
}
