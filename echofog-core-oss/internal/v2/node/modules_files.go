package node

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// registerFiles registers files/2 (T8.1) when Config.Files is set: the sender side of the protocol
// serves the shares the app adds to the server, and tells Config.FilesOnDone when a recipient has the
// whole file. Without the flag the service is not offered and peers get UNSUPPORTED.
func (r *Runtime) registerFiles() {
	if !r.cfg.Files {
		return
	}
	spec, ok := service.Builtin(service.Files)
	if !ok {
		return
	}
	srv := files.NewServer(files.ServerConfig{Limits: r.cfg.FilesLimits, OnDone: r.cfg.FilesOnDone})
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		srv.Handle(ctx, files.Peer{Node: [32]byte(p.Node), Device: [32]byte(p.Device)}, st)
	})
	r.mu.Lock()
	r.files = srv
	r.mu.Unlock()
}

// Files returns the files/2 server of this node, or nil when Config.Files is not set.
func (r *Runtime) Files() *files.Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files
}

// ErrNoFilesDevice means no device of the account could be reached for files/2.
var ErrNoFilesDevice = errors.New("node: no device of the account offers files")

// OpenFiles opens a files/2 stream to a device of the account behind node: the devices its published
// cards name are tried in turn. It is what the app fetches a file over (app.FetchOver).
func (r *Runtime) OpenFiles(ctx context.Context, node [32]byte) (*mux.Stream, error) {
	res, err := chatDir{r}.Resolve(ctx, node)
	if err != nil {
		return nil, err
	}
	var last error = ErrNoFilesDevice
	for _, c := range res.Cards {
		if c == nil {
			continue
		}
		_, st, err := r.Open(ctx, DeviceID(c.DeviceID), service.Files, DialOpt{})
		if err == nil {
			return st, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}
