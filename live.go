package otel

import (
	"context"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// LiveSpanProcessor is a SpanProcessor whose OnStart calls OnEnd on the
// underlying SpanProcessor in order to send live telemetry.
type LiveSpanProcessor struct {
	sdktrace.SpanProcessor
}

func NewLiveSpanProcessor(exp sdktrace.SpanExporter) *LiveSpanProcessor {
	if exp != nil {
		exp = coalescingSpanExporter{exp}
	}
	return &LiveSpanProcessor{
		SpanProcessor: sdktrace.NewBatchSpanProcessor(
			// NOTE: span heartbeating is handled by the Cloud exporter
			exp,
			sdktrace.WithBatchTimeout(NearlyImmediate),
		),
	}
}

func (p *LiveSpanProcessor) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	// Send a read-only snapshot of the live span downstream so it can be
	// filtered out by FilterLiveSpansExporter. Otherwise the span can complete
	// before being exported, resulting in two completed spans being sent, which
	// will confuse traditional OpenTelemetry services.
	p.OnEnd(SnapshotSpan(span))
}

// coalescingSpanExporter keeps the latest update for each span in a batch. If a
// span starts and ends before the batch is exported, only its final state is sent.
// Updates in subsequent batches are still exported so long-running spans remain
// visible while they are running.
type coalescingSpanExporter struct {
	sdktrace.SpanExporter
}

func (exp coalescingSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	type spanKey struct {
		traceID trace.TraceID
		spanID  trace.SpanID
	}
	indices := make(map[spanKey]int, len(spans))
	batch := make([]sdktrace.ReadOnlySpan, 0, len(spans))
	for _, span := range spans {
		sc := span.SpanContext()
		key := spanKey{sc.TraceID(), sc.SpanID()}
		if i, ok := indices[key]; ok {
			batch[i] = span
		} else {
			indices[key] = len(batch)
			batch = append(batch, span)
		}
	}
	return exp.SpanExporter.ExportSpans(ctx, batch)
}
