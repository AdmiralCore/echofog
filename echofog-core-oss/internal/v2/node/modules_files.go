package node

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// registerFiles registers files/2 (T8.1) when Config.Files is set (stub of the red commit).
func (r *Runtime) registerFiles() {}

// Files returns the files/2 server of this node, or nil when Config.Files is not set.
func (r *Runtime) Files() *files.Server { return nil }

// OpenFiles opens a files/2 stream to a device of the account behind node.
func (r *Runtime) OpenFiles(ctx context.Context, node [32]byte) (*mux.Stream, error) {
	return nil, errors.New("node: files are not built")
}
