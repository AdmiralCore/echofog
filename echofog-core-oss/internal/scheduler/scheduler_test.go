package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduler_RecurringAndPanicShield(t *testing.T) {
	sched := New(context.Background(), nil)
	defer sched.Stop()

	var counter atomic.Int32
	var panicCounter atomic.Int32

	// Normal recurring task
	err := sched.Schedule("counter_task", 20*time.Millisecond, func(ctx context.Context) error {
		counter.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("failed to schedule task: %v", err)
	}

	// Task that panics (should be absorbed by panic shield without crashing)
	err = sched.Schedule("panic_task", 25*time.Millisecond, func(ctx context.Context) error {
		panicCounter.Add(1)
		panic("simulated panic in background task")
	})
	if err != nil {
		t.Fatalf("failed to schedule panic task: %v", err)
	}

	time.Sleep(80 * time.Millisecond)

	if counter.Load() < 2 {
		t.Fatalf("expected counter >= 2, got %d", counter.Load())
	}
	if panicCounter.Load() < 2 {
		t.Fatalf("expected panicCounter >= 2, got %d", panicCounter.Load())
	}

	tasks := sched.GetTasks()
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks in metrics, got %d", len(tasks))
	}
}

func TestScheduler_ScheduleOnce(t *testing.T) {
	sched := New(context.Background(), nil)
	defer sched.Stop()

	var ran atomic.Bool

	err := sched.ScheduleOnce("one_off", 20*time.Millisecond, func(ctx context.Context) error {
		ran.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("failed to schedule once: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if !ran.Load() {
		t.Fatalf("expected one_off task to have run")
	}

	// Should be automatically cleaned up after execution
	if len(sched.GetTasks()) != 0 {
		t.Fatalf("expected one_off task to be removed after execution")
	}
}

func TestScheduler_Cancel(t *testing.T) {
	sched := New(context.Background(), nil)
	defer sched.Stop()

	var counter atomic.Int32

	sched.Schedule("to_cancel", 15*time.Millisecond, func(ctx context.Context) error {
		counter.Add(1)
		return nil
	})

	time.Sleep(40 * time.Millisecond)
	sched.Cancel("to_cancel")

	valAtCancel := counter.Load()
	time.Sleep(40 * time.Millisecond)

	if counter.Load() != valAtCancel {
		t.Fatalf("task kept running after cancellation")
	}
}

func TestScheduler_DefaultInterval(t *testing.T) {
	sched := New(context.Background(), nil)
	defer sched.Stop()

	err := sched.Schedule("default_interval_task", 0, func(ctx context.Context) error {
		return errors.New("err")
	})
	if err != nil {
		t.Fatalf("schedule failed: %v", err)
	}

	tasks := sched.GetTasks()
	if len(tasks) != 1 || tasks[0].Interval != DefaultInterval {
		t.Fatalf("expected task interval to be DefaultInterval (%v), got %v", DefaultInterval, tasks[0].Interval)
	}
}
