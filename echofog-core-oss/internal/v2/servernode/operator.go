package servernode

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
)

// StatusInfo is what GET /v1/status returns.
type StatusInfo struct {
	NodeID     string   `json:"node_id"`
	DeviceID   string   `json:"device_id"`
	Roles      []string `json:"roles"`
	Sessions   int      `json:"sessions"`
	PublicCard bool     `json:"public_card"`
	Warnings   []string `json:"warnings,omitempty"`
	Listen     []string `json:"listen,omitempty"`
}

// ErrNoInvites: invitations are off (no invite listener configured).
var ErrNoInvites = errors.New("servernode: invitations are not configured")

// Backend is the node as the operator API sees it.
type Backend interface {
	Status() StatusInfo
	MintInvite(ctx context.Context, opt access.MintOptions) (*access.Minted, error)
	ListAccess(ctx context.Context) ([]access.Info, error)
	ExtendAccess(ctx context.Context, id access.PassID, d time.Duration) (access.Info, error)
	RotateKeys(ctx context.Context) (*access.Rotation, error)
}

// OperatorOptions configures the operator handler.
type OperatorOptions struct {
	Backend Backend
	// Token is the bearer token; empty refuses everything.
	Token string
	Log   *slog.Logger
	Now   func() time.Time
}

// NewOperator returns the operator API handler. See operator_test.go for the contract.
func NewOperator(OperatorOptions) http.Handler { return http.NotFoundHandler() }

// NewTokenFile returns the token stored at path, creating it (random, mode 0600) when missing.
func NewTokenFile(path string) (string, error) { return "", errors.New("servernode: not implemented") }
