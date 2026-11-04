package otlp_test

import (
	"context"
	"testing"

	otel "github.com/dagger/otel-go"
	"github.com/dagger/otel-go/otlp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
)

func TestImportUsesOpenTelemetryExporters(t *testing.T) {
	require.Equal(t, otlp.Exporters{}, otel.OTLP)

	ctx := context.Background()
	for _, proto := range []string{"http/protobuf", "grpc"} {
		exp, err := otel.OTLP.SpanExporter(ctx, proto, "http://localhost:4317", nil)
		require.NoError(t, err, proto)
		require.IsType(t, &otlptrace.Exporter{}, exp, proto)
		require.NoError(t, exp.Shutdown(ctx))

		_, err = otel.OTLP.LogExporter(ctx, proto, "http://localhost:4317", nil)
		require.NoError(t, err, proto)
		_, err = otel.OTLP.MetricExporter(ctx, proto, "http://localhost:4317", nil)
		require.NoError(t, err, proto)
	}
}
