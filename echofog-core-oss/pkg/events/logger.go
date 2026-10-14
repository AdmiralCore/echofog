package events

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// LogEventPayload represents a structured log entry sent over the event bus
type LogEventPayload struct {
	Level      string            `json:"level"`
	Message    string            `json:"message"`
	Time       time.Time         `json:"time"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// EventLogHandler is an slog.Handler that forwards structured log records to an Emitter.
// Emits to topics such as "log:debug", "log:info", "log:warn", "log:error".
type EventLogHandler struct {
	emitter  Emitter
	minLevel slog.Level
	attrs    []slog.Attr
	groups   []string
}

// NewEventLogHandler creates an slog.Handler that pipes logs into an Emitter
func NewEventLogHandler(emitter Emitter, minLevel slog.Level) *EventLogHandler {
	return &EventLogHandler{
		emitter:  emitter,
		minLevel: minLevel,
	}
}

// Enabled returns true if the level is greater than or equal to minLevel
func (h *EventLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	if h.emitter == nil {
		return false
	}
	return level >= h.minLevel
}

// Handle serializes the slog.Record and emits it to topic "log:<level>"
func (h *EventLogHandler) Handle(_ context.Context, record slog.Record) error {
	if h.emitter == nil || record.Level < h.minLevel {
		return nil
	}

	attrsMap := make(map[string]string)
	for _, a := range h.attrs {
		attrsMap[a.Key] = fmt.Sprint(a.Value.Any())
	}
	record.Attrs(func(a slog.Attr) bool {
		attrsMap[a.Key] = fmt.Sprint(a.Value.Any())
		return true
	})

	levelStr := strings.ToLower(record.Level.String())
	payload := LogEventPayload{
		Level:      levelStr,
		Message:    record.Message,
		Time:       record.Time,
		Attributes: attrsMap,
	}

	// Emit to specific level topic, e.g. "log:error", "log:warn", "log:info"
	topic := "log:" + levelStr
	h.emitter.Emit(topic, payload)

	return nil
}

// WithAttrs returns a new handler with additional attributes pre-attached
func (h *EventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)

	return &EventLogHandler{
		emitter:  h.emitter,
		minLevel: h.minLevel,
		attrs:    newAttrs,
		groups:   h.groups,
	}
}

// WithGroup returns a new handler with a group name appended
func (h *EventLogHandler) WithGroup(name string) slog.Handler {
	newGroups := append(append([]string{}, h.groups...), name)
	return &EventLogHandler{
		emitter:  h.emitter,
		minLevel: h.minLevel,
		attrs:    h.attrs,
		groups:   newGroups,
	}
}

// CompositeHandler fans out slog records to multiple handlers (e.g. stdout + the event bus)
type CompositeHandler struct {
	handlers []slog.Handler
}

// NewCompositeHandler creates a handler that forwards records to all provided handlers
func NewCompositeHandler(handlers ...slog.Handler) *CompositeHandler {
	return &CompositeHandler{handlers: handlers}
}

func (c *CompositeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range c.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (c *CompositeHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, h := range c.handlers {
		if h.Enabled(ctx, record.Level) {
			_ = h.Handle(ctx, record)
		}
	}
	return nil
}

func (c *CompositeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := make([]slog.Handler, len(c.handlers))
	for i, h := range c.handlers {
		cloned[i] = h.WithAttrs(attrs)
	}
	return &CompositeHandler{handlers: cloned}
}

func (c *CompositeHandler) WithGroup(name string) slog.Handler {
	cloned := make([]slog.Handler, len(c.handlers))
	for i, h := range c.handlers {
		cloned[i] = h.WithGroup(name)
	}
	return &CompositeHandler{handlers: cloned}
}
