//go:build windows

package osprotect

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapi is the Windows Data Protection API for the current user: a blob opens for this Windows
// account on this computer (and where the account's profile roams), not for another account, and
// not on another computer the profile folder was copied to.
type dpapi struct{}

func system() Protector { return dpapi{} }

func (dpapi) Protect(secret, entropy []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptProtectData(dataBlob(secret), nil, dataBlob(entropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}

func (dpapi) Unprotect(blob, entropy []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(dataBlob(blob), nil, dataBlob(entropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}

// dataBlob points the system at b; nil for none.
func dataBlob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return nil
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

// takeBlob copies what the system returned into Go memory, then wipes and frees the original.
func takeBlob(b *windows.DataBlob) []byte {
	if b.Data == nil {
		return nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(b.Data)))
	src := unsafe.Slice(b.Data, b.Size)
	out := append([]byte(nil), src...)
	clear(src)
	return out
}
