package otel_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	otel "github.com/dagger/otel-go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// otlpReceiver records the OTLP requests POSTed to it, by path.
type otlpReceiver struct {
	mu   sync.Mutex
	reqs map[string][]*http.Request
	body map[string][][]byte
}

func newOTLPReceiver(t *testing.T) (*otlpReceiver, *httptest.Server) {
	r := &otlpReceiver{reqs: map[string][]*http.Request{}, body: map[string][][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.reqs[req.URL.Path] = append(r.reqs[req.URL.Path], req)
		r.body[req.URL.Path] = append(r.body[req.URL.Path], b)
		r.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return r, srv
}

func (r *otlpReceiver) only(t *testing.T, path string, msg proto.Message) *http.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Len(t, r.body[path], 1, path)
	require.NoError(t, proto.Unmarshal(r.body[path][0], msg))
	return r.reqs[path][0]
}

var testResource = resource.NewSchemaless(attribute.String("service.name", "otlphttp-test"))

// The default exporters must send the engine what OpenTelemetry's HTTP
// exporters sent before them, so a module's telemetry doesn't change.
func TestOTLPHTTPSendsWhatOpenTelemetrySends(t *testing.T) {
	ctx := context.Background()
	ours, oursSrv := newOTLPReceiver(t)
	theirs, theirsSrv := newOTLPReceiver(t)
	headers := map[string]string{"X-Test": "yes"}

	t.Run("spans", func(t *testing.T) {
		oursExp, err := otel.OTLP.SpanExporter(ctx, "http/protobuf", oursSrv.URL+"/v1/traces", headers)
		require.NoError(t, err)
		theirsExp, err := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(theirsSrv.URL+"/v1/traces"),
			otlptracehttp.WithHeaders(headers))
		require.NoError(t, err)

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(testResource),
			sdktrace.WithSyncer(oursExp),
			sdktrace.WithSyncer(theirsExp))
		_, span := tp.Tracer("test", trace.WithInstrumentationVersion("v1"),
			trace.WithInstrumentationAttributes(attribute.String("scope", "attr"))).Start(ctx, "a span",
			trace.WithAttributes(attribute.Int("n", 1)))
		span.AddEvent("an event")
		span.End()
		require.NoError(t, tp.Shutdown(ctx))

		var got, want coltracepb.ExportTraceServiceRequest
		req := ours.only(t, "/v1/traces", &got)
		theirs.only(t, "/v1/traces", &want)
		require.Equal(t, "a span", got.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0].GetName())
		require.True(t, proto.Equal(&want, &got), "want %v\ngot  %v", &want, &got)
		require.Equal(t, "application/x-protobuf", req.Header.Get("Content-Type"))
		require.Equal(t, "yes", req.Header.Get("X-Test"))
	})

	t.Run("logs", func(t *testing.T) {
		oursExp, err := otel.OTLP.LogExporter(ctx, "http/protobuf", oursSrv.URL+"/v1/logs", headers)
		require.NoError(t, err)
		theirsExp, err := otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(theirsSrv.URL+"/v1/logs"),
			otlploghttp.WithHeaders(headers))
		require.NoError(t, err)

		lp := sdklog.NewLoggerProvider(
			sdklog.WithResource(testResource),
			sdklog.WithProcessor(sdklog.NewSimpleProcessor(oursExp)),
			sdklog.WithProcessor(sdklog.NewSimpleProcessor(theirsExp)))
		spanCtx := trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID{1},
			SpanID:     trace.SpanID{2},
			TraceFlags: trace.FlagsSampled,
		}))
		var rec log.Record
		rec.SetTimestamp(time.Unix(100, 0))
		rec.SetEventName("an.event")
		rec.SetSeverity(log.SeverityWarn)
		rec.SetSeverityText("WARN")
		rec.SetBody(log.StringValue("hello"))
		rec.AddAttributes(log.String("k", "v"), log.Map("m", log.Int("i", 1)))
		logger := lp.Logger("test", log.WithInstrumentationVersion("v1"),
			log.WithInstrumentationAttributes(attribute.String("scope", "attr")))
		logger.Emit(spanCtx, rec)
		// no span, no body, and an attribute with an empty value
		var bare log.Record
		bare.AddAttributes(log.KeyValue{Key: "empty"}, log.String("k", "v"))
		logger.Emit(ctx, bare)
		require.NoError(t, lp.Shutdown(ctx))

		var got, want collogspb.ExportLogsServiceRequest
		var gotRecs, wantRecs []proto.Message
		ours.mu.Lock()
		theirs.mu.Lock()
		require.Len(t, ours.body["/v1/logs"], 2)
		require.Len(t, theirs.body["/v1/logs"], 2)
		for i := range 2 {
			require.NoError(t, proto.Unmarshal(ours.body["/v1/logs"][i], &got))
			require.NoError(t, proto.Unmarshal(theirs.body["/v1/logs"][i], &want))
			gotRecs = append(gotRecs, proto.Clone(&got))
			wantRecs = append(wantRecs, proto.Clone(&want))
		}
		ours.mu.Unlock()
		theirs.mu.Unlock()
		for i := range gotRecs {
			require.True(t, proto.Equal(wantRecs[i], gotRecs[i]), "want %v\ngot  %v", wantRecs[i], gotRecs[i])
		}
		first := gotRecs[0].(*collogspb.ExportLogsServiceRequest).GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0]
		require.Equal(t, "hello", first.GetBody().GetStringValue())
		require.NotZero(t, first.GetObservedTimeUnixNano())
		barePB := gotRecs[1].(*collogspb.ExportLogsServiceRequest).GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0]
		require.Nil(t, barePB.GetBody().GetValue())
		require.Equal(t, log.StringValue(""), otel.LogValueFromPB(barePB.GetBody()))
	})

	t.Run("metrics", func(t *testing.T) {
		oursExp, err := otel.OTLP.MetricExporter(ctx, "http/protobuf", oursSrv.URL+"/v1/metrics", headers)
		require.NoError(t, err)
		theirsExp, err := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(theirsSrv.URL+"/v1/metrics"),
			otlpmetrichttp.WithHeaders(headers))
		require.NoError(t, err)

		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(testResource), sdkmetric.WithReader(reader))
		meter := mp.Meter("test", metric.WithInstrumentationAttributes(attribute.String("scope", "attr")))
		counter, err := meter.Int64Counter("a.counter")
		require.NoError(t, err)
		counter.Add(ctx, 3)
		hist, err := meter.Float64Histogram("a.histogram")
		require.NoError(t, err)
		hist.Record(ctx, 1.5)
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &rm))

		require.NoError(t, oursExp.Export(ctx, &rm))
		require.NoError(t, theirsExp.Export(ctx, &rm))

		var got, want colmetricspb.ExportMetricsServiceRequest
		ours.only(t, "/v1/metrics", &got)
		theirs.only(t, "/v1/metrics", &want)
		require.Len(t, got.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics(), 2)
		require.True(t, proto.Equal(&want, &got), "want %v\ngot  %v", &want, &got)
	})
}

func TestOTLPHTTPReportsRejection(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no thanks", http.StatusBadRequest)
	}))
	defer srv.Close()

	exp, err := otel.OTLP.LogExporter(ctx, "http/protobuf", srv.URL+"/v1/logs", nil)
	require.NoError(t, err)
	var rec sdklog.Record
	err = exp.Export(ctx, []sdklog.Record{rec})
	require.ErrorContains(t, err, "400 Bad Request")
	require.ErrorContains(t, err, "no thanks")
}

func TestOTLPHTTPRefusesGRPC(t *testing.T) {
	_, err := otel.OTLP.SpanExporter(context.Background(), "grpc", "http://localhost:4317", nil)
	require.ErrorContains(t, err, "github.com/dagger/otel-go/otlp")
}

// Generated module code imports this package; it must not make every module
// compile gRPC again. The test helpers send only http/protobuf too: they run
// in containers the engine gives http/protobuf, and dagger/go builds
// otelgotest from source for every test run.
func TestNoGRPC(t *testing.T) {
	for _, pkg := range []string{".", "./cmd/otelgotest", "./cmd/oteljunit", "./gotestmain", "./oteltestctx"} {
		out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
		require.NoError(t, err, string(out))
		for _, dep := range strings.Fields(string(out)) {
			require.False(t, strings.HasPrefix(dep, "google.golang.org/grpc"), "%s links %s", pkg, dep)
			require.False(t, strings.HasPrefix(dep, "go.opentelemetry.io/proto/otlp/collector/"), "%s links %s", pkg, dep)
		}
	}
}
