package omp

import (
	"context"
	"errors"
)

// classifiedError preserves the existing error while exposing safe diagnostic metadata.
type classifiedError struct {
	kind string
	err  error
}

func (e *classifiedError) Error() string { return e.err.Error() }
func (e *classifiedError) Unwrap() error { return e.err }

// ClassifyError returns a fixed reason, never native diagnostics or raw error text.
func ClassifyError(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var classified *classifiedError
	if errors.As(err, &classified) {
		return classified.kind
	}
	if errors.Is(err, errProtocol) {
		return "protocol"
	}
	return "unknown"
}
