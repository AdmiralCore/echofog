package handshake

import (
	"errors"
	"reflect"
	"unsafe"

	utls "github.com/refraction-networking/utls"
)

// uconnOf returns the *utls.UConn inside a UQUICConn. uTLS keeps it in an unexported field and
// offers no way to install private key-share keys on a QUIC connection, which the ClientHello
// hook needs (patch 4). The lookup fails closed if a uTLS release changes the layout; a test
// pins the pinned version.
func uconnOf(q *utls.UQUICConn) (*utls.UConn, error) {
	f := reflect.ValueOf(q).Elem().FieldByName("conn")
	if !f.IsValid() || f.Type() != reflect.TypeOf((*utls.UConn)(nil)) {
		return nil, errors.New("quic: uTLS UQUICConn has no conn field of type *UConn")
	}
	uc := *(**utls.UConn)(unsafe.Pointer(f.UnsafeAddr()))
	if uc == nil {
		return nil, errors.New("quic: uTLS UQUICConn.conn is nil")
	}
	return uc, nil
}
