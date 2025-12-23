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
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	otelTrace "go.opentelemetry.io/otel/trace"
)

type Tracer struct {
	ServiceName   string
	traceProvider *trace.TracerProvider
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
func (th *Tracer) Connect() error {
	// Get the OTEL endpoint from environment variable
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otelEndpoint == "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT is not set")
	}

	// Determine if using insecure HTTP
	isInsecure := strings.HasPrefix(otelEndpoint, "http://")

	// Strip scheme from endpoint - the client handles scheme internally
	endpoint := strings.TrimPrefix(otelEndpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")

	// Configure client options based on endpoint scheme
	clientOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithHeaders(map[string]string{
			"content-type": "application/json",
		}),
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

	// Create the trace provider with the exporter
	traceProvider := trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithResource(
			resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceNameKey.String(th.ServiceName),
				attribute.String("environment", "develop"),
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
