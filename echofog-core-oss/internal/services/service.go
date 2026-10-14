package services

import (
	"context"

	"github.com/echofog/echofog-core/internal/dispatcher"
)

// Lifecycle defines the basic Start / Stop lifecycle contract for all background services.
type Lifecycle interface {
	Name() string
	Start(ctx context.Context) error
	Stop() error
}

// Service is the interface for services that require both Lifecycle management
// and message routing via Dispatcher.
type Service interface {
	Lifecycle
	dispatcher.Handler
	GetSubscribedTypes() []interface{}
}
