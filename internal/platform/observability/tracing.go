package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Tracing holds the tracer provider so main can shut it down cleanly.
type Tracing struct {
	provider *sdktrace.TracerProvider
	Tracer   trace.Tracer
}

// TracingOptions configures the exporter.
type TracingOptions struct {
	ServiceName  string
	ServiceVer   string
	Env          string
	OTLPEndpoint string
	Sampling     float64
}

// NewTracing sets up OTLP/gRPC export. When OTLPEndpoint is empty it installs a
// no-op tracer, so local development needs no collector running and the code
// path is identical either way.
func NewTracing(ctx context.Context, o TracingOptions) (*Tracing, error) {
	if o.OTLPEndpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return &Tracing{Tracer: noop.NewTracerProvider().Tracer(o.ServiceName)}, nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	exporter, err := otlptracegrpc.New(dialCtx,
		otlptracegrpc.WithEndpoint(o.OTLPEndpoint),
		otlptracegrpc.WithInsecure(), // the collector is reached over the mesh, not the internet
	)
	if err != nil {
		return nil, fmt.Errorf("observability: otlp exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(o.ServiceName),
		semconv.ServiceVersion(o.ServiceVer),
		semconv.DeploymentEnvironmentNameKey.String(o.Env),
		attribute.String("platform", "telemed"),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: resource: %w", err)
	}

	sampling := o.Sampling
	if sampling <= 0 {
		sampling = 0.1
	}
	if sampling > 1 {
		sampling = 1
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		// ParentBased keeps a sampled trace intact end to end: if the gateway
		// decided to sample a request, every downstream span is kept too.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampling))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return &Tracing{provider: tp, Tracer: tp.Tracer(o.ServiceName)}, nil
}

// Shutdown flushes pending spans.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t.provider == nil {
		return nil
	}
	return t.provider.Shutdown(ctx)
}
