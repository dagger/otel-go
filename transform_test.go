package otel_test

import (
	"testing"

	otel "github.com/dagger/otel-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	otlpcommonv1 "go.opentelemetry.io/proto/otlp/common/v1"
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
