package agent

import (
	"context"
	"errors"
	"io"

	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// Close releases the resources held by the agent's engine and lifecycle
// hooks. Components that do not implement io.Closer are skipped, and
// close errors are aggregated with errors.Join. Idempotency of each
// component is that component's responsibility. Close is safe on a nil
// receiver and on nil/typed-nil components.
func (a *Agent) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	closeIfCloser(&errs, a.Engine)
	closeSlice(&errs, a.Prepare)
	closeSlice(&errs, a.Observe)
	closeSlice(&errs, a.Referees)
	closeSlice(&errs, a.Commit)
	err := errors.Join(errs...)
	if err != nil {
		attrs := []otellog.KeyValue{
			otellog.String(telemetry.AttrErrorMessage, err.Error()),
		}
		if a.ID != "" {
			attrs = append(attrs, otellog.String(telemetry.AttrAgentID, a.ID))
		}
		telemetry.Error(context.Background(), "agent close failed", attrs...)
	}
	return err
}

func closeSlice[T any](errs *[]error, values []T) {
	for _, value := range values {
		closeIfCloser(errs, value)
	}
}

func closeIfCloser(errs *[]error, value any) {
	if value == nil || isNilInterface(value) {
		return
	}
	closer, ok := value.(io.Closer)
	if !ok {
		return
	}
	if err := closer.Close(); err != nil {
		*errs = append(*errs, err)
	}
}
