package gotestmain

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasOTLPEndpoint(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		assert.False(t, hasOTLPEndpoint())
	})
	t.Run("traces endpoint", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		assert.True(t, hasOTLPEndpoint())
	})
	t.Run("general endpoint", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
		assert.True(t, hasOTLPEndpoint())
	})
}

func TestForceTest2JSON(t *testing.T) {
	origArgs := make([]string, len(os.Args))
	copy(origArgs, os.Args)
	defer func() { os.Args = origArgs }()

	t.Run("adds flag when missing", func(t *testing.T) {
		os.Args = []string{"test.bin", "-test.run=TestFoo", "-test.count=1"}
		forceTest2JSON()
		assert.Contains(t, os.Args, "-test.v=test2json")
		assert.Contains(t, os.Args, "-test.run=TestFoo")
		assert.Contains(t, os.Args, "-test.count=1")
	})

	t.Run("replaces existing -test.v", func(t *testing.T) {
		os.Args = []string{"test.bin", "-test.v=true", "-test.count=1"}
		forceTest2JSON()
		assert.Contains(t, os.Args, "-test.v=test2json")
		assert.NotContains(t, os.Args, "-test.v=true")
	})

	t.Run("replaces bare -test.v", func(t *testing.T) {
		os.Args = []string{"test.bin", "-test.v", "-test.count=1"}
		forceTest2JSON()
		assert.Contains(t, os.Args, "-test.v=test2json")
	})
}
