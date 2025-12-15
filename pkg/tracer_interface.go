package pkg

import (
	"context"

	"github.com/sirupsen/logrus"
)

type TracerInterface interface {
	Initialize(logger *logrus.Logger) error
	Connect(logger *logrus.Logger) error
	CreateSpan(ctx context.Context, parameters map[string]string) (context.Context, any)
	ContinueWithTrace(ctx context.Context, traceID string) context.Context
	Audit(parameters map[string]string)
}
