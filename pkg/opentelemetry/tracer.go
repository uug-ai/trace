package utils

import (
	"context"
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

func Connect(serviceName string) (*trace.TracerProvider, error) {
	headers := map[string]string{
		"content-type": "application/json",
	}
	var traceProvider *trace.TracerProvider
	otelEndpoint := ""
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		otelEndpoint = endpoint
		// Configure client options based on endpoint scheme
		clientOpts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(otelEndpoint),
			otlptracehttp.WithHeaders(headers),
			otlptracehttp.WithURLPath("/v1/traces"),
		}
		// Only use insecure for http:// endpoints
		if strings.HasPrefix(endpoint, "http://") {
			clientOpts = append(clientOpts, otlptracehttp.WithInsecure())
		}
		exporter, err := otlptrace.New(
			context.Background(),
			otlptracehttp.NewClient(clientOpts...),
		)
		if err != nil {
			return nil, fmt.Errorf("creating new exporter: %w", err)
		}
		tracerprovider := trace.NewTracerProvider(
			trace.WithBatcher(exporter),
			trace.WithResource(
				resource.NewWithAttributes(
					semconv.SchemaURL,
					semconv.ServiceNameKey.String(serviceName),
					attribute.String("environment", "develop"),
				),
			),
		)

		otel.SetTracerProvider(tracerprovider)
		traceProvider = tracerprovider
		return traceProvider, nil
	}
	return traceProvider, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT is not set")
}

type Tracer struct {
	tracer otelTrace.Tracer
}

func NewTracer() (*Tracer, error) {
	tracer := otel.Tracer(returnFilePath(2))
	return &Tracer{tracer: tracer}, nil
}

func (th *Tracer) CreateSpanContext(ctx context.Context, traceID string) context.Context {
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

func (th *Tracer) CreateSpan(ctx context.Context, parameters map[string]string) (context.Context, otelTrace.Span) {
	select {
	case <-ctx.Done():
		// Context is already done, return a no-op span
		return ctx, otelTrace.SpanFromContext(ctx)
	default:
	}

	spanName := GetCallerFunctionName(2)
	ctx, span := th.tracer.Start(ctx, spanName)
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "default"
	}
	span.SetAttributes(attribute.String("environment", environment))
	span.SetAttributes(attribute.String("url", returnGitHubEndpoint(2)))
	for key, value := range parameters {
		span.SetAttributes(attribute.String("param."+key, value))
	}
	return ctx, span
}

func returnGitHubEndpoint(level int) string {
	_, file, line, ok := runtime.Caller(level)
	if !ok {
		return ""
	}
	projectPath := ServiceName + "/"
	idx := strings.Index(file, projectPath)
	if idx == -1 {
		// fallback: just return the file name and line
		return fmt.Sprintf("%s#L%d", file, line)
	}
	// Build the GitHub URL format
	relPath := file[idx+len(projectPath):]
	return fmt.Sprintf("github.com/uug-ai/"+ServiceName+"/blob/main/%s#L%d", relPath, line)
}

func returnFilePath(level int) string {
	_, file, line, ok := runtime.Caller(level)
	if !ok {
		return ""
	}
	projectPath := ServiceName + "/"
	idx := strings.Index(file, projectPath)
	if idx == -1 {
		// fallback: just return the file name and line
		return fmt.Sprintf("%s#L%d", file, line)
	}
	// Build the GitHub URL format
	relPath := file[idx+len(projectPath):]
	return relPath
}

func GetCallerFunctionName(level int) string {
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

/*func (th *Tracer) Audit(parameters map[string]string) {
	// This is a no-op function for now, but can be used to log audit information
	// or perform additional actions related to tracing.
	// You can implement your own logic here if needed.
	actionName := GetCallerFunctionName(2)

	// Write audit to mongodb or any other storage
	//...
	db.Collectioms.Audit.InsertOne(context.Background(), map[string]interface{}{
		"action":      actionName,
		"timestamp":   time.Now().Unix(),
	});

}	*/
