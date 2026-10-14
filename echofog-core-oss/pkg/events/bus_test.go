package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBus_EmitNoSubscribers(t *testing.T) {
	bus := NewBus()
	// Should not panic or block
	bus.Emit("any:topic", "test_payload")
	bus.Emit("test", 123)
}

func TestBus_ExactAndWildcardSubscription(t *testing.T) {
	bus := NewBus()

	chExact, unsubExact := bus.Subscribe("security:alert", 10)
	defer unsubExact()

	chWildcard, unsubWild := bus.Subscribe("security:*", 10)
	defer unsubWild()

	chAll, unsubAll := bus.Subscribe("*", 10)
	defer unsubAll()

	chOther, unsubOther := bus.Subscribe("network:*", 10)
	defer unsubOther()

	// Emit security:alert
	bus.Emit("security:alert", "attack detected")

	select {
	case ev := <-chExact:
		if ev.Payload.(string) != "attack detected" {
			t.Fatalf("unexpected payload: %v", ev.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("chExact did not receive event")
	}

	select {
	case ev := <-chWildcard:
		if ev.Topic != "security:alert" {
			t.Fatalf("unexpected topic: %v", ev.Topic)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("chWildcard did not receive event")
	}

	select {
	case ev := <-chAll:
		if ev.Topic != "security:alert" {
			t.Fatalf("unexpected topic in all: %v", ev.Topic)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("chAll did not receive event")
	}

	// chOther should NOT receive anything
	select {
	case ev := <-chOther:
		t.Fatalf("chOther unexpectedly received event: %v", ev)
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	bus := NewBus()

	ch, unsub := bus.Subscribe("test:topic", 10)
	bus.Emit("test:topic", "first")

	<-ch

	unsub()

	// After unsub, channel should be closed
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after unsub")
	}
}

func TestBus_OnCallback(t *testing.T) {
	bus := NewBus()

	var counter int32
	var wg sync.WaitGroup
	wg.Add(1)

	cancel := bus.On("count:*", func(ev Event) {
		atomic.AddInt32(&counter, 1)
		wg.Done()
	})
	defer cancel()

	bus.Emit("count:up", 1)
	wg.Wait()

	if atomic.LoadInt32(&counter) != 1 {
		t.Fatalf("expected counter 1, got %d", counter)
	}
}

func TestEventLogHandler(t *testing.T) {
	bus := NewBus()

	logWarnCh, unsubWarn := bus.Subscribe("log:warn", 10)
	defer unsubWarn()

	logErrCh, unsubErr := bus.Subscribe("log:error", 10)
	defer unsubErr()

	logAllCh, unsubAll := bus.Subscribe("log:*", 10)
	defer unsubAll()

	// Only WARN and above
	handler := NewEventLogHandler(bus, slog.LevelWarn)
	logger := slog.New(handler)

	// INFO should be dropped by minLevel
	logger.Info("this info should be ignored", "peer", "123")

	select {
	case ev := <-logAllCh:
		t.Fatalf("unexpected log: %v", ev)
	case <-time.After(50 * time.Millisecond):
		// OK
	}

	// WARN should be emitted
	logger.Warn("network latency high", "ms", 350)

	select {
	case ev := <-logWarnCh:
		payload := ev.Payload.(LogEventPayload)
		if payload.Message != "network latency high" {
			t.Fatalf("unexpected message: %s", payload.Message)
		}
		if payload.Attributes["ms"] != "350" {
			t.Fatalf("unexpected attribute: %v", payload.Attributes)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected warn log event")
	}

	// ERROR should be emitted
	logger.Error("connection dropped", "reason", "timeout")

	select {
	case ev := <-logErrCh:
		payload := ev.Payload.(LogEventPayload)
		if payload.Message != "connection dropped" {
			t.Fatalf("unexpected message: %s", payload.Message)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected error log event")
	}
}
