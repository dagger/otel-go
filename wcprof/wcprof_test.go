package wcprof

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	telemetry "github.com/dagger/otel-go"
)

func attrs(kvs []attribute.KeyValue) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

// A walk of two tasks, one depending on the other, each acquiring a unit of
// one resource (one granted and released, one cancelled), reports what the
// engine's decoder of the reporting contract reads.
func TestReportsUnderTheContract(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	p := NewProducer(tp.Tracer("test"), "gobuild-1")
	ctx := context.Background()

	walkCtx, walk := p.TaskGroup(ctx, "walk")
	slots := p.Resource(walkCtx, "gobuild.slots", 16, PriorityFIFO)
	depCtx, dep := p.Task(walkCtx, "package", "example.com/dep")
	a := slots.Acquire(depCtx, 7, map[string]string{"weight": "3"})
	a.Granted()
	a.Release()
	dep.End()
	pkgCtx, pkg := p.Task(walkCtx, "package", "example.com/a")
	t0 := time.Unix(0, 100)
	pkg.DependsOn(dep, t0, t0.Add(50))
	c := slots.Acquire(pkgCtx, 2, nil)
	c.Cancel()
	pkg.End()
	slots.End()
	walk.End()
	p.Complete(ctx)

	spans := sr.Ended()
	if len(spans) != 7 {
		t.Fatalf("%d spans, want 7", len(spans))
	}
	byKind := map[string][]sdktrace.ReadOnlySpan{}
	var carrier sdktrace.ReadOnlySpan
	for _, s := range spans {
		a := attrs(s.Attributes())
		if a[ProducerAttr].AsString() != "gobuild-1" {
			t.Errorf("span %q without the producer ID", s.Name())
		}
		if a[ProducerCompleteAttr].AsBool() {
			carrier = s
			continue
		}
		if a[WorkTypeAttr].AsString() != "engine" {
			t.Errorf("span %q work type %q", s.Name(), a[WorkTypeAttr].AsString())
		}
		if !a[telemetry.UIInternalAttr].AsBool() {
			t.Errorf("span %q isn't marked internal, so it would show in the UI", s.Name())
		}
		k := a[OpKindAttr].AsString()
		byKind[k] = append(byKind[k], s)
	}
	if len(byKind["task_group"]) != 1 || len(byKind["task"]) != 2 || len(byKind["resource"]) != 1 || len(byKind["acquire"]) != 2 {
		t.Fatalf("kinds %v", byKind)
	}

	// The carrier counts the producer's other spans.
	if carrier == nil || attrs(carrier.Attributes())[ProducerSpanCountAttr].AsString() != "6" {
		t.Errorf("completeness carrier %v", carrier)
	}

	// The resource: its declaration, a child of the walk.
	res := byKind["resource"][0]
	ra := attrs(res.Attributes())
	if ra[ResourceNameAttr].AsString() != "gobuild.slots" || ra[ResourceCapacityAttr].AsString() != "16" || ra[ResourcePolicyAttr].AsString() != "priority_fifo" ||
		res.Parent().SpanID() != byKind["task_group"][0].SpanContext().SpanID() {
		t.Errorf("resource %v, parent %v", ra, res.Parent())
	}

	// Tasks carry their labels and live under the walk; the dependent one
	// has a wait link to its dependency.
	var tasks = map[string]sdktrace.ReadOnlySpan{}
	for _, s := range byKind["task"] {
		tasks[attrs(s.Attributes())[LabelAttr].AsString()] = s
		if s.Parent().SpanID() != byKind["task_group"][0].SpanContext().SpanID() {
			t.Errorf("task %v not under the walk", s.Name())
		}
	}
	links := tasks["example.com/a"].Links()
	if len(links) != 1 || links[0].SpanContext.SpanID() != tasks["example.com/dep"].SpanContext().SpanID() {
		t.Fatalf("dependency links %v", links)
	}
	la := attrs(links[0].Attributes)
	if la[telemetry.LinkPurposeAttr].AsString() != "wait" || la[WaitReasonAttr].AsString() != "dependency" ||
		la[WaitStartUnixNanoAttr].AsString() != "100" || la[WaitEndUnixNanoAttr].AsString() != "150" {
		t.Errorf("dependency link attributes %v", la)
	}

	// Acquisitions: children of their holders, naming the resource's span.
	for _, s := range byKind["acquire"] {
		aa := attrs(s.Attributes())
		if aa[ResourceIDAttr].AsString() != res.SpanContext().SpanID().String() {
			t.Errorf("acquisition names resource %q, want %q", aa[ResourceIDAttr].AsString(), res.SpanContext().SpanID())
		}
		switch aa[ResourceOutcomeAttr].AsString() {
		case "released":
			if s.Parent().SpanID() != tasks["example.com/dep"].SpanContext().SpanID() || aa[ResourcePriorityAttr].AsString() != "7" ||
				aa[ResourcePriorityInputsAttr].AsString() != `{"weight":"3"}` {
				t.Errorf("released acquisition %v", aa)
			}
			g, err := strconv.ParseInt(aa[ResourceGrantedUnixNanoAttr].AsString(), 10, 64)
			if err != nil || g < s.StartTime().UnixNano() || g > s.EndTime().UnixNano() {
				t.Errorf("grant %q outside the acquisition [%d, %d]", aa[ResourceGrantedUnixNanoAttr].AsString(), s.StartTime().UnixNano(), s.EndTime().UnixNano())
			}
		case "cancelled":
			if s.Parent().SpanID() != tasks["example.com/a"].SpanContext().SpanID() || aa[ResourceGrantedUnixNanoAttr].Type() != attribute.INVALID {
				t.Errorf("cancelled acquisition %v", aa)
			}
		default:
			t.Errorf("acquisition outcome %v", aa[ResourceOutcomeAttr])
		}
	}
}
