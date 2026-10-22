package opentelemetry

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
)

// SamplingClassKey names the sampling class of a trace's root span (see
// CreateClassSpan), e.g. "upload". It is set when the span starts, the only
// moment a sampler can read it.
const SamplingClassKey = attribute.Key("sampling.class")

// TraceSamplerRatiosEnv configures a sampling ratio per sampling class, so one
// service can trace one kind of work (e.g. uploads) at its own rate and the
// rest at another, or not at all:
//
//	TRACE_SAMPLER_RATIOS="upload=0.1,default=0"
//
// Each entry is class=ratio with a ratio between 0 and 1; "default" applies to
// new traces without a listed class and is 1 (record everything, the SDK
// default) when omitted. Spans that continue a parent always follow the
// parent's decision. When set, it takes precedence over OTEL_TRACES_SAMPLER.
const TraceSamplerRatiosEnv = "TRACE_SAMPLER_RATIOS"

const defaultSamplingClass = "default"

// classRatioSampler samples a new trace at the ratio of its root span's
// sampling class (SamplingClassKey), or at the default ratio.
type classRatioSampler struct {
	classes     map[string]trace.Sampler
	fallback    trace.Sampler
	description string
}

func (s classRatioSampler) ShouldSample(parameters trace.SamplingParameters) trace.SamplingResult {
	for _, kv := range parameters.Attributes {
		if kv.Key == SamplingClassKey {
			if sampler, ok := s.classes[kv.Value.AsString()]; ok {
				return sampler.ShouldSample(parameters)
			}
			break
		}
	}
	return s.fallback.ShouldSample(parameters)
}

func (s classRatioSampler) Description() string {
	return s.description
}

// classSamplerFromEnv returns the parent-based class sampler configured by
// TraceSamplerRatiosEnv, or nil when it is unset so the SDK's own
// OTEL_TRACES_SAMPLER configuration applies.
func classSamplerFromEnv() (trace.Sampler, error) {
	raw := strings.TrimSpace(os.Getenv(TraceSamplerRatiosEnv))
	if raw == "" {
		return nil, nil
	}
	root, err := parseClassRatios(raw)
	if err != nil {
		return nil, err
	}
	return trace.ParentBased(root), nil
}

func parseClassRatios(raw string) (classRatioSampler, error) {
	sampler := classRatioSampler{classes: map[string]trace.Sampler{}, fallback: trace.AlwaysSample()}
	ratios := map[string]float64{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		class, value, ok := strings.Cut(entry, "=")
		class = strings.TrimSpace(class)
		ratio, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if !ok || class == "" || err != nil || ratio < 0 || ratio > 1 {
			return classRatioSampler{}, fmt.Errorf("%s: invalid entry %q, want class=ratio with a ratio between 0 and 1", TraceSamplerRatiosEnv, entry)
		}
		if _, duplicate := ratios[class]; duplicate {
			return classRatioSampler{}, fmt.Errorf("%s: class %q is listed twice", TraceSamplerRatiosEnv, class)
		}
		ratios[class] = ratio
		if class == defaultSamplingClass {
			sampler.fallback = trace.TraceIDRatioBased(ratio)
		} else {
			sampler.classes[class] = trace.TraceIDRatioBased(ratio)
		}
	}
	if len(ratios) == 0 {
		return classRatioSampler{}, fmt.Errorf("%s: no class=ratio entries", TraceSamplerRatiosEnv)
	}
	names := make([]string, 0, len(ratios))
	for class, ratio := range ratios {
		names = append(names, fmt.Sprintf("%s=%g", class, ratio))
	}
	sort.Strings(names)
	sampler.description = "ClassRatioSampler{" + strings.Join(names, ",") + "}"
	return sampler, nil
}
