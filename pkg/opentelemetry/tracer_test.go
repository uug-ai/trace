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

func TestTracer_ContinueWithTraceData(t *testing.T) {
	const (
		traceID       = "0123456789abcdef0123456789abcdef"
		spanID        = "a1b2c3d4e5f60718"
		otherTraceID  = "fedcba9876543210fedcba9876543210"
		sampledParent = "00-" + traceID + "-" + spanID + "-01"
	)

	tests := []struct {
		name        string
		traceID     string
		data        map[string]any
		wantErr     bool
		wantTraceID string
		wantSpanID  string
		wantSampled bool
		wantState   string
	}{
		{
			name:        "continues from a sampled traceparent",
			traceID:     traceID,
			data:        map[string]any{TraceParentKey: sampledParent},
			wantTraceID: traceID,
			wantSpanID:  spanID,
			wantSampled: true,
		},
		{
			name:        "keeps an unsampled decision",
			traceID:     traceID,
			data:        map[string]any{TraceParentKey: "00-" + traceID + "-" + spanID + "-00"},
			wantTraceID: traceID,
			wantSpanID:  spanID,
			wantSampled: false,
		},
		{
			name:        "carries tracestate",
			traceID:     traceID,
			data:        map[string]any{TraceParentKey: sampledParent, TraceStateKey: "vendor=value"},
			wantTraceID: traceID,
			wantSpanID:  spanID,
			wantSampled: true,
			wantState:   "vendor=value",
		},
		{
			name:        "matches the trace ID case-insensitively",
			traceID:     strings.ToUpper(traceID),
			data:        map[string]any{TraceParentKey: sampledParent},
			wantTraceID: traceID,
			wantSpanID:  spanID,
			wantSampled: true,
		},
		{
			name:        "accepts any valid traceparent without a trace ID",
			traceID:     "",
			data:        map[string]any{TraceParentKey: sampledParent},
			wantTraceID: traceID,
			wantSpanID:  spanID,
			wantSampled: true,
		},
		{
			name:        "falls back to the trace ID without a traceparent",
			traceID:     traceID,
			data:        map[string]any{"thumby": "inline"},
			wantTraceID: traceID,
		},
		{
			name:        "falls back to the trace ID with nil data",
			traceID:     traceID,
			data:        nil,
			wantTraceID: traceID,
		},
		{
			name:        "ignores a non-string traceparent",
			traceID:     traceID,
			data:        map[string]any{TraceParentKey: 42},
			wantTraceID: traceID,
		},
		{
			name:        "rejects a traceparent from another trace",
			traceID:     traceID,
			data:        map[string]any{TraceParentKey: "00-" + otherTraceID + "-" + spanID + "-01"},
			wantErr:     true,
			wantTraceID: traceID,
		},
		{
			name:        "rejects a malformed traceparent",
			traceID:     traceID,
			data:        map[string]any{TraceParentKey: "not-a-traceparent"},
			wantErr:     true,
			wantTraceID: traceID,
		},
		{
			name:    "fails without a trace ID or traceparent",
			traceID: "",
			data:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, err := NewTracer("test-service")
			if err != nil {
				t.Fatalf("NewTracer() failed: %v", err)
			}

			newCtx, err := tracer.ContinueWithTraceData(context.Background(), tt.traceID, tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ContinueWithTraceData() error = %v, wantErr %v", err, tt.wantErr)
			}
			if newCtx == nil {
				t.Fatal("ContinueWithTraceData() returned nil context")
			}

			spanContext := otelTrace.SpanContextFromContext(newCtx)
			if tt.wantTraceID != "" && spanContext.TraceID().String() != tt.wantTraceID {
				t.Errorf("TraceID = %v, want %v", spanContext.TraceID(), tt.wantTraceID)
			}
			if tt.wantSpanID != "" {
				if spanContext.SpanID().String() != tt.wantSpanID {
					t.Errorf("SpanID = %v, want %v", spanContext.SpanID(), tt.wantSpanID)
				}
				if !spanContext.IsRemote() {
					t.Error("parent span context should be marked as remote")
				}
			} else if spanContext.SpanID().IsValid() {
				t.Errorf("SpanID = %v, want none for a trace-ID-only fallback", spanContext.SpanID())
			}
			if spanContext.IsSampled() != tt.wantSampled {
				t.Errorf("IsSampled() = %v, want %v", spanContext.IsSampled(), tt.wantSampled)
			}
			if got := spanContext.TraceState().String(); got != tt.wantState {
				t.Errorf("TraceState = %q, want %q", got, tt.wantState)
			}
		})
	}
}

func TestTracer_InjectTraceData(t *testing.T) {
	tracer, err := NewTracer("test-service")
	if err != nil {
		t.Fatalf("NewTracer() failed: %v", err)
	}

	traceID, _ := otelTrace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := otelTrace.SpanIDFromHex("a1b2c3d4e5f60718")
	state, _ := otelTrace.ParseTraceState("vendor=value")
	spanContext := otelTrace.NewSpanContext(otelTrace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: otelTrace.FlagsSampled,
		TraceState: state,
	})
	ctx := otelTrace.ContextWithSpanContext(context.Background(), spanContext)
	want := "00-" + traceID.String() + "-" + spanID.String() + "-01"

	t.Run("allocates data when nil", func(t *testing.T) {
		data := tracer.InjectTraceData(ctx, nil)
		if data[TraceParentKey] != want {
			t.Errorf("traceparent = %v, want %q", data[TraceParentKey], want)
		}
		if data[TraceStateKey] != "vendor=value" {
			t.Errorf("tracestate = %v, want %q", data[TraceStateKey], "vendor=value")
		}
	})

	t.Run("overwrites an upstream traceparent and keeps other keys", func(t *testing.T) {
		data := map[string]any{
			TraceParentKey: "00-" + traceID.String() + "-0000000000000001-01",
			"thumby":       "inline",
		}
		data = tracer.InjectTraceData(ctx, data)
		if data[TraceParentKey] != want {
			t.Errorf("traceparent = %v, want %q", data[TraceParentKey], want)
		}
		if data["thumby"] != "inline" {
			t.Errorf("thumby = %v, want it untouched", data["thumby"])
		}
	})

	t.Run("drops a stale tracestate", func(t *testing.T) {
		noState := otelTrace.ContextWithSpanContext(context.Background(), otelTrace.NewSpanContext(otelTrace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: otelTrace.FlagsSampled,
		}))
		data := tracer.InjectTraceData(noState, map[string]any{TraceStateKey: "stale=1"})
		if _, ok := data[TraceStateKey]; ok {
			t.Errorf("tracestate = %v, want it removed", data[TraceStateKey])
		}
	})

	t.Run("leaves data unchanged without a span context", func(t *testing.T) {
		upstream := "00-" + traceID.String() + "-0000000000000001-01"
		data := tracer.InjectTraceData(context.Background(), map[string]any{TraceParentKey: upstream})
		if data[TraceParentKey] != upstream {
			t.Errorf("traceparent = %v, want upstream %q kept", data[TraceParentKey], upstream)
		}
		if got := tracer.InjectTraceData(context.Background(), nil); got != nil {
			t.Errorf("InjectTraceData(nil) = %v, want nil", got)
		}
	})
}

// TestIntegration_HeadSamplingFollowsTraceData shows why downstream services
// must continue from the traceparent rather than the trace ID alone: with a
// parentbased_always_off sampler only the carried decision can make a span
// record, while a trace-ID-only parent is treated as a new (dropped) root.
func TestIntegration_HeadSamplingFollowsTraceData(t *testing.T) {
	const traceID = "0123456789abcdef0123456789abcdef"

	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(
		trace.WithSyncer(exporter),
		trace.WithSampler(trace.ParentBased(trace.NeverSample())),
	)
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	tracer, _ := NewTracer("test-service")
	tests := []struct {
		name       string
		data       map[string]any
		wantRecord bool
	}{
		{"sampled upstream", map[string]any{TraceParentKey: "00-" + traceID + "-a1b2c3d4e5f60718-01"}, true},
		{"unsampled upstream", map[string]any{TraceParentKey: "00-" + traceID + "-a1b2c3d4e5f60718-00"}, false},
		{"trace ID only", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, err := tracer.ContinueWithTraceData(context.Background(), traceID, tt.data)
			if err != nil {
				t.Fatalf("ContinueWithTraceData() error = %v", err)
			}
			stageCtx, span := tracer.CreateSpan(ctx, nil)
			if span.IsRecording() != tt.wantRecord {
				t.Errorf("IsRecording() = %v, want %v", span.IsRecording(), tt.wantRecord)
			}

			// The forwarded traceparent names this stage's span and keeps the decision.
			data := tracer.InjectTraceData(stageCtx, nil)
			forwarded, err := tracer.ContinueWithTraceData(context.Background(), traceID, data)
			if err != nil {
				t.Fatalf("ContinueWithTraceData(forwarded) error = %v", err)
			}
			next := otelTrace.SpanContextFromContext(forwarded)
			if next.SpanID() != span.SpanContext().SpanID() {
				t.Errorf("forwarded parent = %v, want this stage's span %v", next.SpanID(), span.SpanContext().SpanID())
			}
			if tt.data != nil && next.IsSampled() != tt.wantRecord {
				t.Errorf("forwarded IsSampled() = %v, want %v", next.IsSampled(), tt.wantRecord)
			}
			span.End()
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
			serviceName: "trace",
			level:       1,
			wantErr:     false,
			wantContain: []string{"github.com/uug-ai/trace", "tracer_test.go", "#L"},
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

// The typed carrier round-trips through a message hop with the same semantics
// as the data-map helpers: the consumer nests under the sender's span and keeps
// its sampling decision, and a carrier without a traceparent falls back to the
// trace ID.
func TestTracer_TraceContextCarrier(t *testing.T) {
	const traceID = "0123456789abcdef0123456789abcdef"
	tracer, _ := NewTracer("test-service")

	spanID, _ := otelTrace.SpanIDFromHex("a1b2c3d4e5f60718")
	tid, _ := otelTrace.TraceIDFromHex(traceID)
	state, _ := otelTrace.ParseTraceState("vendor=value")
	for _, flags := range []otelTrace.TraceFlags{otelTrace.FlagsSampled, 0} {
		sender := otelTrace.ContextWithSpanContext(context.Background(), otelTrace.NewSpanContext(otelTrace.SpanContextConfig{
			TraceID: tid, SpanID: spanID, TraceFlags: flags, TraceState: state,
		}))

		carrier := tracer.InjectTraceContext(sender)
		if want := "00-" + traceID + "-" + spanID.String() + "-" + flags.String(); carrier.TraceParent != want {
			t.Fatalf("TraceParent = %q, want %q", carrier.TraceParent, want)
		}
		if carrier.TraceState != "vendor=value" {
			t.Fatalf("TraceState = %q, want %q", carrier.TraceState, "vendor=value")
		}

		ctx, err := tracer.ContinueWithTraceContext(context.Background(), traceID, carrier)
		if err != nil {
			t.Fatalf("ContinueWithTraceContext() error = %v", err)
		}
		parent := otelTrace.SpanContextFromContext(ctx)
		if parent.SpanID() != spanID || !parent.IsRemote() || parent.IsSampled() != flags.IsSampled() {
			t.Fatalf("parent = %v remote=%v sampled=%v, want sender span with its decision", parent.SpanID(), parent.IsRemote(), parent.IsSampled())
		}
		if parent.TraceState().String() != "vendor=value" {
			t.Fatalf("TraceState = %q", parent.TraceState().String())
		}
	}

	if got := tracer.InjectTraceContext(context.Background()); got != (TraceContextCarrier{}) {
		t.Fatalf("InjectTraceContext() without a span = %+v, want empty", got)
	}
	ctx, err := tracer.ContinueWithTraceContext(context.Background(), traceID, TraceContextCarrier{})
	if err != nil {
		t.Fatalf("ContinueWithTraceContext(empty) error = %v", err)
	}
	if fallback := otelTrace.SpanContextFromContext(ctx); fallback.TraceID().String() != traceID || fallback.SpanID().IsValid() {
		t.Fatalf("empty carrier = %v/%v, want the trace-ID-only fallback", fallback.TraceID(), fallback.SpanID())
	}
	if _, err := tracer.ContinueWithTraceContext(context.Background(), traceID, TraceContextCarrier{TraceParent: "garbage"}); err == nil {
		t.Fatal("ContinueWithTraceContext(malformed) error = nil, want the degradation reported")
	}
}
