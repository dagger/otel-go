package otel

import (
	"context"
	"sync"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

const blockingLogQueueSize = 2048

// NewBlockingLogProcessor batches logs without discarding queued records when
// the exporter falls behind. Emitters wait for capacity instead. Use it for
// local consumers such as a CLI frontend, where logs carry command output.
// Export failures and cancellation are still reported by the underlying SDK.
func NewBlockingLogProcessor(exporter sdklog.Exporter) sdklog.Processor {
	flushCtx, cancelFlush := context.WithCancel(context.Background())
	p := &blockingLogProcessor{BatchProcessor: sdklog.NewBatchProcessor(exporter,
		sdklog.WithMaxQueueSize(blockingLogQueueSize),
		sdklog.WithExportMaxBatchSize(512),
		sdklog.WithExportInterval(NearlyImmediate),
	), gate: make(chan struct{}, 1), stopped: make(chan struct{}),
		flushCtx: flushCtx, cancelFlush: cancelFlush}
	p.gate <- struct{}{}
	return p
}

type blockingLogProcessor struct {
	*sdklog.BatchProcessor
	// Admission and flushes are serialized, but waiting for admission must
	// honor the caller's context. Shutdown never waits for this gate.
	gate        chan struct{}
	stopped     chan struct{}
	stopOnce    sync.Once
	flushCtx    context.Context
	cancelFlush context.CancelFunc
	// A conservative upper bound on queued records. Automatic exports can only
	// reduce the queue. A successful explicit flush resets this bound to zero.
	pending int
}

func (p *blockingLogProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	acquired, err := p.acquire(ctx)
	if !acquired {
		return err
	}
	defer p.release()
	if p.pending == blockingLogQueueSize {
		if err := p.flush(ctx); err != nil {
			return err
		}
	}
	if err := p.BatchProcessor.OnEmit(ctx, record); err != nil {
		return err
	}
	p.pending++
	return nil
}

func (p *blockingLogProcessor) ForceFlush(ctx context.Context) error {
	acquired, err := p.acquire(ctx)
	if !acquired {
		return err
	}
	defer p.release()
	return p.flush(ctx)
}

// flush is called with the admission gate held. Stopping the processor must
// interrupt an active admission flush even if its caller has no deadline.
func (p *blockingLogProcessor) flush(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.flushCtx, cancel)
	defer stop()
	defer cancel()
	if err := p.BatchProcessor.ForceFlush(ctx); err != nil {
		return err
	}
	p.pending = 0
	return nil
}

func (p *blockingLogProcessor) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() {
		close(p.stopped)
		p.cancelFlush()
	})
	// The SDK supports concurrent Shutdown and ForceFlush. Do not acquire the
	// admission gate: its holder may be waiting for an unresponsive exporter.
	return p.BatchProcessor.Shutdown(ctx)
}

func (p *blockingLogProcessor) acquire(ctx context.Context) (bool, error) {
	select {
	case <-p.stopped:
		return false, nil
	default:
	}
	select {
	case <-p.stopped:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-p.gate:
	}
	// Cancellation and shutdown take priority if they raced with the gate.
	select {
	case <-p.stopped:
		p.release()
		return false, nil
	default:
	}
	if err := ctx.Err(); err != nil {
		p.release()
		return false, err
	}
	return true, nil
}

func (p *blockingLogProcessor) release() { p.gate <- struct{}{} }
