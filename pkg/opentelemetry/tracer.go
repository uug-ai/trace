package opentelemetry

import (
	"context"
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
	sampler       string
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

	// A per-class sampling configuration (TRACE_SAMPLER_RATIO_<CLASS>) is checked
	// before anything is installed, so a malformed value leaves tracing off and
	// is reported instead of sampling at an unintended rate.
	classSampler, samplerDescription, err := classSamplerFromEnv()
	if err != nil {
		return err
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
	providerOptions := []trace.TracerProviderOption{
		trace.WithBatcher(exporter),
		trace.WithResource(th.newResource(context.Background(), environment)),
	}
	// Without a class configuration the SDK reads OTEL_TRACES_SAMPLER itself.
	if classSampler != nil {
		providerOptions = append(providerOptions, trace.WithSampler(classSampler))
	} else {
		samplerDescription = envSamplerDescription()
	}
	traceProvider := trace.NewTracerProvider(providerOptions...)

	// Set the global tracer provider
	otel.SetTracerProvider(traceProvider)
	th.traceProvider = traceProvider
	th.sampler = samplerDescription

	return nil
}

// newResource describes this service to the tracing backend. The service name
// passed to NewTracer and the ENVIRONMENT variable are the defaults; the
// standard OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES variables override
// them, so a deployment can add attributes such as service.version without a
// code change. The SDK's own telemetry.sdk.* attributes are included. A
// malformed OTEL_RESOURCE_ATTRIBUTES is reported to the OpenTelemetry error
// handler and the rest of the resource is kept, as it does not change what is
// traced.
func (th *Tracer) newResource(ctx context.Context, environment string) *resource.Resource {
	attributes := []attribute.KeyValue{
		semconv.ServiceNameKey.String(th.ServiceName),
		// Kept for existing dashboards; deployment.environment.name is the
		// semantic-convention attribute.
		attribute.String("environment", environment),
	}
	if configured := os.Getenv("ENVIRONMENT"); configured != "" {
		attributes = append(attributes, attribute.String("deployment.environment.name", configured))
	}
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attributes...),
		resource.WithFromEnv(),
	)
	if err != nil {
		otel.Handle(err)
	}
	if res == nil {
		return resource.NewSchemaless(attributes...)
	}
	return res
}

// Sampler describes the sampler Connect installed, e.g.
// "ParentBased(ClassRatioSampler{other=0,upload=0.1})", for the service to log
// at startup: it shows the ratios in effect, and a misspelt
// TRACE_SAMPLER_RATIO_<CLASS> variable as a class no span uses. It is empty
// while tracing is off.
func (th *Tracer) Sampler() string {
	return th.sampler
}

// envSamplerDescription describes the sampler the SDK reads from
// OTEL_TRACES_SAMPLER when no class ratio is set.
func envSamplerDescription() string {
	name := os.Getenv("OTEL_TRACES_SAMPLER")
	if name == "" {
		return "OTEL_TRACES_SAMPLER unset (parentbased_always_on)"
	}
	description := "OTEL_TRACES_SAMPLER=" + name
	if arg := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); arg != "" {
		description += " OTEL_TRACES_SAMPLER_ARG=" + arg
	}
	return description
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

// W3C trace-context keys carried inside a message's data map (for example
// models.PipelineEvent.Data), next to the message's legacy trace ID.
const (
	TraceParentKey = "traceparent"
	TraceStateKey  = "tracestate"
)

var errInvalidTraceData = errors.New("invalid or mismatched traceparent in message data")

// ContinueWithTraceData continues traceID with the W3C traceparent/tracestate
// found in data as the remote parent. Unlike ContinueWithTrace, this keeps the
// sender's span ID and sampling decision: the next span becomes a child of the
// sender's span, and a parent-based sampler follows the upstream decision
// instead of sampling the message as a new root.
//
// The traceparent is only trusted when its trace ID matches traceID (any valid
// traceparent is accepted when traceID is empty). When data carries no
// traceparent, this behaves exactly like ContinueWithTrace. A malformed or
// mismatched traceparent falls back to ContinueWithTrace and returns an error
// so callers can log the degradation; the returned context is still usable.
func (th *Tracer) ContinueWithTraceData(ctx context.Context, traceID string, data map[string]any) (context.Context, error) {
	carrier := propagation.MapCarrier{}
	for _, key := range []string{TraceParentKey, TraceStateKey} {
		if value, ok := data[key].(string); ok && value != "" {
			carrier.Set(key, value)
		}
	}
	if carrier.Get(TraceParentKey) == "" {
		return th.ContinueWithTrace(ctx, traceID)
	}

	extracted := propagation.TraceContext{}.Extract(ctx, carrier)
	spanContext := otelTrace.SpanContextFromContext(extracted)
	if spanContext.IsValid() && (traceID == "" || strings.EqualFold(spanContext.TraceID().String(), traceID)) {
		return extracted, nil
	}

	fallback, err := th.ContinueWithTrace(ctx, traceID)
	if err != nil {
		return ctx, errors.Join(errInvalidTraceData, err)
	}
	return fallback, errInvalidTraceData
}

// InjectTraceData writes the span context in ctx into data as traceparent (and
// tracestate when present), so the consumer of the message continues from this
// span. Call it with the context of the span that handled the message, right
// before the message is forwarded or published. It allocates data when nil and
// returns it. When ctx holds no valid span context (for example tracing is
// disabled), data is returned unchanged so an upstream traceparent passes
// through untouched.
func (th *Tracer) InjectTraceData(ctx context.Context, data map[string]any) map[string]any {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	traceParent := carrier.Get(TraceParentKey)
	if traceParent == "" {
		return data
	}

	if data == nil {
		data = make(map[string]any, 2)
	}
	data[TraceParentKey] = traceParent
	if traceState := carrier.Get(TraceStateKey); traceState != "" {
		data[TraceStateKey] = traceState
	} else {
		delete(data, TraceStateKey)
	}
	return data
}

// TraceContextCarrier is the W3C trace context of one queue hop, for messages
// that carry it as typed fields rather than in a data map (for example
// models.WorkflowRun.TraceParent / TraceState).
type TraceContextCarrier struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
}

// ContinueWithTraceContext is ContinueWithTraceData for a typed carrier: it
// continues traceID with the carrier's traceparent as the remote parent, keeping
// the sender's span and sampling decision, and falls back to ContinueWithTrace
// (with an error for a malformed or mismatched traceparent) like
// ContinueWithTraceData does.
func (th *Tracer) ContinueWithTraceContext(ctx context.Context, traceID string, carrier TraceContextCarrier) (context.Context, error) {
	return th.ContinueWithTraceData(ctx, traceID, map[string]any{
		TraceParentKey: carrier.TraceParent,
		TraceStateKey:  carrier.TraceState,
	})
}

// InjectTraceContext returns the span context in ctx as a typed carrier, so the
// consumer of the message continues from this span. Call it with the context of
// the span that handled the message, right before publishing. It is empty when
// ctx holds no valid span context.
func (th *Tracer) InjectTraceContext(ctx context.Context) TraceContextCarrier {
	data := th.InjectTraceData(ctx, nil)
	traceParent, _ := data[TraceParentKey].(string)
	traceState, _ := data[TraceStateKey].(string)
	return TraceContextCarrier{TraceParent: traceParent, TraceState: traceState}
}

// CreateSpan creates a new span with the given parameters and returns the updated context and span.
func (th *Tracer) CreateSpan(ctx context.Context, parameters map[string]string) (context.Context, otelTrace.Span) {
	return th.createSpan(ctx, parameters)
}

// CreateClassSpan is CreateSpan for the root of one kind of work, tagged with
// its sampling class (SamplingClassKey) when the span starts, so
// TRACE_SAMPLER_RATIO_<CLASS> can trace that work at its own rate. A span that
// continues a parent follows the parent's decision whatever its class.
func (th *Tracer) CreateClassSpan(ctx context.Context, class string, parameters map[string]string) (context.Context, otelTrace.Span) {
	return th.createSpan(ctx, parameters, otelTrace.WithAttributes(SamplingClassKey.String(class)))
}

// createSpan is called by the exported span constructors only: the span name
// and source URL are taken from their caller, two frames up.
func (th *Tracer) createSpan(ctx context.Context, parameters map[string]string, options ...otelTrace.SpanStartOption) (context.Context, otelTrace.Span) {
	select {
	case <-ctx.Done():
		// Context is already done, return a no-op span
		return ctx, otelTrace.SpanFromContext(ctx)
	default:
	}

	pc, file, line, callerFound := runtime.Caller(2)
	tracer := otel.Tracer(th.ServiceName)
	spanName := ""
	if callerFound {
		tracer = otel.Tracer(th.filePath(file, line))
		spanName, _ = functionName(pc)
	}
	ctx, span := tracer.Start(ctx, spanName, options...)
	// A span the sampler dropped records nothing, so its attributes are not built.
	if !span.IsRecording() {
		return ctx, span
	}

	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "default"
	}
	span.SetAttributes(attribute.String("environment", environment))
	if callerFound {
		span.SetAttributes(attribute.String("url", th.gitHubEndpoint(file, line)))
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
	return th.gitHubEndpoint(file, line), nil
}

func (th *Tracer) ReturnFilePath(level int) (string, error) {
	_, file, line, ok := runtime.Caller(level)
	if !ok {
		return "", errors.New("could not retrieve caller information")
	}
	return th.filePath(file, line), nil
}

// relativePath returns file relative to the service's directory, or false when
// file is outside it.
func (th *Tracer) relativePath(file string) (string, bool) {
	projectPath := th.ServiceName + "/"
	idx := strings.Index(file, projectPath)
	if idx == -1 {
		return "", false
	}
	return file[idx+len(projectPath):], true
}

func (th *Tracer) gitHubEndpoint(file string, line int) string {
	relPath, ok := th.relativePath(file)
	if !ok {
		// fallback: just return the file name and line
		return fmt.Sprintf("%s#L%d", file, line)
	}
	return fmt.Sprintf("github.com/uug-ai/%s/blob/main/%s#L%d", th.ServiceName, relPath, line)
}

func (th *Tracer) filePath(file string, line int) string {
	relPath, ok := th.relativePath(file)
	if !ok {
		// fallback: just return the file name and line
		return fmt.Sprintf("%s#L%d", file, line)
	}
	return relPath
}

func (th *Tracer) GetCallerFunctionName(level int) (string, error) {
	pc, _, _, ok := runtime.Caller(level)
	if !ok {
		return "", errors.New("could not retrieve caller information")
	}
	return functionName(pc)
}

// functionName returns the name of the function at pc without its package
// path, e.g. controllers.GetMediaSequences.
func functionName(pc uintptr) (string, error) {
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "", errors.New("could not retrieve function information")
	}

	name := fn.Name()

	// You'll have something like this: github.com/uug-ai/$PROJECT/controllers.GetMediaSequences
	// strip of the package path but keep the controller name (controllers.GetMediaSequences)
	// strip everthing before the last /.

	parts := strings.Split(name, "/")
	if len(parts) == 0 {
		return "", errors.New("function name split resulted in empty parts")
	}
	return parts[len(parts)-1], nil
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
