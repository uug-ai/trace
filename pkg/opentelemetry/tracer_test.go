package opentelemetry

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	otelTrace "go.opentelemetry.io/otel/trace"
)

func TestNewTracer(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		wantErr     bool
	}{
		{
			name:        "creates tracer with valid service name",
			serviceName: "test-service",
			wantErr:     false,
		},
		{
			name:        "creates tracer with empty service name",
			serviceName: "",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, err := NewTracer(tt.serviceName)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewTracer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tracer == nil {
				t.Error("NewTracer() returned nil tracer")
				return
			}
			if tracer.ServiceName != tt.serviceName {
				t.Errorf("NewTracer() ServiceName = %v, want %v", tracer.ServiceName, tt.serviceName)
			}
		})
	}
}

func TestTracer_Connect(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		wantErr      bool
		errContains  string
		wantProvider bool
	}{
		{
			// Tracing is optional: an unset endpoint must not error. The
			// tracer stays in no-op mode (no provider installed) so callers
			// that treat a Connect error as fatal are not crashed.
			name:         "no-op when OTEL_EXPORTER_OTLP_ENDPOINT not set",
			endpoint:     "",
			wantErr:      false,
			wantProvider: false,
		},
		{
			name:         "succeeds with http endpoint",
			endpoint:     "http://localhost:4318",
			wantErr:      false,
			wantProvider: true,
		},
		{
			name:         "succeeds with https endpoint",
			endpoint:     "https://otel-collector.example.com:4318",
			wantErr:      false,
			wantProvider: true,
		},
		{
			name:         "succeeds with hostname only",
			endpoint:     "localhost:4318",
			wantErr:      false,
			wantProvider: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup
			originalEnv := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
			defer os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", originalEnv)

			if tt.endpoint != "" {
				os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tt.endpoint)
			} else {
				os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
			}

			tracer, err := NewTracer("test-service")
			if err != nil {
				t.Fatalf("NewTracer() failed: %v", err)
			}

			// Execute
			err = tracer.Connect()

			// Assert
			if (err != nil) != tt.wantErr {
				t.Errorf("Connect() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Connect() error = %v, want error containing %q", err, tt.errContains)
				}
			}

			if tt.wantProvider {
				if tracer.traceProvider == nil {
					t.Error("Connect() did not set traceProvider")
				}
			} else if tracer.traceProvider != nil {
				t.Error("Connect() set traceProvider in no-op mode")
			}

			// Cleanup
			if tracer.traceProvider != nil {
				_ = tracer.traceProvider.Shutdown(context.Background())
			}
		})
	}
}

func TestTracer_CreateSpan(t *testing.T) {
	// Setup in-memory span recorder for testing
	spanRecorder := tracetest.NewSpanRecorder()
	traceProvider := trace.NewTracerProvider(
		trace.WithSpanProcessor(spanRecorder),
	)
	otel.SetTracerProvider(traceProvider)
	defer traceProvider.Shutdown(context.Background())

	tests := []struct {
		name           string
		serviceName    string
		parameters     map[string]string
		environment    string
		contextDone    bool
		wantSpanName   bool
		wantAttributes int
	}{
		{
			name:        "creates span with parameters",
			serviceName: "tracer",
			parameters: map[string]string{
				"user_id":  "123",
				"action":   "test",
				"resource": "item",
			},
			environment:    "testing",
			contextDone:    false,
			wantSpanName:   true,
			wantAttributes: 5, // environment + 3 params + potentially url
		},
		{
			name:           "creates span with empty parameters",
			serviceName:    "tracer",
			parameters:     map[string]string{},
			environment:    "production",
			contextDone:    false,
			wantSpanName:   true,
			wantAttributes: 1, // just environment (+ potentially url)
		},
		{
			name:        "handles cancelled context gracefully",
			serviceName: "tracer",
			parameters:  map[string]string{},
			environment: "testing",
			contextDone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup environment
			if tt.environment != "" {
				os.Setenv("ENVIRONMENT", tt.environment)
				defer os.Unsetenv("ENVIRONMENT")
			}

			tracer, err := NewTracer(tt.serviceName)
			if err != nil {
				t.Fatalf("NewTracer() failed: %v", err)
			}

			// Create context
			var ctx context.Context
			var cancel context.CancelFunc
			if tt.contextDone {
				ctx, cancel = context.WithCancel(context.Background())
				cancel() // Cancel immediately
			} else {
				ctx = context.Background()
			}

			// Clear previous spans
			spanRecorder.Reset()

			// Execute
			newCtx, span := tracer.CreateSpan(ctx, tt.parameters)

			if newCtx == nil {
				t.Error("CreateSpan() returned nil context")
			}

			if span == nil {
				t.Error("CreateSpan() returned nil span")
				return
			}

			// For cancelled context, we just get the existing span
			if tt.contextDone {
				return
			}

			// End the span to finalize it
			span.End()

			// Verify span was recorded
			spans := spanRecorder.Ended()
			if len(spans) == 0 {
				t.Error("CreateSpan() did not create a span")
				return
			}

			recordedSpan := spans[0]

			// Verify span has a name
			if tt.wantSpanName && recordedSpan.Name() == "" {
				t.Error("CreateSpan() created span with empty name")
			}

			// Verify attributes
			attrs := recordedSpan.Attributes()
			hasEnvironment := false
			paramCount := 0

			for _, attr := range attrs {
				if attr.Key == "environment" {
					hasEnvironment = true
					if attr.Value.AsString() != tt.environment {
						t.Errorf("environment attribute = %v, want %v", attr.Value.AsString(), tt.environment)
					}
				}
				if strings.HasPrefix(string(attr.Key), "param.") {
					paramCount++
					paramKey := strings.TrimPrefix(string(attr.Key), "param.")
					expectedValue, ok := tt.parameters[paramKey]
					if !ok {
						t.Errorf("unexpected parameter attribute: %s", paramKey)
					} else if attr.Value.AsString() != expectedValue {
						t.Errorf("parameter %s = %v, want %v", paramKey, attr.Value.AsString(), expectedValue)
					}
				}
			}

			if !hasEnvironment {
				t.Error("CreateSpan() did not set environment attribute")
			}

			if paramCount != len(tt.parameters) {
				t.Errorf("CreateSpan() set %d parameter attributes, want %d", paramCount, len(tt.parameters))
			}
		})
	}
}

func TestTracer_ContinueWithTrace(t *testing.T) {
	tests := []struct {
		name        string
		traceID     string
		wantErr     bool
		errContains string
	}{
		{
			name:    "continues with valid trace ID",
			traceID: "0123456789abcdef0123456789abcdef",
			wantErr: false,
		},
		{
			name:        "fails with invalid trace ID",
			traceID:     "invalid-trace-id",
			wantErr:     true,
			errContains: "invalid trace ID",
		},
		{
			name:        "fails with empty trace ID",
			traceID:     "",
			wantErr:     true,
			errContains: "invalid trace ID",
		},
		{
			name:        "fails with short trace ID",
			traceID:     "0123456789abcdef",
			wantErr:     true,
			errContains: "invalid trace ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, err := NewTracer("test-service")
			if err != nil {
				t.Fatalf("NewTracer() failed: %v", err)
			}

			ctx := context.Background()

			// Execute
			newCtx, err := tracer.ContinueWithTrace(ctx, tt.traceID)

			// Assert
			if (err != nil) != tt.wantErr {
				t.Errorf("ContinueWithTrace() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ContinueWithTrace() error = %v, want error containing %q", err, tt.errContains)
				}
			}

			if !tt.wantErr {
				if newCtx == nil {
					t.Error("ContinueWithTrace() returned nil context")
				}

				// Verify the span context has the correct trace ID
				spanContext := otelTrace.SpanContextFromContext(newCtx)

				// Note: The span context will not be fully valid since SpanID is empty,
				// but we can still verify the trace ID was set correctly
				if spanContext.TraceID().String() != tt.traceID {
					t.Errorf("ContinueWithTrace() TraceID = %v, want %v", spanContext.TraceID().String(), tt.traceID)
				}

				if !spanContext.IsRemote() {
					t.Error("ContinueWithTrace() span context should be marked as remote")
				}
			}
		})
	}
}

func TestTracer_GetCallerFunctionName(t *testing.T) {
	tracer, err := NewTracer("tracer")
	if err != nil {
		t.Fatalf("NewTracer() failed: %v", err)
	}

	// Test from this function (level 1 = GetCallerFunctionName, level 2 = this test)
	functionName, err := tracer.GetCallerFunctionName(1)
	if err != nil {
		t.Errorf("GetCallerFunctionName() error = %v", err)
		return
	}

	if functionName == "" {
		t.Error("GetCallerFunctionName() returned empty string")
	}

	// Should contain the test function name
	if !strings.Contains(functionName, "TestTracer_GetCallerFunctionName") {
		t.Errorf("GetCallerFunctionName() = %v, want it to contain 'TestTracer_GetCallerFunctionName'", functionName)
	}

	// Should not contain full package path
	if strings.Contains(functionName, "github.com") {
		t.Errorf("GetCallerFunctionName() = %v, should not contain full package path", functionName)
	}
}

func TestTracer_ReturnFilePath(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		level       int
		wantErr     bool
		wantContain string
	}{
		{
			name:        "returns file path from current call",
			serviceName: "tracer",
			level:       1,
			wantErr:     false,
			wantContain: "tracer_test.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, err := NewTracer(tt.serviceName)
			if err != nil {
				t.Fatalf("NewTracer() failed: %v", err)
			}

			filePath, err := tracer.ReturnFilePath(tt.level)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReturnFilePath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if filePath == "" {
					t.Error("ReturnFilePath() returned empty string")
				}

				if tt.wantContain != "" && !strings.Contains(filePath, tt.wantContain) {
					t.Errorf("ReturnFilePath() = %v, want it to contain %q", filePath, tt.wantContain)
				}
			}
		})
	}
}

func TestTracer_ReturnGitHubEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		level       int
		wantErr     bool
		wantContain []string
	}{
		{
			name:        "returns GitHub endpoint from current call",
			serviceName: "tracer",
			level:       1,
			wantErr:     false,
			wantContain: []string{"github.com/uug-ai/tracer", "tracer_test.go", "#L"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, err := NewTracer(tt.serviceName)
			if err != nil {
				t.Fatalf("NewTracer() failed: %v", err)
			}

			githubURL, err := tracer.ReturnGitHubEndpoint(tt.level)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReturnGitHubEndpoint() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if githubURL == "" {
					t.Error("ReturnGitHubEndpoint() returned empty string")
				}

				for _, want := range tt.wantContain {
					if !strings.Contains(githubURL, want) {
						t.Errorf("ReturnGitHubEndpoint() = %v, want it to contain %q", githubURL, want)
					}
				}
			}
		})
	}
}

func TestTracer_Audit(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]string
	}{
		{
			name: "audits with parameters",
			parameters: map[string]string{
				"action": "create",
				"user":   "test-user",
			},
		},
		{
			name:       "audits with empty parameters",
			parameters: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, err := NewTracer("test-service")
			if err != nil {
				t.Fatalf("NewTracer() failed: %v", err)
			}

			// Audit should not panic
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Audit() panicked: %v", r)
				}
			}()

			tracer.Audit(tt.parameters)
		})
	}
}

// TestIntegration_SpanCreationAndExport demonstrates integration testing
// with in-memory span recording
func TestIntegration_SpanCreationAndExport(t *testing.T) {
	// Setup
	spanRecorder := tracetest.NewSpanRecorder()
	traceProvider := trace.NewTracerProvider(
		trace.WithSpanProcessor(spanRecorder),
	)
	otel.SetTracerProvider(traceProvider)
	defer traceProvider.Shutdown(context.Background())

	// Create and initialize tracer
	tracer, err := NewTracer("tracer")
	if err != nil {
		t.Fatalf("NewTracer() failed: %v", err)
	}

	// Create a span
	ctx := context.Background()
	params := map[string]string{
		"user_id": "12345",
		"action":  "integration_test",
	}

	ctx, span := tracer.CreateSpan(ctx, params)

	// Add additional attributes
	if s, ok := span.(otelTrace.Span); ok {
		s.SetAttributes(attribute.String("test.type", "integration"))
	}

	span.End()

	// Verify span was recorded
	spans := spanRecorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("Expected 1 span, got %d", len(spans))
	}

	recordedSpan := spans[0]

	// Verify span properties
	if recordedSpan.Name() == "" {
		t.Error("Span has no name")
	}

	// Verify attributes exist
	attrs := recordedSpan.Attributes()
	if len(attrs) == 0 {
		t.Error("Span has no attributes")
	}

	// Check for expected attributes
	hasUserID := false
	hasAction := false
	for _, attr := range attrs {
		if attr.Key == "param.user_id" && attr.Value.AsString() == "12345" {
			hasUserID = true
		}
		if attr.Key == "param.action" && attr.Value.AsString() == "integration_test" {
			hasAction = true
		}
	}

	if !hasUserID {
		t.Error("Span missing user_id parameter")
	}
	if !hasAction {
		t.Error("Span missing action parameter")
	}
}
