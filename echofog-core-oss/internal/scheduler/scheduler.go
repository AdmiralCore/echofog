package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultInterval is the standard background interval (15 minutes) if none specified
const DefaultInterval = 15 * time.Minute

// TaskFunc defines the signature of a background task
type TaskFunc func(ctx context.Context) error

// TaskStatus represents the current state of a task
type TaskStatus string

const (
	StatusIdle    TaskStatus = "idle"
	StatusRunning TaskStatus = "running"
	StatusStopped TaskStatus = "stopped"
	StatusError   TaskStatus = "error"
)

// TaskMeta contains runtime metrics for a task (inspectable via MCP / AI tools)
type TaskMeta struct {
	Name         string        `json:"name"`
	Interval     time.Duration `json:"interval"`
	Status       TaskStatus    `json:"status"`
	LastRun      time.Time     `json:"last_run"`
	LastDuration time.Duration `json:"last_duration"`
	RunsCount    uint64        `json:"runs_count"`
	ErrorsCount  uint64        `json:"errors_count"`
	LastError    string        `json:"last_error,omitempty"`
}

type taskHandle struct {
	name     string
	interval time.Duration
	fn       TaskFunc
	mu       sync.Mutex // guards meta: GetTasks reads it while the task runs
	meta     TaskMeta
	stopCh   chan struct{}
}

// TaskScheduler is the node-wide background runner coordinating background jobs,
// preventing unmanaged goroutines, memory leaks, and shielding from unhandled panics.
type TaskScheduler struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.RWMutex
	tasks  map[string]*taskHandle
	logger *slog.Logger
	closed atomic.Bool
}

// New creates a new centralized TaskScheduler bound to parent context
func New(parentCtx context.Context, logger *slog.Logger) *TaskScheduler {
	if logger == nil {
		logger = slog.Default().With("component", "scheduler")
	}
	ctx, cancel := context.WithCancel(parentCtx)
	return &TaskScheduler{
		ctx:    ctx,
		cancel: cancel,
		tasks:  make(map[string]*taskHandle),
		logger: logger,
	}
}

// Schedule registers a recurring background job.
// If interval <= 0, DefaultInterval (15 minutes) is used automatically.
func (s *TaskScheduler) Schedule(name string, interval time.Duration, fn TaskFunc) error {
	if s.closed.Load() {
		return errors.New("scheduler is stopped")
	}
	if fn == nil {
		return errors.New("task function cannot be nil")
	}

	if interval <= 0 {
		interval = DefaultInterval
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// If task with same name already exists, stop the previous one
	if old, exists := s.tasks[name]; exists {
		close(old.stopCh)
		delete(s.tasks, name)
	}

	handle := &taskHandle{
		name:     name,
		interval: interval,
		fn:       fn,
		meta: TaskMeta{
			Name:     name,
			Interval: interval,
			Status:   StatusIdle,
		},
		stopCh: make(chan struct{}),
	}

	s.tasks[name] = handle
	s.wg.Add(1)
	go s.runLoop(handle)

	s.logger.Debug("Scheduler: Registered task", "name", name, "interval", interval)
	return nil
}

// ScheduleOnce executes a function once after a specified delay
func (s *TaskScheduler) ScheduleOnce(name string, delay time.Duration, fn TaskFunc) error {
	if s.closed.Load() {
		return errors.New("scheduler is stopped")
	}
	if fn == nil {
		return errors.New("task function cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	handle := &taskHandle{
		name:     name,
		interval: delay,
		fn:       fn,
		meta: TaskMeta{
			Name:     name,
			Interval: delay,
			Status:   StatusIdle,
		},
		stopCh: make(chan struct{}),
	}

	s.tasks[name] = handle
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-s.ctx.Done():
			return
		case <-handle.stopCh:
			return
		case <-timer.C:
			s.executeTask(handle)
			s.mu.Lock()
			delete(s.tasks, name)
			s.mu.Unlock()
		}
	}()

	return nil
}

// Cancel stops and removes a scheduled task by name
func (s *TaskScheduler) Cancel(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if handle, exists := s.tasks[name]; exists {
		close(handle.stopCh)
		delete(s.tasks, name)
		s.logger.Debug("Scheduler: Cancelled task", "name", name)
		return true
	}
	return false
}

// GetTasks returns a snapshot of all active tasks and their execution metrics (for MCP/AI)
func (s *TaskScheduler) GetTasks() []TaskMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]TaskMeta, 0, len(s.tasks))
	for _, h := range s.tasks {
		h.mu.Lock()
		result = append(result, h.meta)
		h.mu.Unlock()
	}
	return result
}

// Stop gracefully stops all scheduled tasks and waits for in-flight tasks to exit
func (s *TaskScheduler) Stop() {
	if s.closed.Swap(true) {
		return
	}

	s.logger.Info("Scheduler: Stopping background tasks...")
	s.cancel()

	s.mu.Lock()
	for _, h := range s.tasks {
		select {
		case <-h.stopCh:
		default:
			close(h.stopCh)
		}
	}
	s.mu.Unlock()

	s.wg.Wait()
	s.logger.Info("Scheduler: All background tasks stopped cleanly")
}

func (s *TaskScheduler) runLoop(h *taskHandle) {
	defer s.wg.Done()

	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-h.stopCh:
			return
		case <-ticker.C:
			s.executeTask(h)
		}
	}
}

func (s *TaskScheduler) executeTask(h *taskHandle) {
	h.mu.Lock()
	h.meta.Status = StatusRunning
	h.mu.Unlock()
	start := time.Now()

	// Panic shield: isolated recovery so a buggy task never crashes the node
	defer func() {
		dur := time.Since(start)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.meta.LastRun = start
		h.meta.LastDuration = dur
		h.meta.RunsCount++

		if r := recover(); r != nil {
			h.meta.Status = StatusError
			h.meta.ErrorsCount++
			h.meta.LastError = fmt.Sprintf("panic: %v", r)
			s.logger.Error("Scheduler: Task panicked", "name", h.name, "panic", r)
		} else if h.meta.Status == StatusRunning {
			h.meta.Status = StatusIdle
		}
	}()

	err := h.fn(s.ctx)
	if err != nil {
		h.mu.Lock()
		h.meta.Status = StatusError
		h.meta.ErrorsCount++
		h.meta.LastError = err.Error()
		h.mu.Unlock()
		s.logger.Warn("Scheduler: Task returned error", "name", h.name, "err", err)
	}
}
