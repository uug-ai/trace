package opentelemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	otelTrace "go.opentelemetry.io/otel/trace"
)

type Tracer struct {
	ServiceName   string
	traceProvider *trace.TracerProvider
}

type TraceContextCarrier struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
}

// MarshalWithTraceContext marshals value as a JSON object and adds the optional
// W3C carrier fields without requiring them on the message's domain model.
func MarshalWithTraceContext(value any, carrier TraceContextCarrier) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, fmt.Errorf("trace context requires a JSON object: %w", err)
	}
	if carrier.TraceParent != "" {
		object["traceparent"], _ = json.Marshal(carrier.TraceParent)
	}
	if carrier.TraceState != "" {
		object["tracestate"], _ = json.Marshal(carrier.TraceState)
	}
	return json.Marshal(object)
}

// UnmarshalWithTraceContext decodes a JSON message and returns any top-level
// W3C carrier fields alongside it.
func UnmarshalWithTraceContext(payload []byte, value any) (TraceContextCarrier, error) {
	if err := json.Unmarshal(payload, value); err != nil {
		return TraceContextCarrier{}, err
	}
	var carrier TraceContextCarrier
	if err := json.Unmarshal(payload, &carrier); err != nil {
		return TraceContextCarrier{}, err
	}
	return carrier, nil
}

func NewTracer(serviceName string) (*Tracer, error) {
	return &Tracer{ServiceName: serviceName}, nil
}

// Shutdown gracefully shuts down the tracer provider, ensuring all spans are exported.
func (th *Tracer) Shutdown(ctx context.Context) error {
	if th.traceProvider != nil {
		return th.traceProvider.Shutdown(ctx)
	}
	return nil
}

// Connect sets up the OpenTelemetry tracer provider with an OTLP exporter.
// It reads the OTEL_EXPORTER_OTLP_ENDPOINT environment variable to determine
// where to send the trace data.
//
// Tracing is an optional, best-effort observability concern: when
// OTEL_EXPORTER_OTLP_ENDPOINT is not set the tracer stays in no-op mode and
// Connect returns nil instead of an error. No global tracer provider is
// installed, so the OpenTelemetry SDK's default no-op provider is used and
// CreateSpan transparently produces no-op spans. This keeps a missing (or
// intentionally disabled, e.g. Helm opentelemetry.enabled=false) collector from
// taking down the calling service — callers that treat a Connect error as fatal
// must not be crashed just because tracing is off.
func (th *Tracer) Connect() error {
	// Get the OTEL endpoint from environment variable
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otelEndpoint == "" {
		// No endpoint configured: run without tracing (no-op) rather than
		// failing. The global provider is left untouched (default no-op).
		return nil
	}

	// Determine if using insecure HTTP
	isInsecure := strings.HasPrefix(otelEndpoint, "http://")

	// Strip scheme from endpoint - the client handles scheme internally
	endpoint := strings.TrimPrefix(otelEndpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")

	// Configure client options based on endpoint scheme. The OTLP HTTP
	// exporter encodes spans as protobuf, so we let it set its own
	// Content-Type (application/x-protobuf) rather than mislabelling the body.
	clientOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithURLPath("/v1/traces"),
	}

	// Only use insecure for http:// endpoints
	if isInsecure {
		clientOpts = append(clientOpts, otlptracehttp.WithInsecure())
	}

	// Create the OTLP trace exporter
	exporter, err := otlptrace.New(
		context.Background(),
		otlptracehttp.NewClient(clientOpts...),
	)
	if err != nil {
		return fmt.Errorf("creating new exporter: %w", err)
	}

	// Resolve the deployment environment from the ENVIRONMENT variable so the
	// resource reflects reality (prod/staging/...) instead of a hardcoded value.
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "default"
	}

	// Create the trace provider with the exporter
	traceProvider := trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithResource(
			resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceNameKey.String(th.ServiceName),
				attribute.String("environment", environment),
			),
		),
	)

	// Set the global tracer provider
	otel.SetTracerProvider(traceProvider)
	th.traceProvider = traceProvider

	return nil
}

// ContinueWithTrace continues a trace from the given trace ID in the context.
// This is useful for propagating traces across service boundaries.
// Returns an error if the trace ID is invalid.
func (th *Tracer) ContinueWithTrace(ctx context.Context, traceID string) (context.Context, error) {
	tid, err := otelTrace.TraceIDFromHex(traceID)
	if err != nil {
		return ctx, fmt.Errorf("invalid trace ID: %w", err)
	}
	spanContext := otelTrace.NewSpanContext(otelTrace.SpanContextConfig{
		TraceID: tid,
		SpanID:  otelTrace.SpanID{},
		Remote:  true,
	})
	return otelTrace.ContextWithRemoteSpanContext(ctx, spanContext), nil
}

// InjectTraceContext serializes the current W3C parent context for transport
// across a queue or other process boundary.
func (th *Tracer) InjectTraceContext(ctx context.Context) TraceContextCarrier {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return TraceContextCarrier{
		TraceParent: carrier.Get("traceparent"),
		TraceState:  carrier.Get("tracestate"),
	}
}

// ContinueWithTraceContext restores a W3C parent context when it is valid and
// agrees with traceID. Invalid or mismatched carriers fall back to the legacy
// trace-ID-only context and return an error so callers can log the degradation.
func (th *Tracer) ContinueWithTraceContext(ctx context.Context, traceID string, carrier TraceContextCarrier) (context.Context, error) {
	if carrier.TraceParent == "" && carrier.TraceState == "" {
		return th.ContinueWithTrace(ctx, traceID)
	}

	values := propagation.MapCarrier{}
	values.Set("traceparent", carrier.TraceParent)
	values.Set("tracestate", carrier.TraceState)
	extracted := propagation.TraceContext{}.Extract(ctx, values)
	spanContext := otelTrace.SpanContextFromContext(extracted)
	if spanContext.IsValid() && spanContext.TraceID().String() == traceID {
		return extracted, nil
	}

	fallback, err := th.ContinueWithTrace(ctx, traceID)
	if err != nil {
		return ctx, errors.Join(errors.New("invalid or mismatched trace context carrier"), err)
	}
	return fallback, errors.New("invalid or mismatched trace context carrier")
}

// CreateSpan creates a new span with the given parameters and returns the updated context and span.
func (th *Tracer) CreateSpan(ctx context.Context, parameters map[string]string) (context.Context, otelTrace.Span) {
	select {
	case <-ctx.Done():
		// Context is already done, return a no-op span
		return ctx, otelTrace.SpanFromContext(ctx)
	default:
	}

	var tracer otelTrace.Tracer
	filePath, err := th.ReturnFilePath(2)
	if err == nil {
		tracer = otel.Tracer(filePath)
	}

	spanName, _ := th.GetCallerFunctionName(2)
	ctx, span := tracer.Start(ctx, spanName)
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "default"
	}
	span.SetAttributes(attribute.String("environment", environment))
	if githubURL, err := th.ReturnGitHubEndpoint(2); err == nil {
		span.SetAttributes(attribute.String("url", githubURL))
	}

	for key, value := range parameters {
		span.SetAttributes(attribute.String("param."+key, value))
	}
	return ctx, span
}

func (th *Tracer) ReturnGitHubEndpoint(level int) (string, error) {
	_, file, line, ok := runtime.Caller(level)
	if !ok {
		return "", errors.New("could not retrieve caller information")
	}
	projectPath := th.ServiceName + "/"
	idx := strings.Index(file, projectPath)
	if idx == -1 {
		// fallback: just return the file name and line
		return fmt.Sprintf("%s#L%d", file, line), nil
	}
	// Build the GitHub URL format
	relPath := file[idx+len(projectPath):]
	return fmt.Sprintf("github.com/uug-ai/"+th.ServiceName+"/blob/main/%s#L%d", relPath, line), nil
}

func (th *Tracer) ReturnFilePath(level int) (string, error) {
	_, file, line, ok := runtime.Caller(level)
	if !ok {
		return "", errors.New("could not retrieve caller information")
	}
	projectPath := th.ServiceName + "/"
	idx := strings.Index(file, projectPath)
	if idx == -1 {
		// fallback: just return the file name and line
		return fmt.Sprintf("%s#L%d", file, line), nil
	}
	// Build the GitHub URL format
	relPath := file[idx+len(projectPath):]
	return relPath, nil
}

func (th *Tracer) GetCallerFunctionName(level int) (string, error) {
	pc, _, _, ok := runtime.Caller(level)
	if !ok {
		return "", errors.New("could not retrieve caller information")
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "", errors.New("could not retrieve function information")
	}

	functionName := fn.Name()

	// You'll have something like this: github.com/uug-ai/$PROJECT/controllers.GetMediaSequences
	// strip of the package path but keep the controller name (controllers.GetMediaSequences)
	// strip everthing before the last /.

	parts := strings.Split(functionName, "/")
	if len(parts) == 0 {
		return "", errors.New("function name split resulted in empty parts")
	}
	functionName = parts[len(parts)-1]
	return functionName, nil
}

func (th *Tracer) Audit(parameters map[string]string) {
	// This is a no-op function for now, but can be used to log audit information
	// or perform additional actions related to tracing.
	// You can implement your own logic here if needed.
	actionName, _ := th.GetCallerFunctionName(2)
	fmt.Printf("Audit action: %s with parameters: %v\n", actionName, parameters)

	// Write audit to mongodb or any other storage
	//...
	/*db.Collectioms.Audit.InsertOne(context.Background(), map[string]interface{}{
		"action":      actionName,
		"timestamp":   time.Now().Unix(),
	});*/
}
