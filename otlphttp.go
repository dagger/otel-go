package otel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	otlpmetricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	otlptracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// otlpHTTPExporters is the default OTLP: http/protobuf only, without
// OpenTelemetry's OTLP HTTP exporters, which link gRPC through their shared
// configuration and through the collector packages holding the request types.
// Spans still go through OpenTelemetry's otlptrace exporter, which doesn't.
type otlpHTTPExporters struct{}

func (otlpHTTPExporters) SpanExporter(ctx context.Context, proto, endpoint string, headers map[string]string) (sdktrace.SpanExporter, error) {
	c, err := newOTLPHTTPClient(proto, endpoint, headers)
	if err != nil {
		return nil, err
	}
	exp, err := otlptrace.New(ctx, otlpHTTPTraceClient{c})
	if err != nil {
		return nil, err
	}
	return exp, nil
}

func (otlpHTTPExporters) LogExporter(ctx context.Context, proto, endpoint string, headers map[string]string) (sdklog.Exporter, error) {
	c, err := newOTLPHTTPClient(proto, endpoint, headers)
	if err != nil {
		return nil, err
	}
	return otlpHTTPLogExporter{c}, nil
}

func (otlpHTTPExporters) MetricExporter(ctx context.Context, proto, endpoint string, headers map[string]string) (sdkmetric.Exporter, error) {
	c, err := newOTLPHTTPClient(proto, endpoint, headers)
	if err != nil {
		return nil, err
	}
	return otlpHTTPMetricExporter{c}, nil
}

type otlpHTTPClient struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
}

func newOTLPHTTPClient(proto, endpoint string, headers map[string]string) (*otlpHTTPClient, error) {
	switch proto {
	case "http/protobuf", "http":
	case "grpc":
		return nil, fmt.Errorf("OTLP protocol %s needs github.com/dagger/otel-go/otlp imported", proto)
	default:
		return nil, fmt.Errorf("unknown OTLP protocol: %s", proto)
	}
	return &otlpHTTPClient{
		endpoint: endpoint,
		headers:  headers,
		// OpenTelemetry's HTTP exporters' default timeout
		client: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// export POSTs an OTLP Export{Trace,Logs,Metrics}ServiceRequest holding
// resources. Each of those requests has one field, number 1: the repeated
// resources. So the request is each resource's encoding as that field, with no
// need for the request types.
func export[M proto.Message](ctx context.Context, c *otlpHTTPClient, resources []M) error {
	var body []byte
	for _, r := range resources {
		b, err := proto.Marshal(r)
		if err != nil {
			return err
		}
		body = protowire.AppendTag(body, 1, protowire.BytesType)
		body = protowire.AppendBytes(body, b)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("failed to send to %s: %s (body: %s)", c.endpoint, resp.Status, strings.TrimSpace(string(msg)))
}

type otlpHTTPTraceClient struct {
	c *otlpHTTPClient
}

var _ otlptrace.Client = otlpHTTPTraceClient{}

func (otlpHTTPTraceClient) Start(context.Context) error { return nil }

func (t otlpHTTPTraceClient) Stop(context.Context) error {
	t.c.client.CloseIdleConnections()
	return nil
}

func (t otlpHTTPTraceClient) UploadTraces(ctx context.Context, spans []*otlptracev1.ResourceSpans) error {
	return export(ctx, t.c, spans)
}

type otlpHTTPLogExporter struct {
	c *otlpHTTPClient
}

var _ sdklog.Exporter = otlpHTTPLogExporter{}

func (e otlpHTTPLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if len(records) == 0 {
		return nil
	}
	return export(ctx, e.c, LogsToPB(records))
}

func (e otlpHTTPLogExporter) Shutdown(context.Context) error {
	e.c.client.CloseIdleConnections()
	return nil
}

func (otlpHTTPLogExporter) ForceFlush(context.Context) error { return nil }

type otlpHTTPMetricExporter struct {
	c *otlpHTTPClient
}

var _ sdkmetric.Exporter = otlpHTTPMetricExporter{}

func (otlpHTTPMetricExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

func (otlpHTTPMetricExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

// Export sends what converts, as OpenTelemetry's exporter does, and reports
// what didn't.
func (e otlpHTTPMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	pb, convErr := ResourceMetricsToPB(rm)
	return errors.Join(convErr, export(ctx, e.c, []*otlpmetricsv1.ResourceMetrics{pb}))
}

func (otlpHTTPMetricExporter) ForceFlush(context.Context) error { return nil }

func (e otlpHTTPMetricExporter) Shutdown(context.Context) error {
	e.c.client.CloseIdleConnections()
	return nil
}
