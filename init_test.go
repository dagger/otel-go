package otel

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/resource"
)

// TestFallbackResourceMergesWithDefault guards against the fallback
// resource's semconv SchemaURL drifting from the one the OTel SDK uses for
// resource.Default, which makes resource.Merge fail with a conflicting
// Schema URL error.
func TestFallbackResourceMergesWithDefault(t *testing.T) {
	fallback := fallbackResource()

	merged, err := resource.Merge(resource.Default(), fallback)
	require.NoError(t, err)
	require.Equal(t, resource.Default().SchemaURL(), fallback.SchemaURL())
	require.Equal(t, fallback.SchemaURL(), merged.SchemaURL())

	name, ok := merged.Set().Value("service.name")
	require.True(t, ok)
	require.Equal(t, "dagger", name.AsString())
}
