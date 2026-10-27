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
	return &blockingLogProcessor{BatchProcessor: sdklog.NewBatchProcessor(exporter,
		sdklog.WithMaxQueueSize(blockingLogQueueSize),
		sdklog.WithExportMaxBatchSize(512),
		sdklog.WithExportInterval(NearlyImmediate),
	)}
}

type blockingLogProcessor struct {
	*sdklog.BatchProcessor
	mu sync.Mutex
	// A conservative upper bound on queued records. Automatic exports can only
	// reduce the queue. A successful explicit flush resets this bound to zero.
	pending int
}

func (p *blockingLogProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == blockingLogQueueSize {
		if err := p.BatchProcessor.ForceFlush(ctx); err != nil {
			return err
		}
		p.pending = 0
	}
	if err := p.BatchProcessor.OnEmit(ctx, record); err != nil {
		return err
	}
	p.pending++
	return nil
}

func (p *blockingLogProcessor) ForceFlush(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.BatchProcessor.ForceFlush(ctx); err != nil {
		return err
	}
	p.pending = 0
	return nil
}

func (p *blockingLogProcessor) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.BatchProcessor.Shutdown(ctx)
}
