// Package bootstrap embeds the signed official list of entry nodes at build time (T5.6).
package bootstrap

import (
	_ "embed"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/releasekeys"
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
// OSS source build: then no official list is trusted and no update is accepted. init fills it from
// releaseKeysSpec.
var ReleaseKeys []keys.AKey

// releaseKeysSpec is the build-time release key list: Ed25519 public keys separated by commas in
// hex or base64. A release build passes it with
//
//	-ldflags "-X github.com/echofog/echofog-core/internal/v2/bootstrap.releaseKeysSpec=..."
//
// init parses it into ReleaseKeys; an empty spec is an OSS build with no release keys.
var releaseKeysSpec string

// buildReleaseKeys is the key set parsed by init, kept apart from ReleaseKeys so that tests can
// check the build wiring without depending on test mutations of ReleaseKeys.
var buildReleaseKeys []keys.AKey

func init() {
	buildReleaseKeys = releasekeys.MustParseSpec("bootstrap", releaseKeysSpec)
	ReleaseKeys = buildReleaseKeys
}
