// Package bootstrap embeds the signed official list of entry nodes at build time (T5.6).
package bootstrap

import (
	_ "embed"

	"github.com/echofog/echofog-core/internal/v2/keys"
)

// Official list of the release. The file is empty in the OSS source tree: a source build has no
// addresses. A release build fills it with a signed list (efv2-release bootstrap-sign) before
// `go build`; nothing else changes.
//
//go:embed release/list.bin
var builtinList []byte

// BuiltinRaw returns the signed official list embedded in this build, or nil/empty when the build
// carries none. The bytes are not trusted here: OpenList checks the release signature and term.
func BuiltinRaw() []byte { return append([]byte(nil), builtinList...) }

// ReleaseKeys is the release public key set compiled into the build (T4.5/T12.4e). Empty in the
// OSS source build: then no official list is trusted and no update is accepted.
var ReleaseKeys []keys.AKey
