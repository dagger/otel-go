// Package wcprof reports a program's own units of work to Dagger's
// wall-clock profiler, wcprof, so a profile of a Dagger run can show them
// next to the engine's work: which tasks the program ran, which tasks each
// waited for, and its waits for its own limited resources, such as a build
// tool's slots for concurrent compiles.
//
// It writes ordinary OTel spans and links under wcprof's reporting
// contract. A program inside a container exec or a Dagger module reports to
// the engine through the OTel exporter it already has, so the spans need no
// other setup. Each dependency is a link on the dependent task's span, so a
// tracer provider set up some other way than otel-go's Init needs
// otel.SpanLimits' link limit, or a task's dependencies beyond the SDK's
// default of 128 are dropped. When the program is done, it calls Producer.Complete, and
// then flushes its exporter as usual, so a reader can tell a complete report
// from a partial one.
package wcprof

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	telemetry "github.com/dagger/otel-go"
)

// The reporting contract's attributes, as the engine reads them.
const (
	OpKindAttr   = "wcprof.op.kind"
	WorkTypeAttr = "wcprof.work_type"
	LabelAttr    = "wcprof.label"
	ProducerAttr = "wcprof.producer"

	WaitReasonAttr        = "wcprof.wait.reason"
	WaitStartUnixNanoAttr = "wcprof.wait.start_unix_ns"
	WaitEndUnixNanoAttr   = "wcprof.wait.end_unix_ns"

	ResourceNameAttr            = "wcprof.resource.name"
	ResourceCapacityAttr        = "wcprof.resource.capacity"
	ResourcePolicyAttr          = "wcprof.resource.policy"
	ResourceIDAttr              = "wcprof.resource.id"
	ResourcePriorityAttr        = "wcprof.resource.priority"
	ResourcePriorityInputsAttr  = "wcprof.resource.priority_inputs"
	ResourceGrantedUnixNanoAttr = "wcprof.resource.granted_unix_ns"
	ResourceOutcomeAttr         = "wcprof.resource.outcome"

	ProducerCompleteAttr  = "wcprof.producer.complete"
	ProducerSpanCountAttr = "wcprof.producer.span_count"
)

// Op kinds of the reporting contract.
const (
	kindTask      = "task"
	kindTaskGroup = "task_group"
	kindResource  = "resource"
	kindAcquire   = "acquire"
)

// Policy is how a Resource admits waiting requests.
type Policy string

const (
	// PriorityFIFO admits the highest priority first, the earliest request
	// first among equals.
	PriorityFIFO Policy = "priority_fifo"
	// FIFO admits the earliest request first.
	FIFO Policy = "fifo"
)

// Producer reports one program's work. Every span it starts carries its ID,
// and Complete declares how many there were.
type Producer struct {
	tracer trace.Tracer
	id     string
	spans  atomic.Int64
}

// NewProducer returns a producer that starts its spans with tracer. id
// identifies this producer among the session's, e.g. the program's name and
// its process ID.
func NewProducer(tracer trace.Tracer, id string) *Producer {
	return &Producer{tracer: tracer, id: id}
}

func (p *Producer) start(ctx context.Context, name, kind string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	p.spans.Add(1)
	return p.tracer.Start(ctx, name,
		telemetry.Internal(),
		trace.WithAttributes(
			attribute.String(OpKindAttr, kind),
			attribute.String(WorkTypeAttr, "engine"),
			attribute.String(ProducerAttr, p.id),
		),
		trace.WithAttributes(attrs...),
	)
}

// Op is a reported unit of work: a task or a task group. End it when the
// work is done.
type Op struct {
	span trace.Span
}

// TaskGroup starts a group whose tasks run concurrently, ordered only by
// their dependencies and their waits for resources, e.g. a build's walk
// over its packages. Start its tasks with the returned context.
func (p *Producer) TaskGroup(ctx context.Context, name string) (context.Context, *Op) {
	ctx, span := p.start(ctx, name, kindTaskGroup)
	return ctx, &Op{span: span}
}

// Task starts one unit of the program's work, labelled for comparisons
// (e.g. a Go import path). Start it when the program creates the work, before
// it waits for the work's dependencies, so those waits fall inside it.
// Engine requests made with the returned context are attributed to it.
func (p *Producer) Task(ctx context.Context, name, label string) (context.Context, *Op) {
	ctx, span := p.start(ctx, name, kindTask, attribute.String(LabelAttr, label))
	return ctx, &Op{span: span}
}

// DependsOn records that o needed dep's result: o waited for it from start to
// end. For a dependency that was already done when o needed it, pass the
// same time twice: the dependency is recorded either way.
func (o *Op) DependsOn(dep *Op, start, end time.Time) {
	o.span.AddLink(trace.Link{
		SpanContext: dep.span.SpanContext(),
		Attributes: []attribute.KeyValue{
			attribute.String(telemetry.LinkPurposeAttr, telemetry.LinkPurposeWait),
			attribute.String(WaitReasonAttr, "dependency"),
			attribute.String(WaitStartUnixNanoAttr, strconv.FormatInt(start.UnixNano(), 10)),
			attribute.String(WaitEndUnixNanoAttr, strconv.FormatInt(end.UnixNano(), 10)),
		},
	})
}

// End ends the op.
func (o *Op) End() { o.span.End() }

// Resource is a limited resource of the program, such as its slots for
// concurrent work. End it when the resource is no longer used.
type Resource struct {
	p    *Producer
	span trace.Span
	id   string
	name string
}

// Resource declares a resource instance named name (instances may share a
// name; each is its own) with capacity units, admitted by policy. Declare it
// under the op that owns it.
func (p *Producer) Resource(ctx context.Context, name string, capacity int, policy Policy) *Resource {
	_, span := p.start(ctx, name, kindResource,
		attribute.String(ResourceNameAttr, name),
		attribute.String(ResourceCapacityAttr, strconv.Itoa(capacity)),
		attribute.String(ResourcePolicyAttr, string(policy)),
	)
	return &Resource{p: p, span: span, id: span.SpanContext().SpanID().String(), name: name}
}

// End ends the resource's life.
func (r *Resource) End() { r.span.End() }

// Acquisition is one request for one unit of a Resource, from the request
// to the release.
type Acquisition struct {
	span trace.Span
}

// Acquire records a request for one unit of r, made now by the task whose
// context ctx is, with priority (larger first). inputs are what the
// priority was computed from, if the program wants a profile to be able to
// recompute it another way (e.g. {"weight": "20298"}); nil for none. Call
// Granted when the unit is granted, then Release, or Cancel if the request
// is given up before a grant.
func (r *Resource) Acquire(ctx context.Context, priority int64, inputs map[string]string) *Acquisition {
	attrs := []attribute.KeyValue{
		attribute.String(ResourceIDAttr, r.id),
		attribute.String(ResourcePriorityAttr, strconv.FormatInt(priority, 10)),
	}
	if len(inputs) > 0 {
		b, err := json.Marshal(inputs)
		if err == nil {
			attrs = append(attrs, attribute.String(ResourcePriorityInputsAttr, string(b)))
		}
	}
	_, span := r.p.start(ctx, "acquire "+r.name, kindAcquire, attrs...)
	return &Acquisition{span: span}
}

// Granted records that the unit was granted now.
func (a *Acquisition) Granted() {
	a.span.SetAttributes(attribute.String(ResourceGrantedUnixNanoAttr, strconv.FormatInt(time.Now().UnixNano(), 10)))
}

// Release records that the unit was released now. It ends the acquisition.
func (a *Acquisition) Release() { a.end("released") }

// Cancel records that the request was given up now, before a grant. It ends
// the acquisition.
func (a *Acquisition) Cancel() { a.end("cancelled") }

func (a *Acquisition) end(outcome string) {
	a.span.SetAttributes(attribute.String(ResourceOutcomeAttr, outcome))
	a.span.End()
}

// Complete declares how many spans the producer reported. Call it once,
// after every op, resource and acquisition has ended, then flush the
// program's exporter.
func (p *Producer) Complete(ctx context.Context) {
	_, span := p.tracer.Start(ctx, "wcprof producer complete",
		telemetry.Internal(),
		trace.WithAttributes(
			attribute.String(ProducerAttr, p.id),
			attribute.Bool(ProducerCompleteAttr, true),
			attribute.String(ProducerSpanCountAttr, strconv.FormatInt(p.spans.Load(), 10)),
		),
	)
	span.End()
}
