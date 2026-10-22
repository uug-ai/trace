package opentelemetry

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	otelTrace "go.opentelemetry.io/otel/trace"
)

func TestParseClassRatios(t *testing.T) {
	for raw, want := range map[string]string{
		"upload=0.1,default=0":         "ClassRatioSampler{default=0,upload=0.1}",
		" upload = 1 , heartbeat=0.01": "ClassRatioSampler{heartbeat=0.01,upload=1}",
		"default=0":                    "ClassRatioSampler{default=0}",
	} {
		sampler, err := parseClassRatios(raw)
		if err != nil || sampler.Description() != want {
			t.Errorf("parseClassRatios(%q) = %q, %v; want %q", raw, sampler.Description(), err, want)
		}
	}
	for _, raw := range []string{"upload", "upload=", "=0.1", "upload=1.5", "upload=-0.1", "upload=abc", "upload=0.1,upload=0.2", " , "} {
		if _, err := parseClassRatios(raw); err == nil {
			t.Errorf("parseClassRatios(%q) = nil error, want invalid", raw)
		}
	}
}

// recordWithClassRatios installs a provider with the class sampler configured
// by ratios and returns the recorder of its ended spans.
func recordWithClassRatios(t *testing.T, ratios string) *tracetest.SpanRecorder {
	t.Helper()
	t.Setenv(TraceSamplerRatiosEnv, ratios)
	sampler, err := classSamplerFromEnv()
	if err != nil {
		t.Fatalf("classSamplerFromEnv() error = %v", err)
	}
	recorder := tracetest.NewSpanRecorder()
	provider := trace.NewTracerProvider(trace.WithSampler(sampler), trace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return recorder
}

// Uploads are traced at their own ratio while everything else is off.
func TestClassSamplerTracesOneClassOnly(t *testing.T) {
	recorder := recordWithClassRatios(t, "upload=1,default=0")
	tracer, _ := NewTracer("test-service")

	_, upload := tracer.CreateClassSpan(context.Background(), "upload", nil)
	_, other := tracer.CreateSpan(context.Background(), nil)
	_, unlisted := tracer.CreateClassSpan(context.Background(), "heartbeat", nil)
	if !upload.IsRecording() {
		t.Error("an upload root was dropped at upload=1")
	}
	if other.IsRecording() || unlisted.IsRecording() {
		t.Error("a root without a listed class was recorded at default=0")
	}
	upload.End()
	other.End()
	unlisted.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want the upload only", len(ended))
	}
	if value, ok := spanAttribute(ended[0], SamplingClassKey); !ok || value.AsString() != "upload" {
		t.Fatalf("upload span %s = %v, want %q", SamplingClassKey, value.AsInterface(), "upload")
	}
	if !strings.Contains(ended[0].Name(), "TestClassSamplerTracesOneClassOnly") {
		t.Fatalf("span name = %q, want the caller's function", ended[0].Name())
	}
}

func TestClassSamplerCanTurnAClassOff(t *testing.T) {
	recorder := recordWithClassRatios(t, "upload=0")
	tracer, _ := NewTracer("test-service")

	_, upload := tracer.CreateClassSpan(context.Background(), "upload", nil)
	_, other := tracer.CreateSpan(context.Background(), nil)
	if upload.IsRecording() {
		t.Error("an upload root was recorded at upload=0")
	}
	if !other.IsRecording() {
		t.Error("an unlisted root was dropped although default is omitted (SDK default: record)")
	}
	upload.End()
	other.End()
	if len(recorder.Ended()) != 1 {
		t.Fatalf("recorded %d spans, want the unclassed one only", len(recorder.Ended()))
	}
}

// A span that continues a parent follows the parent's decision whatever its
// class, so a classed span inside an unsampled trace stays unsampled.
func TestClassSamplerFollowsParent(t *testing.T) {
	recordWithClassRatios(t, "upload=1,default=0")
	tracer, _ := NewTracer("test-service")
	traceID, _ := otelTrace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := otelTrace.SpanIDFromHex("a1b2c3d4e5f60718")

	for _, flags := range []otelTrace.TraceFlags{otelTrace.FlagsSampled, 0} {
		parent := otelTrace.ContextWithRemoteSpanContext(context.Background(), otelTrace.NewSpanContext(otelTrace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID, TraceFlags: flags, Remote: true,
		}))
		_, other := tracer.CreateSpan(parent, nil)
		_, upload := tracer.CreateClassSpan(parent, "upload", nil)
		if other.IsRecording() != flags.IsSampled() || upload.IsRecording() != flags.IsSampled() {
			t.Errorf("parent sampled=%v: unclassed recording=%v, upload recording=%v; want both to follow the parent",
				flags.IsSampled(), other.IsRecording(), upload.IsRecording())
		}
		other.End()
		upload.End()
	}
}

// A malformed setting leaves tracing off and is reported, instead of
// sampling at an unintended rate.
func TestConnectRejectsInvalidClassRatios(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv(TraceSamplerRatiosEnv, "upload=10")
	tracer, _ := NewTracer("test-service")
	if err := tracer.Connect(); err == nil || !strings.Contains(err.Error(), TraceSamplerRatiosEnv) {
		t.Fatalf("Connect() error = %v, want the %s error", err, TraceSamplerRatiosEnv)
	}
	if tracer.traceProvider != nil {
		t.Fatal("a tracer provider was installed despite the invalid sampler setting")
	}
}

func spanAttribute(span trace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range span.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}
