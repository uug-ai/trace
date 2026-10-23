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
	for _, tt := range []struct {
		environ []string
		want    string
	}{
		{[]string{"TRACE_SAMPLER_RATIO_UPLOAD=0.1", "TRACE_SAMPLER_RATIO_OTHER=0", "PATH=/usr/bin"}, "ClassRatioSampler{other=0,upload=0.1}"},
		{[]string{"TRACE_SAMPLER_RATIO_UPLOAD= 1 ", "TRACE_SAMPLER_RATIO_HEARTBEAT=0.01"}, "ClassRatioSampler{heartbeat=0.01,upload=1}"},
		{[]string{"TRACE_SAMPLER_RATIO_OTHER=0"}, "ClassRatioSampler{other=0}"},
	} {
		sampler, configured, err := parseClassRatios(tt.environ)
		if err != nil || !configured || sampler.Description() != tt.want {
			t.Errorf("parseClassRatios(%v) = %q, %v, %v; want %q", tt.environ, sampler.Description(), configured, err, tt.want)
		}
	}
	for _, environ := range [][]string{nil, {"PATH=/usr/bin"}, {"TRACE_SAMPLER_RATIO_UPLOAD="}, {"TRACE_SAMPLER_RATIO_UPLOAD= "}} {
		if _, configured, err := parseClassRatios(environ); configured || err != nil {
			t.Errorf("parseClassRatios(%v) = configured %v, %v; want unset", environ, configured, err)
		}
	}
	for _, environ := range [][]string{
		{"TRACE_SAMPLER_RATIO_UPLOAD=1.5"},
		{"TRACE_SAMPLER_RATIO_UPLOAD=-0.1"},
		{"TRACE_SAMPLER_RATIO_UPLOAD=abc"},
		{"TRACE_SAMPLER_RATIO_=0.1"},
		{"TRACE_SAMPLER_RATIO_UPLOAD=0.1", "TRACE_SAMPLER_RATIO_upload=0.2"},
	} {
		if _, _, err := parseClassRatios(environ); err == nil {
			t.Errorf("parseClassRatios(%v) = nil error, want invalid", environ)
		}
	}
}

// The catch-all was called DEFAULT in v1.2.5. Accepting it as an ordinary
// class would leave OTHER unset and record everything, so it is rejected with
// a pointer to the new name.
func TestParseClassRatiosRejectsRetiredDefault(t *testing.T) {
	_, _, err := parseClassRatios([]string{"TRACE_SAMPLER_RATIO_UPLOAD=0.1", "TRACE_SAMPLER_RATIO_DEFAULT=0"})
	if err == nil || !strings.Contains(err.Error(), "TRACE_SAMPLER_RATIO_OTHER") {
		t.Fatalf("error = %v, want a pointer to TRACE_SAMPLER_RATIO_OTHER", err)
	}
}

// recordWithClassRatios sets the given TRACE_SAMPLER_RATIO_<CLASS> variables,
// installs a provider with the class sampler they configure and returns the
// recorder of its ended spans.
func recordWithClassRatios(t *testing.T, ratios map[string]string) *tracetest.SpanRecorder {
	t.Helper()
	for class, ratio := range ratios {
		t.Setenv(TraceSamplerRatioEnvPrefix+class, ratio)
	}
	sampler, _, err := classSamplerFromEnv()
	if err != nil || sampler == nil {
		t.Fatalf("classSamplerFromEnv() = %v, %v", sampler, err)
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
	recorder := recordWithClassRatios(t, map[string]string{"UPLOAD": "1", "OTHER": "0"})
	tracer, _ := NewTracer("test-service")

	_, upload := tracer.CreateClassSpan(context.Background(), "upload", nil)
	_, other := tracer.CreateSpan(context.Background(), nil)
	_, unlisted := tracer.CreateClassSpan(context.Background(), "heartbeat", nil)
	if !upload.IsRecording() {
		t.Error("an upload root was dropped at upload=1")
	}
	if other.IsRecording() || unlisted.IsRecording() {
		t.Error("a root without a listed class was recorded at other=0")
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

// Only UPLOAD set: uploads are off and everything else keeps the SDK default.
func TestClassSamplerCanTurnAClassOff(t *testing.T) {
	recorder := recordWithClassRatios(t, map[string]string{"UPLOAD": "0"})
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
	recordWithClassRatios(t, map[string]string{"UPLOAD": "1", "OTHER": "0"})
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
	t.Setenv("TRACE_SAMPLER_RATIO_UPLOAD", "10")
	tracer, _ := NewTracer("test-service")
	if err := tracer.Connect(); err == nil || !strings.Contains(err.Error(), "TRACE_SAMPLER_RATIO_UPLOAD") {
		t.Fatalf("Connect() error = %v, want the TRACE_SAMPLER_RATIO_UPLOAD error", err)
	}
	if tracer.traceProvider != nil {
		t.Fatal("a tracer provider was installed despite the invalid sampler setting")
	}
}

// Sampler reports the ratios Connect installed, so a service can log them.
func TestConnectReportsSampler(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"class ratios", map[string]string{"TRACE_SAMPLER_RATIO_UPLOAD": "0.1", "TRACE_SAMPLER_RATIO_OTHER": "0"}, "ParentBased(ClassRatioSampler{other=0,upload=0.1})"},
		{"standard sampler", map[string]string{"OTEL_TRACES_SAMPLER": "parentbased_always_off"}, "OTEL_TRACES_SAMPLER=parentbased_always_off"},
		{"standard sampler with arg", map[string]string{"OTEL_TRACES_SAMPLER": "parentbased_traceidratio", "OTEL_TRACES_SAMPLER_ARG": "0.1"}, "OTEL_TRACES_SAMPLER=parentbased_traceidratio OTEL_TRACES_SAMPLER_ARG=0.1"},
		{"nothing set", nil, "OTEL_TRACES_SAMPLER unset (parentbased_always_on)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			previous := otel.GetTracerProvider()
			tracer, _ := NewTracer("test-service")
			if err := tracer.Connect(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = tracer.Shutdown(context.Background())
				otel.SetTracerProvider(previous)
			})
			if got := tracer.Sampler(); got != tt.want {
				t.Fatalf("Sampler() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Without an endpoint tracing is off, and Sampler says so by being empty.
func TestSamplerEmptyWhileTracingOff(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("TRACE_SAMPLER_RATIO_UPLOAD", "0.1")
	tracer, _ := NewTracer("test-service")
	if err := tracer.Connect(); err != nil {
		t.Fatal(err)
	}
	if got := tracer.Sampler(); got != "" {
		t.Fatalf("Sampler() = %q, want empty", got)
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
