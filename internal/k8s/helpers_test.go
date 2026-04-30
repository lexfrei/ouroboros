package k8s_test

import (
	"context"
	"testing"
)

func newCanceledContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	return ctx, func() {}
}
