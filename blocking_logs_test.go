package otel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apilog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type blockedLogCapture struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	bodies  []string
}

func (e *blockedLogCapture) Export(ctx context.Context, records []sdklog.Record) error {
	e.once.Do(func() { close(e.started) })
	select {
	case <-e.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		e.bodies = append(e.bodies, r.Body().AsString())
	}
	return nil
}
func (*blockedLogCapture) ForceFlush(context.Context) error { return nil }
func (*blockedLogCapture) Shutdown(context.Context) error   { return nil }
func blockingTestRecord(body string) sdklog.Record {
	var r sdklog.Record
	r.SetBody(apilog.StringValue(body))
	return r
}

func TestBlockingLogProcessorPreservesOutputUnderLoad(t *testing.T) {
	e := &blockedLogCapture{started: make(chan struct{}), release: make(chan struct{})}
	p := NewBlockingLogProcessor(e)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(e.release) }) }
	t.Cleanup(func() { release(); require.NoError(t, p.Shutdown(context.Background())) })
	want := make([]string, blockingLogQueueSize*3)
	for i := range want {
		if i%17 == 0 {
			want[i] = strings.Repeat(fmt.Sprintf("stdout-%04d ", i), 400)
		} else {
			want[i] = fmt.Sprintf("engine-%04d", i)
		}
	}
	// Fill the admission budget while the exporter is blocked. The next record
	// must wait, rather than overwrite queued stdout as the SDK normally does.
	for _, body := range want[:blockingLogQueueSize] {
		r := blockingTestRecord(body)
		require.NoError(t, p.OnEmit(ctx, &r))
	}
	select {
	case <-e.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	done := make(chan error, 1)
	go func() {
		for _, body := range want[blockingLogQueueSize:] {
			r := blockingTestRecord(body)
			if err := p.OnEmit(ctx, &r); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		t.Fatalf("emitter did not wait for capacity: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, p.ForceFlush(ctx))
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Equal(t, want, e.bodies)
}

func TestBlockingLogProcessorCanceledFlushKeepsAdmissionBound(t *testing.T) {
	e := &blockedLogCapture{started: make(chan struct{}), release: make(chan struct{})}
	p := NewBlockingLogProcessor(e)
	defer func() { close(e.release); require.NoError(t, p.Shutdown(context.Background())) }()
	r := blockingTestRecord("accepted")
	for range blockingLogQueueSize {
		require.NoError(t, p.OnEmit(t.Context(), &r))
	}
	<-e.started
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	r = blockingTestRecord("not accepted")
	require.Error(t, p.OnEmit(ctx, &r))
	require.Equal(t, blockingLogQueueSize, p.(*blockingLogProcessor).pending, "failed flush must not admit more records")
}

func TestBlockingLogProcessorAcceptsCanceledContextWithCapacity(t *testing.T) {
	e := &blockedLogCapture{started: make(chan struct{}), release: make(chan struct{})}
	close(e.release)
	p := NewBlockingLogProcessor(e)
	t.Cleanup(func() { require.NoError(t, p.Shutdown(context.Background())) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, body := range []string{"before cancellation", "cleanup output", ""} {
		r := blockingTestRecord(body)
		require.NoError(t, p.OnEmit(ctx, &r), "cancellation alone must not discard an available record")
	}
	require.ErrorIs(t, p.ForceFlush(ctx), context.Canceled, "explicit flush still honors its context")
	require.NoError(t, p.ForceFlush(t.Context()))
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Equal(t, []string{"before cancellation", "cleanup output", ""}, e.bodies)
}

func TestInitWithCustomLogProcessor(t *testing.T) {
	e := &blockedLogCapture{started: make(chan struct{}), release: make(chan struct{})}
	close(e.release)
	p := NewBlockingLogProcessor(e)
	oldCount := len(LogProcessors)
	ctx := Init(t.Context(), Config{LogProcessors: []sdklog.Processor{p}})
	defer Close()
	stdio := SpanStdio(ctx, "test")
	_, err := stdio.Stdout.Write([]byte("complete output"))
	require.NoError(t, err)
	require.NoError(t, stdio.Close())
	require.NoError(t, LoggerProvider(ctx).ForceFlush(t.Context()))
	require.Same(t, p, LogProcessors[oldCount])
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Contains(t, e.bodies, "complete output")
}

func TestBlockingLogProcessorWaitHonorsCancellation(t *testing.T) {
	e := &blockedLogCapture{started: make(chan struct{}), release: make(chan struct{})}
	close(e.release)
	p := NewBlockingLogProcessor(e).(*blockingLogProcessor)
	t.Cleanup(func() { require.NoError(t, p.Shutdown(context.Background())) })
	acquired, err := p.acquire(t.Context())
	require.NoError(t, err)
	require.True(t, acquired)
	defer p.release()
	for _, operation := range []string{"emit", "flush"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if operation == "emit" {
					r := blockingTestRecord("waiting")
					done <- p.OnEmit(ctx, &r)
				} else {
					done <- p.ForceFlush(ctx)
				}
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case <-time.After(time.Second):
				t.Fatal("waiting for admission ignored the context deadline")
			}
		})
	}
}

func TestBlockingLogProcessorShutdownInterruptsAdmissionFlush(t *testing.T) {
	e := &blockedLogCapture{started: make(chan struct{}), release: make(chan struct{})}
	p := NewBlockingLogProcessor(e)
	t.Cleanup(func() { close(e.release); _ = p.Shutdown(context.Background()) })
	r := blockingTestRecord("output")
	for range blockingLogQueueSize {
		require.NoError(t, p.OnEmit(t.Context(), &r))
	}
	select {
	case <-e.started:
	case <-time.After(time.Second):
		t.Fatal("export did not start")
	}
	emitted := make(chan error, 1)
	go func() { emitted <- p.OnEmit(context.Background(), &r) }()
	select {
	case err := <-emitted:
		t.Fatalf("admission did not wait for the blocked exporter: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- p.Shutdown(ctx) }()
	select {
	case err := <-shutdown:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for admission despite its deadline")
	}
	select {
	case err := <-emitted:
		// Shutdown may win before the SDK flush starts, in which case the
		// stopped processor returns nil. An active flush returns cancellation.
		if err != nil {
			require.ErrorIs(t, err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not interrupt the active admission flush")
	}
	require.NoError(t, p.OnEmit(context.Background(), &r), "shutdown releases future emitters")
	require.NoError(t, p.ForceFlush(context.Background()))
}
