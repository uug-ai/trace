package pkg

import (
	"context"
)

type TracerInterface interface {
	Initialize() error
	Connect() error
	CreateSpan(ctx context.Context, parameters map[string]string) (context.Context, any)
	ContinueWithTrace(ctx context.Context, traceID string) (context.Context, error)
	Audit(parameters map[string]string)
}
