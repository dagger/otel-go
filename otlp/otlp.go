// Package otlp makes otel-go export with OpenTelemetry's own OTLP exporters,
// over http/protobuf or grpc, with their TLS, compression and retry settings.
// Import it for its side effect in programs that send telemetry to collectors
// other than the engine:
//
//	import _ "github.com/dagger/otel-go/otlp"
//
// Without it, otel-go sends http/protobuf only, and links no gRPC.
package otlp

import (
	"context"
	"fmt"
	"net"
	"net/url"

	otel "github.com/dagger/otel-go"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
)

func init() {
	otel.OTLP = Exporters{}
}

// Exporters builds OpenTelemetry's OTLP exporters.
type Exporters struct{}

var _ otel.OTLPExporters = Exporters{}

func (Exporters) SpanExporter(ctx context.Context, proto, endpoint string, headers map[string]string) (sdktrace.SpanExporter, error) {
	var (
		exp sdktrace.SpanExporter
		err error
	)
	//nolint:dupl
	switch proto {
	case "http/protobuf", "http":
		exp, err = otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(endpoint),
			otlptracehttp.WithHeaders(headers))
	case "grpc":
		var u *url.URL
		u, err = url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("bad OTLP span endpoint %q: %w", endpoint, err)
		}
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpointURL(endpoint),
		}
		if u.Scheme == "unix" {
			dialer := func(ctx context.Context, addr string) (net.Conn, error) {
				return net.Dial(u.Scheme, u.Path)
			}
			opts = append(opts,
				otlptracegrpc.WithDialOption(grpc.WithContextDialer(dialer)),
				otlptracegrpc.WithInsecure())
		}
		exp, err = otlptracegrpc.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown OTLP protocol: %s", proto)
	}
	if err != nil {
		return nil, err
	}
	return exp, nil
}

func (Exporters) LogExporter(ctx context.Context, proto, endpoint string, headers map[string]string) (sdklog.Exporter, error) {
	var (
		exp sdklog.Exporter
		err error
	)
	switch proto {
	case "http/protobuf", "http":
		exp, err = otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(endpoint),
			otlploghttp.WithHeaders(headers))

	case "grpc":
		// FIXME: bring back when it's actually implemented

		// u, err := url.Parse(endpoint)
		// if err != nil {
		// 	slog.Warn("bad OTLP logs endpoint %q: %w", endpoint, err)
		// 	return
		// }
		//
		opts := []otlploggrpc.Option{
			// 	otlploggrpc.WithEndpointURL(endpoint),
		}
		// if u.Scheme == "unix" {
		// 	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		// 		return net.Dial(u.Scheme, u.Path)
		// 	}
		// 	opts = append(opts,
		// 		otlploggrpc.WithDialOption(grpc.WithContextDialer(dialer)),
		// 		otlploggrpc.WithInsecure())
		// }
		exp, err = otlploggrpc.New(ctx, opts...)

	default:
		return nil, fmt.Errorf("unknown OTLP protocol: %s", proto)
	}
	if err != nil {
		return nil, err
	}
	return exp, nil
}

func (Exporters) MetricExporter(ctx context.Context, proto, endpoint string, headers map[string]string) (sdkmetric.Exporter, error) {
	var (
		exp sdkmetric.Exporter
		err error
	)
	//nolint:dupl
	switch proto {
	case "http/protobuf", "http":
		exp, err = otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(endpoint),
			otlpmetrichttp.WithHeaders(headers))
	case "grpc":
		var u *url.URL
		u, err = url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("bad OTLP metrics endpoint %q: %w", endpoint, err)
		}
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpointURL(endpoint),
		}
		if u.Scheme == "unix" {
			dialer := func(ctx context.Context, addr string) (net.Conn, error) {
				return net.Dial(u.Scheme, u.Path)
			}
			opts = append(opts,
				otlpmetricgrpc.WithDialOption(grpc.WithContextDialer(dialer)),
				otlpmetricgrpc.WithInsecure())
		}
		exp, err = otlpmetricgrpc.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown OTLP protocol: %s", proto)
	}
	if err != nil {
		return nil, err
	}
	return exp, nil
}
