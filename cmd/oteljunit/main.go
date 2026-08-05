// oteljunit reads JUnit XML and emits OTel spans for each test. Usage:
//
//	oteljunit report.xml [more.xml ...]
//	cat report.xml | oteljunit
//
// Each test suite becomes a parent span, with child spans for each test
// case. Pass/fail/skip status and test output are captured.
//
// Requires OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
// to be set.
package main

import (
	"context"
	"fmt"
	"os"

	otel "github.com/dagger/otel-go"
	"github.com/dagger/otel-go/junit"
	otelgo "go.opentelemetry.io/otel"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "oteljunit: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	ctx = otel.InitEmbedded(ctx, nil)
	defer otel.Close()

	tp := otelgo.GetTracerProvider()

	var opts []junit.Option
	if lp := otel.LoggerProvider(ctx); lp != nil {
		opts = append(opts, junit.WithLoggerProvider(lp))
	}

	if len(os.Args) > 1 {
		// Read from file arguments.
		for _, filename := range os.Args[1:] {
			f, err := os.Open(filename)
			if err != nil {
				return err
			}
			if err := junit.Run(ctx, f, tp, opts...); err != nil {
				_ = f.Close()
				return fmt.Errorf("%s: %w", filename, err)
			}
			_ = f.Close()
		}
	} else {
		// Read from stdin.
		if err := junit.Run(ctx, os.Stdin, tp, opts...); err != nil {
			return err
		}
	}

	return nil
}
