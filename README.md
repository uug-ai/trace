# Tracer

A Go library that provides a simple and flexible interface for distributed tracing using OpenTelemetry. This library makes it easy to instrument your Go applications with tracing capabilities, allowing you to monitor and debug distributed systems.

## Overview

The `tracer` library provides:
- A generic `TracerInterface` that can be implemented by different tracing backends
- An OpenTelemetry implementation for industry-standard distributed tracing
- Automatic span creation with context propagation
- Trace continuation across service boundaries
- GitHub integration for linking spans to source code

## Features

- **OpenTelemetry Support**: Built-in implementation using OpenTelemetry SDK
- **Context Propagation**: Seamlessly propagate trace context across service boundaries
- **Automatic Span Naming**: Automatically names spans based on the calling function
- **Flexible Configuration**: Configure via environment variables
- **GitHub Integration**: Links spans to source code locations on GitHub
- **Audit Logging**: Built-in audit capabilities for tracking important actions

## Installation

```bash
go get github.com/uug-ai/trace
```

## Quick Start

### 1. Initialize the Tracer

```go
package main

import (
    "context"
    "log"
    "github.com/uug-ai/tracer/pkg/opentelemetry"
)

func main() {
    // Create a new OpenTelemetry tracer
    tracer, err := opentelemetry.NewTracer("my-service")
    if err != nil {
        log.Fatal("Failed to create tracer: ", err)
    }
    
    // Connect to the OTLP endpoint
    if err := tracer.Connect(); err != nil {
        log.Println("Failed to connect tracer: ", err)
    }
}
```

### 2. Configure Environment Variables

Set the following environment variables:

```bash
# Required: OTLP endpoint for sending traces
export OTEL_EXPORTER_OTLP_ENDPOINT="localhost:4318"

# Optional: Environment identifier
export ENVIRONMENT="production"
```

For HTTPS endpoints, use the full URL:
```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="https://otel-collector.example.com:4318"
```

For HTTP endpoints (insecure):
```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="http://localhost:4318"
```

## Usage

### Creating Spans with CreateSpan

The `CreateSpan` method creates a new span and returns an updated context along with the span object. This is the primary way to instrument your code.

```go
func ProcessOrder(ctx context.Context, tracer *opentelemetry.Tracer) {
    // Create a new span for this operation
    ctx, span := tracer.CreateSpan(ctx, map[string]string{
        "order_id": "12345",
        "customer": "john@example.com",
        "amount":   "99.99",
    })
    defer span.End() // Always end the span when done
    
    // Your business logic here
    processPayment(ctx, tracer)
    updateInventory(ctx, tracer)
}

func processPayment(ctx context.Context, tracer *opentelemetry.Tracer) {
    // Create a nested span
    ctx, span := tracer.CreateSpan(ctx, map[string]string{
        "payment_method": "credit_card",
        "gateway":        "stripe",
    })
    defer span.End()
    
    // Payment processing logic
}
```

**Key Points:**
- `CreateSpan` automatically names the span based on the calling function
- The span name will be in the format `package.FunctionName`
- Parameters are added as attributes with the prefix `param.`
- Always call `span.End()` when the operation completes (use `defer` for automatic cleanup)
- Pass the updated context to child functions to maintain the trace hierarchy

### Using SpanFromContext

Use `SpanFromContext` to retrieve the current span from a context. This is useful when you need to add additional attributes or events to an existing span.

```go
import (
    otelTrace "go.opentelemetry.io/otel/trace"
    "go.opentelemetry.io/otel/attribute"
)

func ProcessItem(ctx context.Context, itemID string) {
    // Get the current span from context
    span := otelTrace.SpanFromContext(ctx)
    
    // Add additional attributes to the existing span
    span.SetAttributes(
        attribute.String("item_id", itemID),
        attribute.Int("processing_stage", 2),
    )
    
    // Add an event to the span
    span.AddEvent("Item validation completed")
    
    // Record an error if something goes wrong
    if err := validateItem(itemID); err != nil {
        span.RecordError(err)
        span.SetAttributes(attribute.Bool("validation_failed", true))
        return
    }
    
    // Your business logic here
}
```

**Key Points:**
- `SpanFromContext` returns the span without creating a new one
- Use it when you want to add information to the current span
- Perfect for adding dynamic attributes based on runtime conditions
- Can be used to record errors and events

### Continuing Traces Across Services

When receiving requests from other services, use `ContinueWithTrace` to continue the distributed trace:

```go
func HandleIncomingRequest(traceID string, tracer *opentelemetry.Tracer) error {
    // Create a base context
    ctx := context.Background()
    
    // Continue the trace from the incoming trace ID
    ctx, err := tracer.ContinueWithTrace(ctx, traceID)
    if err != nil {
        return fmt.Errorf("failed to continue trace: %w", err)
    }
    
    // Now create spans that will be part of the distributed trace
    ctx, span := tracer.CreateSpan(ctx, map[string]string{
        "request_id": "req-789",
    })
    defer span.End()
    
    // Process the request
    return processRequest(ctx, tracer)
}
```

### Complete Example

Here's a complete example showing a typical use case:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"
    
    "github.com/uug-ai/tracer/pkg/opentelemetry"
    otelTrace "go.opentelemetry.io/otel/trace"
    "go.opentelemetry.io/otel/attribute"
)

func main() {
    // Initialize tracer
    tracer, _ := opentelemetry.NewTracer("order-service")
    tracer.Connect()
    
    // Create root context
    ctx := context.Background()
    
    // Process an order
    ProcessOrder(ctx, tracer, "order-123")
}

func ProcessOrder(ctx context.Context, tracer *opentelemetry.Tracer, orderID string) {
    // Create a span for the entire order processing
    ctx, span := tracer.CreateSpan(ctx, map[string]string{
        "order_id": orderID,
        "action":   "process_order",
    })
    defer span.End()
    
    // Add dynamic attributes using SpanFromContext
    currentSpan := otelTrace.SpanFromContext(ctx)
    currentSpan.AddEvent("Order processing started")
    
    // Validate order
    if err := ValidateOrder(ctx, tracer, orderID); err != nil {
        currentSpan.RecordError(err)
        currentSpan.SetAttributes(attribute.Bool("order_valid", false))
        log.Println("Order validation failed: ", err)
        return
    }
    
    currentSpan.SetAttributes(attribute.Bool("order_valid", true))
    
    // Process payment
    ProcessPayment(ctx, tracer, orderID)
    
    // Update inventory
    UpdateInventory(ctx, tracer, orderID)
    
    currentSpan.AddEvent("Order processing completed")
    log.Println("Order processed successfully")
}

func ValidateOrder(ctx context.Context, tracer *opentelemetry.Tracer, orderID string) error {
    ctx, span := tracer.CreateSpan(ctx, map[string]string{
        "order_id": orderID,
    })
    defer span.End()
    
    // Simulate validation
    time.Sleep(10 * time.Millisecond)
    
    // Add validation result
    span := otelTrace.SpanFromContext(ctx)
    span.SetAttributes(
        attribute.String("validation_status", "passed"),
        attribute.Int("validation_checks", 5),
    )
    
    return nil
}

func ProcessPayment(ctx context.Context, tracer *opentelemetry.Tracer, orderID string) {
    ctx, span := tracer.CreateSpan(ctx, map[string]string{
        "order_id":       orderID,
        "payment_method": "credit_card",
    })
    defer span.End()
    
    // Simulate payment processing
    time.Sleep(50 * time.Millisecond)
    
    span := otelTrace.SpanFromContext(ctx)
    span.SetAttributes(
        attribute.Float64("amount", 99.99),
        attribute.String("currency", "USD"),
        attribute.String("transaction_id", "txn-456"),
    )
}

func UpdateInventory(ctx context.Context, tracer *opentelemetry.Tracer, orderID string) {
    ctx, span := tracer.CreateSpan(ctx, map[string]string{
        "order_id": orderID,
    })
    defer span.End()
    
    // Simulate inventory update
    time.Sleep(20 * time.Millisecond)
    
    span := otelTrace.SpanFromContext(ctx)
    span.SetAttributes(
        attribute.Int("items_updated", 3),
        attribute.String("warehouse", "warehouse-01"),
    )
}
```

## API Reference

### TracerInterface

The core interface that all tracer implementations must satisfy:

```go
type TracerInterface interface {
    Connect() error
    CreateSpan(ctx context.Context, parameters map[string]string) (context.Context, any)
    ContinueWithTrace(ctx context.Context, traceID string) (context.Context, error)
    Audit(parameters map[string]string)
}
```

### OpenTelemetry Tracer Methods

#### `NewTracer(serviceName string) (*Tracer, error)`
Creates a new OpenTelemetry tracer instance.

#### `Connect() error`
Connects to the OTLP endpoint specified in `OTEL_EXPORTER_OTLP_ENDPOINT`.

#### `CreateSpan(ctx context.Context, parameters map[string]string) (context.Context, otelTrace.Span)`
Creates a new span with automatic naming based on the calling function. Returns updated context and span.

**Parameters:**
- `ctx`: Parent context
- `parameters`: Key-value pairs to add as span attributes

**Returns:**
- Updated context with the new span
- The created span object

#### `ContinueWithTrace(ctx context.Context, traceID string) (context.Context, error)`
Continues a trace from a remote trace ID for distributed tracing.

**Parameters:**
- `ctx`: Base context
- `traceID`: Hex-encoded trace ID from upstream service

**Returns:**
- Context with remote span context
- Error if the trace ID is invalid

#### `Audit(parameters map[string]string)`
Logs audit information (currently a no-op, but can be extended).

## Best Practices

1. **Always End Spans**: Use `defer span.End()` immediately after creating a span
2. **Pass Context**: Always pass the updated context to child functions
3. **Add Meaningful Attributes**: Include relevant business context in span attributes
4. **Handle Errors**: Use `span.RecordError(err)` to record errors
5. **Use Events**: Add events to mark important milestones within a span
6. **Avoid Over-Instrumentation**: Don't create spans for trivial operations
7. **Context Timeout**: Ensure your context has appropriate timeout settings

## OpenTelemetry Collector Setup

To receive traces, you'll need an OpenTelemetry Collector or compatible backend:

```yaml
# docker-compose.yml example
version: '3'
services:
  otel-collector:
    image: otel/opentelemetry-collector:latest
    command: ["--config=/etc/otel-collector-config.yaml"]
    volumes:
      - ./otel-collector-config.yaml:/etc/otel-collector-config.yaml
    ports:
      - "4318:4318"  # OTLP HTTP receiver
```

## Troubleshooting

**Tracer not connecting:**
- Verify `OTEL_EXPORTER_OTLP_ENDPOINT` is set correctly
- Check if the collector is running and accessible
- Ensure the endpoint URL scheme matches (http:// vs https://)

**Spans not appearing:**
- Make sure you're calling `span.End()`
- Check if the collector is properly configured
- Verify network connectivity to the collector

**Context propagation issues:**
- Ensure you're passing the updated context from `CreateSpan`
- Verify the context isn't being replaced with a new background context
