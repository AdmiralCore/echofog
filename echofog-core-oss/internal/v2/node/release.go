package node

import (
	"time"

	"github.com/echofog/echofog-core/internal/v2/release"
)

// Release returns the release manifest state of the node, or nil when the build has no release
// keys (Config.ReleaseKeys).
func (r *Runtime) Release() *release.Manager { return nil }

// admitVersion is the handshake version admission built from the manifest.
func (r *Runtime) admitVersion() func(peerProto uint32) bool { return nil }

// releaseCheck is the DHT server's release check: the explicit Config.ReleaseCheck, else the
// manager's.
func (r *Runtime) releaseCheck() func(raw []byte, now time.Time) error { return nil }
