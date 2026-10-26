// Package adopt is a test binary that adopts otelgotest's span for each test
// through oteltestctx, as Dagger's integration tests do.
package adopt

import (
	"context"
	"os"
	"testing"

	"github.com/dagger/otel-go/oteltestctx"
	"github.com/dagger/testctx"
	"go.opentelemetry.io/otel"
)

func TestMain(m *testing.M) {
	os.Exit(oteltestctx.Main(m))
}

func TestAdopt(t *testing.T) {
	testctx.New(t, oteltestctx.WithTracing[*testing.T]()).Run("inner", func(ctx context.Context, t *testctx.T) {
		_, span := otel.Tracer("adopt").Start(ctx, "downstream-call")
		span.End()
	})
}
