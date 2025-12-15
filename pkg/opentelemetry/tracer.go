package opentelemetry

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
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
	tracer        otelTrace.Tracer
	traceProvider *trace.TracerProvider
}

func NewTracer(serviceName string) (*Tracer, error) {
	return &Tracer{ServiceName: serviceName}, nil
}

func (th *Tracer) Initialize(logger *logrus.Logger) error {
	tracer := otel.Tracer(th.ReturnFilePath(2))
	th.tracer = tracer
	logger.Debug("Initialized OpenTelemetry Tracer for service: ", th.ServiceName)
	return nil
}

func (th *Tracer) Connect(logger *logrus.Logger) error {

	logger.Debug("Connecting to OpenTelemetry Tracer Provider")

	// Get the OTEL endpoint from environment variable
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otelEndpoint == "" {
		logger.Warn("OTEL_EXPORTER_OTLP_ENDPOINT is not set, skipping OpenTelemetry setup")
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT is not set")
	}

	// Configure client options based on endpoint scheme
	clientOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(otelEndpoint),
		otlptracehttp.WithHeaders(map[string]string{
			"content-type": "application/json",
		}),
		otlptracehttp.WithURLPath("/v1/traces"),
	}

	// Only use insecure for http:// endpoints
	if strings.HasPrefix(otelEndpoint, "http://") {
		clientOpts = append(clientOpts, otlptracehttp.WithInsecure())
	}

	// Create the OTLP trace exporter
	exporter, err := otlptrace.New(
		context.Background(),
		otlptracehttp.NewClient(clientOpts...),
	)
	if err != nil {
		logger.Error("Error creating OTLP trace exporter: ", err)
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
	logger.Debug("Connected to OpenTelemetry Tracer Provider at ", otelEndpoint)

	return nil
}

// ContinueWithTrace continues a trace from the given trace ID in the context.
// This is useful for propagating traces across service boundaries.
func (th *Tracer) ContinueWithTrace(ctx context.Context, traceID string) context.Context {
	tid, err := otelTrace.TraceIDFromHex(traceID)
	if err != nil {
		return ctx
	}
	spanContext := otelTrace.NewSpanContext(otelTrace.SpanContextConfig{
		TraceID: tid,
		SpanID:  otelTrace.SpanID{},
		Remote:  true,
	})
	return otelTrace.ContextWithRemoteSpanContext(ctx, spanContext)
}

// CreateSpan creates a new span with the given parameters and returns the updated context and span.
func (th *Tracer) CreateSpan(ctx context.Context, parameters map[string]string) (context.Context, otelTrace.Span) {
	select {
	case <-ctx.Done():
		// Context is already done, return a no-op span
		return ctx, otelTrace.SpanFromContext(ctx)
	default:
	}

	spanName := th.GetCallerFunctionName(2)
	ctx, span := th.tracer.Start(ctx, spanName)
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "default"
	}
	span.SetAttributes(attribute.String("environment", environment))
	span.SetAttributes(attribute.String("url", th.ReturnGitHubEndpoint(2)))

	for key, value := range parameters {
		span.SetAttributes(attribute.String("param."+key, value))
	}
	return ctx, span
}

func (th *Tracer) ReturnGitHubEndpoint(level int) string {
	_, file, line, ok := runtime.Caller(level)
	if !ok {
		return ""
	}
	projectPath := th.ServiceName + "/"
	idx := strings.Index(file, projectPath)
	if idx == -1 {
		// fallback: just return the file name and line
		return fmt.Sprintf("%s#L%d", file, line)
	}
	// Build the GitHub URL format
	relPath := file[idx+len(projectPath):]
	return fmt.Sprintf("github.com/uug-ai/"+th.ServiceName+"/blob/main/%s#L%d", relPath, line)
}

func (th *Tracer) ReturnFilePath(level int) string {
	_, file, line, ok := runtime.Caller(level)
	if !ok {
		return ""
	}
	projectPath := th.ServiceName + "/"
	idx := strings.Index(file, projectPath)
	if idx == -1 {
		// fallback: just return the file name and line
		return fmt.Sprintf("%s#L%d", file, line)
	}
	// Build the GitHub URL format
	relPath := file[idx+len(projectPath):]
	return relPath
}

func (th *Tracer) GetCallerFunctionName(level int) string {
	pc, _, _, ok := runtime.Caller(level)
	if !ok {
		return ""
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return ""
	}

	functionName := fn.Name()

	// You'll have something like this: github.com/uug-ai/$PROJECT/controllers.GetMediaSequences
	// strip of the package path but keep the controller name (controllers.GetMediaSequences)
	// strip everthing before the last /.

	parts := strings.Split(functionName, "/")
	if len(parts) == 0 {
		return ""
	}
	functionName = parts[len(parts)-1]
	return functionName
}

func (th *Tracer) Audit(parameters map[string]string) {
	// This is a no-op function for now, but can be used to log audit information
	// or perform additional actions related to tracing.
	// You can implement your own logic here if needed.
	actionName := th.GetCallerFunctionName(2)
	fmt.Printf("Audit action: %s with parameters: %v\n", actionName, parameters)

	// Write audit to mongodb or any other storage
	//...
	/*db.Collectioms.Audit.InsertOne(context.Background(), map[string]interface{}{
		"action":      actionName,
		"timestamp":   time.Now().Unix(),
	});*/
}
