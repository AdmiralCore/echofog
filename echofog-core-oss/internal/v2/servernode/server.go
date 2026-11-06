package servernode

import (
	"context"
	"errors"
	"io"
	"log/slog"
)

// Init creates the profile in dataDir: identity (recovery phrase returned once), store and
// server.json. It refuses a directory that already holds a profile.
func Init(dataDir string, cfg Config) (mnemonic string, err error) {
	return "", errors.New("servernode: not implemented")
}

// Server is an open profile with its runtime and operator API.
type Server struct{}

// Open opens the profile of dataDir.
func Open(ctx context.Context, dataDir string, log *slog.Logger) (*Server, error) {
	return nil, errors.New("servernode: not implemented")
}

// Start starts the runtime and the operator API.
func (s *Server) Start(ctx context.Context) error { return errors.New("servernode: not implemented") }

// OperatorAddr is the address the operator API listens on.
func (s *Server) OperatorAddr() string { return "" }

// PeerCard returns the node's current peer-scope card envelope.
func (s *Server) PeerCard(ctx context.Context) ([]byte, error) {
	return nil, errors.New("servernode: not implemented")
}

// Close stops everything and wipes the keys.
func (s *Server) Close(ctx context.Context) error { return nil }

// Main runs an `echofog-node v2` command and returns the exit code.
func Main(args []string, env func(string) string, stdout, stderr io.Writer) int { return 1 }
