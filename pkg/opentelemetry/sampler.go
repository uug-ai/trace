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

// TraceSamplerRatioEnvPrefix names one environment variable per sampling
// class, holding that class's sampling ratio between 0 and 1, so one service
// can trace one kind of work (e.g. uploads) at its own rate and the rest at
// another, or not at all:
//
//	TRACE_SAMPLER_RATIO_UPLOAD=0.1   # new traces started with CreateClassSpan(ctx, "upload", ...)
//	TRACE_SAMPLER_RATIO_OTHER=0      # every other new trace
//
// The class is the variable's suffix, lowercased. OTHER covers new traces
// without a listed class and is 1 (record everything, the SDK default) when
// unset. Spans that continue a parent always follow the parent's decision.
// When any of these variables is set, they take precedence over
// OTEL_TRACES_SAMPLER.
const TraceSamplerRatioEnvPrefix = "TRACE_SAMPLER_RATIO_"

// otherSamplingClass covers every new trace without a listed class.
const otherSamplingClass = "other"

// retiredSamplingClasses were names of the catch-all class in earlier
// releases. Setting one is rejected: it would otherwise become an unused class
// and leave the catch-all at its record-everything default.
var retiredSamplingClasses = map[string]string{"default": otherSamplingClass}

// classRatioSampler samples a new trace at the ratio of its root span's
// sampling class (SamplingClassKey), or at the ratio of the other class.
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
// the TraceSamplerRatioEnvPrefix variables and its description, or nil when
// none is set so the SDK's own OTEL_TRACES_SAMPLER configuration applies.
func classSamplerFromEnv() (trace.Sampler, string, error) {
	root, configured, err := parseClassRatios(os.Environ())
	if err != nil || !configured {
		return nil, "", err
	}
	return trace.ParentBased(root), "ParentBased(" + root.Description() + ")", nil
}

// parseClassRatios reads the TraceSamplerRatioEnvPrefix variables from an
// environment in os.Environ form. configured is false when none is set; empty
// values count as unset.
func parseClassRatios(environ []string) (_ classRatioSampler, configured bool, _ error) {
	sampler := classRatioSampler{classes: map[string]trace.Sampler{}, fallback: trace.AlwaysSample()}
	ratios := map[string]float64{}
	for _, variable := range environ {
		name, value, _ := strings.Cut(variable, "=")
		if !strings.HasPrefix(name, TraceSamplerRatioEnvPrefix) {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		class := strings.ToLower(strings.TrimPrefix(name, TraceSamplerRatioEnvPrefix))
		if replacement, retired := retiredSamplingClasses[class]; retired {
			return classRatioSampler{}, false, fmt.Errorf("%s is no longer supported: use %s%s", name, TraceSamplerRatioEnvPrefix, strings.ToUpper(replacement))
		}
		ratio, err := strconv.ParseFloat(value, 64)
		if class == "" || err != nil || ratio < 0 || ratio > 1 {
			return classRatioSampler{}, false, fmt.Errorf("%s=%q: want a sampling class suffix and a ratio between 0 and 1", name, value)
		}
		if _, duplicate := ratios[class]; duplicate {
			return classRatioSampler{}, false, fmt.Errorf("%s: sampling class %q is set twice", name, class)
		}
		ratios[class] = ratio
		if class == otherSamplingClass {
			sampler.fallback = trace.TraceIDRatioBased(ratio)
		} else {
			sampler.classes[class] = trace.TraceIDRatioBased(ratio)
		}
	}
	if len(ratios) == 0 {
		return classRatioSampler{}, false, nil
	}
	names := make([]string, 0, len(ratios))
	for class, ratio := range ratios {
		names = append(names, fmt.Sprintf("%s=%g", class, ratio))
	}
	sort.Strings(names)
	sampler.description = "ClassRatioSampler{" + strings.Join(names, ",") + "}"
	return sampler, true, nil
}
