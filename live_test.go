package otel_test

import (
	"context"
	"testing"
	"time"

	otel "github.com/dagger/otel-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestCoalescingSpanExporter(t *testing.T) {
	start := tracetest.SpanStub{
		Name: "running",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{1},
			SpanID:  trace.SpanID{1},
		}),
		StartTime: time.Now(),
	}
	update := start
	update.Name = "updated"
	end := update
	end.EndTime = start.StartTime.Add(time.Millisecond)
	end.Attributes = []attribute.KeyValue{attribute.String("result", "done")}
	endUpdate := end
	endUpdate.Name = "completed update"
	zeroDuration := end
	zeroDuration.EndTime = zeroDuration.StartTime
	otherSpan := start
	otherSpan.SpanContext = start.SpanContext.WithSpanID(trace.SpanID{2})
	otherTrace := start
	otherTrace.SpanContext = start.SpanContext.WithTraceID(trace.TraceID{2})

	for _, tc := range []struct {
		name    string
		batches []tracetest.SpanStubs
		want    tracetest.SpanStubs
	}{
		{
			name:    "completed within a batch",
			batches: []tracetest.SpanStubs{{start, otherSpan, update, end}},
			want:    tracetest.SpanStubs{end, otherSpan},
		},
		{
			name:    "completed before started",
			batches: []tracetest.SpanStubs{{end, start}},
			want:    tracetest.SpanStubs{end},
		},
		{
			name:    "live updates cannot replace completion",
			batches: []tracetest.SpanStubs{{start, end, update}},
			want:    tracetest.SpanStubs{end},
		},
		{
			name:    "last completed update wins",
			batches: []tracetest.SpanStubs{{end, start, endUpdate, update}},
			want:    tracetest.SpanStubs{endUpdate},
		},
		{
			name:    "zero duration completion before started",
			batches: []tracetest.SpanStubs{{zeroDuration, start}},
			want:    tracetest.SpanStubs{zeroDuration},
		},
		{
			name:    "latest live update",
			batches: []tracetest.SpanStubs{{start, update}},
			want:    tracetest.SpanStubs{update},
		},
		{
			name:    "completed in a later batch",
			batches: []tracetest.SpanStubs{{start}, {end}},
			want:    tracetest.SpanStubs{start, end},
		},
		{
			name:    "same span ID in different traces",
			batches: []tracetest.SpanStubs{{start, otherTrace, end}},
			want:    tracetest.SpanStubs{end, otherTrace},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := tracetest.NewInMemoryExporter()
			exporter := otel.CoalescingSpanExporter{SpanExporter: recorder}
			for _, batch := range tc.batches {
				spans := batch.Snapshots()
				require.NoError(t, exporter.ExportSpans(t.Context(), spans))
				assert.Equal(t, batch, tracetest.SpanStubsFromReadOnlySpans(spans), "input must remain unchanged")
			}
			assert.Equal(t, tc.want, recorder.GetSpans())
		})
	}
}

func TestLiveSpanProcessorExportsStartAndEnd(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	processor := otel.NewLiveSpanProcessor(exporter)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.Background()))
	})

	_, span := provider.Tracer("test").Start(t.Context(), "live")
	require.NoError(t, processor.ForceFlush(t.Context()))
	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.True(t, spans[0].EndTime.Before(spans[0].StartTime))

	span.SetAttributes(attribute.Bool("finished", true))
	span.End()
	require.NoError(t, processor.ForceFlush(t.Context()))
	spans = exporter.GetSpans()
	require.Len(t, spans, 2)
	assert.Equal(t, spans[0].SpanContext, spans[1].SpanContext)
	assert.False(t, spans[1].EndTime.Before(spans[1].StartTime))
	assert.Contains(t, spans[1].Attributes, attribute.Bool("finished", true))
}
