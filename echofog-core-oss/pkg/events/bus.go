package events

import (
	"strings"
	"sync"
	"time"
)

// Emitter is the minimal interface for emitting events into the system.
// Any core component can take an optional Emitter (or nil) to announce state changes,
// metrics, or security alerts without any knowledge of transports or UI layers.
type Emitter interface {
	Emit(topic string, payload any)
}

// Event represents an event published through the event bus.
type Event struct {
	Topic     string    `json:"topic"`
	Timestamp time.Time `json:"timestamp"`
	Payload   any       `json:"payload"`
}

// Handler is a callback function invoked when a matching event occurs.
type Handler func(event Event)

type subscription struct {
	id      uint64
	pattern string
	ch      chan Event
	handler Handler
}

// Bus is a lightweight, thread-safe, non-blocking in-memory pub-sub event bus.
type Bus struct {
	mu         sync.RWMutex
	nextSubID  uint64
	subs       map[uint64]*subscription
	topicCache map[string][]*subscription // topic -> matching subs
}

// NewBus creates a new pub-sub event bus.
func NewBus() *Bus {
	return &Bus{
		subs:       make(map[uint64]*subscription),
		topicCache: make(map[string][]*subscription),
	}
}

// Emit broadcasts an event to all matching subscribers.
// It is non-blocking: if a subscriber's buffer is full, the event is dropped for that
// subscriber to prevent slow consumers from slowing down core network goroutines.
func (b *Bus) Emit(topic string, payload any) {
	if b == nil {
		return
	}

	b.mu.RLock()
	if len(b.subs) == 0 {
		b.mu.RUnlock()
		return
	}
	matching, cached := b.topicCache[topic]
	b.mu.RUnlock()

	if !cached {
		// Filling the cache writes a map, which needs the exclusive lock.
		b.mu.Lock()
		matching = b.getMatchingSubs(topic)
		b.mu.Unlock()
	}
	if len(matching) == 0 {
		return
	}

	event := Event{
		Topic:     topic,
		Timestamp: time.Now(),
		Payload:   payload,
	}

	// Sends stay under the read lock so an unsubscribe cannot close a channel mid-send; they
	// never block, so holding it is cheap.
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range matching {
		if _, live := b.subs[sub.id]; !live {
			continue
		}
		if sub.handler != nil {
			go sub.handler(event)
		}
		if sub.ch != nil {
			select {
			case sub.ch <- event:
			default:
				// Non-blocking drop on slow consumer
			}
		}
	}
}

// Subscribe opens a channel for events matching the specified pattern.
// Supported patterns:
//   - "network:peer_connected" (exact match)
//   - "security:*" (prefix wildcard)
//   - "*" (matches all events)
//
// Returns a receive-only channel and an unsubscribe cancel function.
func (b *Bus) Subscribe(pattern string, bufSize int) (<-chan Event, func()) {
	if bufSize <= 0 {
		bufSize = 64
	}
	ch := make(chan Event, bufSize)

	b.mu.Lock()
	b.nextSubID++
	id := b.nextSubID
	sub := &subscription{
		id:      id,
		pattern: pattern,
		ch:      ch,
	}
	b.subs[id] = sub
	b.topicCache = make(map[string][]*subscription) // invalidate cache
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if s, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(s.ch)
			b.topicCache = make(map[string][]*subscription)
		}
	}

	return ch, unsubscribe
}

// On registers a direct callback handler for events matching pattern.
// Returns an unsubscribe cancel function.
func (b *Bus) On(pattern string, handler Handler) func() {
	b.mu.Lock()
	b.nextSubID++
	id := b.nextSubID
	sub := &subscription{
		id:      id,
		pattern: pattern,
		handler: handler,
	}
	b.subs[id] = sub
	b.topicCache = make(map[string][]*subscription)
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs, id)
		b.topicCache = make(map[string][]*subscription)
	}
}

// getMatchingSubs resolves subscriptions for a topic and caches them (must be called with Lock held).
func (b *Bus) getMatchingSubs(topic string) []*subscription {
	if cached, ok := b.topicCache[topic]; ok {
		return cached
	}

	var res []*subscription
	for _, sub := range b.subs {
		if matchPattern(sub.pattern, topic) {
			res = append(res, sub)
		}
	}
	b.topicCache[topic] = res
	return res
}

func matchPattern(pattern, topic string) bool {
	if pattern == "*" || pattern == topic {
		return true
	}
	if strings.HasSuffix(pattern, ":*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(topic, prefix)
	}
	return false
}

// Ensure Bus implements Emitter
var _ Emitter = (*Bus)(nil)

// NopEmitter is a zero-cost no-op emitter for testing or when eventing is disabled.
type NopEmitter struct{}

func (NopEmitter) Emit(string, any) {}
