package network

import (
	"errors"

	"io"

	"github.com/echofog/echofog-core/internal/network/net_types"
)

// ErrEmptyFrame: a frame must carry at least its type byte.
var ErrEmptyFrame = errors.New("empty frame")

func WriteFrame(w io.Writer, msgType net_types.MessageType, data []byte) error {

	payload := make([]byte, 1+len(data))

	copy(payload[1:], data)

	payload[0] = byte(msgType)

	return WriteLengthPrefix(w, payload)
}

func ReadFrame(r io.Reader) (net_types.MessageType, []byte, error) {
	data, err := ReadLengthPrefix(r)
	if err != nil {
		return net_types.TypeUnknown, nil, err
	}
	if len(data) == 0 {
		return net_types.TypeUnknown, nil, ErrEmptyFrame // a remote could otherwise crash the reader
	}
	msgType := net_types.MessageType(data[0])

	return msgType, data[1:], err
}

func SendReadyFrame(w io.Writer) bool {
	payload := [1]byte{byte(net_types.TypeReady)}
	_, err := w.Write(payload[:])
	return err == nil
}

func AcceptReadyFrame(r io.Reader) bool {
	var data [1]byte
	_, err := io.ReadFull(r, data[:])
	return err == nil && data == [1]byte{byte(net_types.TypeReady)}
}
