package controller

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

const watchErrorTestKindGateway = "Gateway"

var (
	errSyntheticWatch = errors.New("synthetic ListAndWatch failure")
	errSyntheticBoom  = errors.New("boom")
)

// watchErrorHandler is package-private. Direct invocation pins the contract
// (Warn line tagged with kind+error) without standing up a fake apiserver
// that organically drops a watch — fakes don't trigger the path, and the
// next time someone tweaks the kind argument by accident the slog text
// asserted here will be the only thing catching it.

func TestWatchErrorHandler_EmitsWarnWithKindAndError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	c := &Controller{log: logger}

	handler := c.watchErrorHandler("Ingress")

	handler(nil, errSyntheticWatch)

	out := buf.String()

	if !strings.Contains(out, `level=WARN`) {
		t.Errorf("watch error handler did not emit Warn: %q", out)
	}

	if !strings.Contains(out, `msg="watch error"`) {
		t.Errorf("watch error handler missing canonical msg: %q", out)
	}

	if !strings.Contains(out, "kind=Ingress") {
		t.Errorf("watch error handler did not tag kind: %q", out)
	}

	if !strings.Contains(out, "synthetic ListAndWatch failure") {
		t.Errorf("watch error handler dropped the underlying error text: %q", out)
	}
}

func TestWatchErrorHandler_DistinctKindPerInformer(t *testing.T) {
	t.Parallel()

	// All three informers (Ingress, Gateway, HTTPRoute) share the same
	// handler builder. A swap in registerCoreInformer / registerGateway-
	// Informers (e.g. accidentally tagging the HTTPRoute handler "Gateway")
	// would compile clean and produce mis-attributed warns. Pin that the
	// handler returns distinct closures keyed by the kind string.
	c := &Controller{log: slog.New(slog.DiscardHandler)}

	for _, kind := range []string{"Ingress", watchErrorTestKindGateway, "HTTPRoute"} {
		var buf bytes.Buffer
		c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

		handler := c.watchErrorHandler(kind)
		handler(nil, errSyntheticBoom)

		if !strings.Contains(buf.String(), "kind="+kind) {
			t.Errorf("watch error for kind %q tagged something else: %q", kind, buf.String())
		}
	}
}
