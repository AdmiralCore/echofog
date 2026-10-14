package services

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/echofog/echofog-core/internal/dispatcher"
)

// ServiceManager coordinates the lifecycle of all registered services.
type ServiceManager struct {
	mu        sync.RWMutex
	services  []Lifecycle
	names     map[string]Lifecycle
	started   []Lifecycle
	logger    *slog.Logger
	isRunning bool
}

// NewServiceManager creates a new instance of ServiceManager.
func NewServiceManager(logger *slog.Logger) *ServiceManager {
	if logger == nil {
		logger = slog.Default().With("component", "service_manager")
	}
	return &ServiceManager{
		services: make([]Lifecycle, 0),
		names:    make(map[string]Lifecycle),
		started:  make([]Lifecycle, 0),
		logger:   logger,
	}
}

// Register registers a lifecycle-managed service.
func (sm *ServiceManager) Register(svc Lifecycle) error {
	if svc == nil {
		return fmt.Errorf("cannot register nil service")
	}
	name := svc.Name()
	if name == "" {
		return fmt.Errorf("service name cannot be empty")
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.names[name]; exists {
		return fmt.Errorf("service %s already registered", name)
	}

	sm.services = append(sm.services, svc)
	sm.names[name] = svc
	sm.logger.Debug("Service registered", "name", name)
	return nil
}

// RegisterRouted registers a Service that also handles dispatcher messages.
func (sm *ServiceManager) RegisterRouted(svc Service, disp *dispatcher.Dispatcher) error {
	if err := sm.Register(svc); err != nil {
		return err
	}
	if disp != nil {
		for _, msgType := range svc.GetSubscribedTypes() {
			disp.Registry(msgType, svc)
		}
	}
	return nil
}

// StartAll starts all registered services in order.
// If any service fails to start, previously started services are rolled back in reverse order.
func (sm *ServiceManager) StartAll(ctx context.Context) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.isRunning {
		return nil
	}

	for _, svc := range sm.services {
		sm.logger.Info("Starting service...", "name", svc.Name())
		if err := svc.Start(ctx); err != nil {
			sm.logger.Error("Failed to start service", "name", svc.Name(), "err", err)
			for i := len(sm.started) - 1; i >= 0; i-- {
				s := sm.started[i]
				sm.logger.Info("Rolling back service...", "name", s.Name())
				_ = s.Stop()
			}
			sm.started = nil
			return fmt.Errorf("service %s failed to start: %w", svc.Name(), err)
		}
		sm.started = append(sm.started, svc)
	}

	sm.isRunning = true
	sm.logger.Info("All services started successfully", "count", len(sm.started))
	return nil
}

// StopAll stops all started services in reverse order of startup (graceful shutdown).
func (sm *ServiceManager) StopAll() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if !sm.isRunning && len(sm.started) == 0 {
		return nil
	}

	var firstErr error
	for i := len(sm.started) - 1; i >= 0; i-- {
		svc := sm.started[i]
		sm.logger.Info("Stopping service...", "name", svc.Name())
		if err := svc.Stop(); err != nil {
			sm.logger.Error("Error stopping service", "name", svc.Name(), "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	sm.started = nil
	sm.isRunning = false
	sm.logger.Info("All services stopped")
	return firstErr
}

// GetService retrieves a service by name.
func (sm *ServiceManager) GetService(name string) Lifecycle {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.names[name]
}

// Count returns the number of registered services.
func (sm *ServiceManager) Count() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.services)
}
