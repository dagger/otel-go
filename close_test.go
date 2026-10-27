package otel

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type shutdownErrorSpanProcessor struct{ err error }

func (shutdownErrorSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (shutdownErrorSpanProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (shutdownErrorSpanProcessor) ForceFlush(context.Context) error                { return nil }
func (p shutdownErrorSpanProcessor) Shutdown(context.Context) error                { return p.err }

type shutdownErrorLogProcessor struct{ err error }

func (shutdownErrorLogProcessor) OnEmit(context.Context, *sdklog.Record) error { return nil }
func (shutdownErrorLogProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool {
	return true
}
func (shutdownErrorLogProcessor) ForceFlush(context.Context) error { return nil }
func (p shutdownErrorLogProcessor) Shutdown(context.Context) error { return p.err }

type shutdownErrorMetricReader struct {
	sdkmetric.Reader
	err error
}

func (r shutdownErrorMetricReader) Shutdown(ctx context.Context) error {
	return errors.Join(r.err, r.Reader.Shutdown(ctx))
}

func TestCloseWithErrorReturnsAllProviderFailures(t *testing.T) {
	oldContext, oldTracer := closeCtx, tracerProvider
	t.Cleanup(func() { closeCtx, tracerProvider = oldContext, oldTracer })
	traceErr, logErr, metricErr := errors.New("trace failure"), errors.New("log failure"), errors.New("metric failure")
	tracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(shutdownErrorSpanProcessor{traceErr}))
	closeCtx = WithLoggerProvider(t.Context(), sdklog.NewLoggerProvider(
		sdklog.WithProcessor(shutdownErrorLogProcessor{logErr})))
	closeCtx = WithMeterProvider(closeCtx, sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(shutdownErrorMetricReader{Reader: sdkmetric.NewManualReader(), err: metricErr})))

	err := CloseWithError()
	for _, expected := range []error{traceErr, logErr, metricErr} {
		require.ErrorIs(t, err, expected)
	}
	for _, provider := range []string{"tracer", "logger", "meter"} {
		require.ErrorContains(t, err, "failed to shut down "+provider+" provider")
	}
}
