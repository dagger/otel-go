package gotest

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/trace"
)

// SpanContextRegistry allows external coordinators to retrieve span contexts
// for test spans created by [Run]. This enables cross-process span context
// propagation: an external process creates the span, and the test binary
// retrieves its context via a sideband (e.g., Unix socket) so that
// in-process operations become children of the externally created span.
type SpanContextRegistry struct {
	mu      sync.Mutex
	spans   map[string]trace.SpanContext
	waiters map[string][]chan trace.SpanContext
	closed  bool
}

// NewSpanContextRegistry creates a new SpanContextRegistry.
func NewSpanContextRegistry() *SpanContextRegistry {
	return &SpanContextRegistry{
		spans:   make(map[string]trace.SpanContext),
		waiters: make(map[string][]chan trace.SpanContext),
	}
}

// Register stores a span context for the given test name and unblocks
// any goroutines waiting for it via [WaitFor].
func (r *SpanContextRegistry) Register(testName string, sc trace.SpanContext) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans[testName] = sc
	if waiters, ok := r.waiters[testName]; ok {
		for _, ch := range waiters {
			ch <- sc
		}
		delete(r.waiters, testName)
	}
}

// WaitFor blocks until a span context is registered for the given test name,
// the context is canceled, or the registry is closed. Returns the span context
// and true on success, or a zero span context and false otherwise.
func (r *SpanContextRegistry) WaitFor(ctx context.Context, testName string) (trace.SpanContext, bool) {
	r.mu.Lock()
	if sc, ok := r.spans[testName]; ok {
		r.mu.Unlock()
		return sc, true
	}
	if r.closed {
		r.mu.Unlock()
		return trace.SpanContext{}, false
	}
	ch := make(chan trace.SpanContext, 1)
	r.waiters[testName] = append(r.waiters[testName], ch)
	r.mu.Unlock()

	select {
	case sc, ok := <-ch:
		return sc, ok
	case <-ctx.Done():
		return trace.SpanContext{}, false
	}
}

// Close unblocks all pending WaitFor calls.
func (r *SpanContextRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, waiters := range r.waiters {
		for _, ch := range waiters {
			close(ch)
		}
	}
	r.waiters = nil
}
