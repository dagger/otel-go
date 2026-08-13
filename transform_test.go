package otel_test

import (
	"context"
	"testing"

	otel "github.com/dagger/otel-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	otlpcommonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	otlplogsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	otlptracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestLogValuePBRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		val    log.Value
		wantPB any
	}{
		{
			name:   "bool",
			val:    log.BoolValue(true),
			wantPB: &otlpcommonv1.AnyValue_BoolValue{BoolValue: true},
		},
		{
			name:   "int64",
			val:    log.Int64Value(42),
			wantPB: &otlpcommonv1.AnyValue_IntValue{IntValue: 42},
		},
		{
			name:   "float64",
			val:    log.Float64Value(1.5),
			wantPB: &otlpcommonv1.AnyValue_DoubleValue{DoubleValue: 1.5},
		},
		{
			name:   "string",
			val:    log.StringValue("hello"),
			wantPB: &otlpcommonv1.AnyValue_StringValue{StringValue: "hello"},
		},
		{
			name:   "bytes",
			val:    log.BytesValue([]byte{0x00, 0x01, 0xff}),
			wantPB: &otlpcommonv1.AnyValue_BytesValue{BytesValue: []byte{0x00, 0x01, 0xff}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pb := otel.LogValueToPB(tc.val)
			require.NotNil(t, pb)
			assert.Equal(t, tc.wantPB, pb.Value)

			got := otel.LogValueFromPB(pb)
			assert.Equal(t, tc.val.Kind(), got.Kind())
			assert.True(t, tc.val.Equal(got), "expected %v, got %v", tc.val, got)
		})
	}
}

// resource is an optional field of ResourceSpans, so a payload that omits it
// must still convert.
func TestSpansFromPBNilResource(t *testing.T) {
	spans := otel.SpansFromPB([]*otlptracev1.ResourceSpans{
		{
			Resource: nil,
			ScopeSpans: []*otlptracev1.ScopeSpans{
				{
					Scope: &otlpcommonv1.InstrumentationScope{Name: "test"},
					Spans: []*otlptracev1.Span{
						{Name: "span-without-resource"},
					},
				},
			},
		},
	})

	require.Len(t, spans, 1)
	assert.Equal(t, "span-without-resource", spans[0].Name())

	res := spans[0].Resource()
	require.NotNil(t, res)
	assert.Empty(t, res.Attributes())
}

// body is an optional field of LogRecord; an attribute-only record with no
// body (and no resource) must still convert.
func TestReexportLogsFromPBNilResourceAndBody(t *testing.T) {
	exp := &collectExporter{}

	err := otel.ReexportLogsFromPB(context.Background(), exp, &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*otlplogsv1.ResourceLogs{
			{
				Resource: nil,
				ScopeLogs: []*otlplogsv1.ScopeLogs{
					{
						Scope: &otlpcommonv1.InstrumentationScope{Name: "test"},
						LogRecords: []*otlplogsv1.LogRecord{
							{
								Body: nil,
								Attributes: []*otlpcommonv1.KeyValue{
									{
										Key:   "foo",
										Value: &otlpcommonv1.AnyValue{Value: &otlpcommonv1.AnyValue_StringValue{StringValue: "bar"}},
									},
									{
										Key:   "count",
										Value: &otlpcommonv1.AnyValue{Value: &otlpcommonv1.AnyValue_IntValue{IntValue: 3}},
									},
								},
							},
						},
					},
				},
			},
		},
	})
	require.NoError(t, err)

	require.Len(t, exp.records, 1)
	rec := exp.records[0]

	assert.Equal(t, log.KindString, rec.Body().Kind())
	assert.Empty(t, rec.Body().AsString())

	attrs := map[string]log.Value{}
	rec.WalkAttributes(func(kv log.KeyValue) bool {
		attrs[kv.Key] = kv.Value
		return true
	})
	require.Len(t, attrs, 2)
	assert.True(t, log.StringValue("bar").Equal(attrs["foo"]), "expected bar, got %v", attrs["foo"])
	assert.True(t, log.Int64Value(3).Equal(attrs["count"]), "expected 3, got %v", attrs["count"])
}

// value is an optional field of KeyValue, so an attribute that omits it must
// still convert.
func TestAttributesFromProtoNilValue(t *testing.T) {
	attrs := otel.AttributesFromProto([]*otlpcommonv1.KeyValue{
		{Key: "missing", Value: nil},
	})

	require.Len(t, attrs, 1)
	assert.Equal(t, attribute.Key("missing"), attrs[0].Key)
	assert.Equal(t, attribute.STRING, attrs[0].Value.Type())
	assert.Empty(t, attrs[0].Value.AsString())
}

type collectExporter struct {
	records []sdklog.Record
}

var _ sdklog.Exporter = (*collectExporter)(nil)

func (e *collectExporter) Export(ctx context.Context, records []sdklog.Record) error {
	for _, rec := range records {
		e.records = append(e.records, rec.Clone())
	}
	return nil
}

func (e *collectExporter) Shutdown(ctx context.Context) error { return nil }

func (e *collectExporter) ForceFlush(ctx context.Context) error { return nil }
