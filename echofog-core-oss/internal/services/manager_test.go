package services

import (
	"context"
	"errors"
	"testing"
)

type mockService struct {
	name      string
	startErr  error
	stopErr   error
	isStarted bool
	isStopped bool
}

func (m *mockService) Name() string {
	return m.name
}

func (m *mockService) Start(ctx context.Context) error {
	if m.startErr != nil {
		return m.startErr
	}
	m.isStarted = true
	return nil
}

func (m *mockService) Stop() error {
	if m.stopErr != nil {
		return m.stopErr
	}
	m.isStopped = true
	return nil
}

func TestServiceManager_RegistrationAndOrder(t *testing.T) {
	sm := NewServiceManager(nil)

	s1 := &mockService{name: "ServiceA"}
	s2 := &mockService{name: "ServiceB"}

	if err := sm.Register(s1); err != nil {
		t.Fatalf("unexpected error registering s1: %v", err)
	}
	if err := sm.Register(s2); err != nil {
		t.Fatalf("unexpected error registering s2: %v", err)
	}

	if sm.Count() != 2 {
		t.Fatalf("expected count 2, got %d", sm.Count())
	}

	// Duplicate registration must fail
	if err := sm.Register(&mockService{name: "ServiceA"}); err == nil {
		t.Fatalf("expected error for duplicate registration, got nil")
	}
}

func TestServiceManager_StartAndStop(t *testing.T) {
	sm := NewServiceManager(nil)

	executionLog := make([]string, 0)

	s1 := &loggingService{name: "S1", log: &executionLog}
	s2 := &loggingService{name: "S2", log: &executionLog}

	_ = sm.Register(s1)
	_ = sm.Register(s2)

	ctx := context.Background()
	if err := sm.StartAll(ctx); err != nil {
		t.Fatalf("failed to start all services: %v", err)
	}

	// Check start order: S1 started, then S2 started
	if len(executionLog) != 2 || executionLog[0] != "start:S1" || executionLog[1] != "start:S2" {
		t.Fatalf("unexpected start order: %v", executionLog)
	}

	if err := sm.StopAll(); err != nil {
		t.Fatalf("failed to stop all services: %v", err)
	}

	// Check stop order: S2 stopped, then S1 stopped (reverse order)
	if len(executionLog) != 4 || executionLog[2] != "stop:S2" || executionLog[3] != "stop:S1" {
		t.Fatalf("unexpected stop order: %v", executionLog)
	}
}

func TestServiceManager_RollbackOnFailure(t *testing.T) {
	sm := NewServiceManager(nil)

	executionLog := make([]string, 0)

	s1 := &loggingService{name: "S1", log: &executionLog}
	s2 := &loggingService{name: "S2", log: &executionLog, startErr: errors.New("boom")}

	_ = sm.Register(s1)
	_ = sm.Register(s2)

	err := sm.StartAll(context.Background())
	if err == nil {
		t.Fatalf("expected error from StartAll, got nil")
	}

	// S1 was started, then S2 failed, so S1 must be rolled back (stopped)
	if len(executionLog) != 2 || executionLog[0] != "start:S1" || executionLog[1] != "stop:S1" {
		t.Fatalf("unexpected rollback order: %v", executionLog)
	}
}

type loggingService struct {
	name     string
	log      *[]string
	startErr error
}

func (l *loggingService) Name() string { return l.name }
func (l *loggingService) Start(ctx context.Context) error {
	if l.startErr != nil {
		return l.startErr
	}
	*l.log = append(*l.log, "start:"+l.name)
	return nil
}
func (l *loggingService) Stop() error {
	*l.log = append(*l.log, "stop:"+l.name)
	return nil
}
