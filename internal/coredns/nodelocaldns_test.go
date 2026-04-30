package coredns_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lexfrei/ouroboros/internal/coredns"
)

func captureWarnLog(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, buf
}

func TestWarnIfNodeLocalDNSDetected_NoConfigMap_StaysSilent(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	logger, buf := captureWarnLog(t)

	coredns.WarnIfNodeLocalDNSDetected(t.Context(), client, logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("clean cluster (no node-local-dns) must not log a warning; got: %s", buf.String())
	}
}

func TestWarnIfNodeLocalDNSDetected_ConfigMapPresent_LogsWarning(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "node-local-dns", Namespace: "kube-system"},
		Data:       map[string]string{"Corefile": "(...)"},
	})
	logger, buf := captureWarnLog(t)

	coredns.WarnIfNodeLocalDNSDetected(t.Context(), client, logger)

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("expected WARN log when node-local-dns is present; got: %s", buf.String())
	}

	// The remediation hint must be in the message — operators reading this
	// log line need to know what to do without consulting the README.
	for _, hint := range []string{"node-local-dns", "external-dns", "rewrite"} {
		if !strings.Contains(buf.String(), hint) {
			t.Fatalf("warning lacks operator hint %q: %s", hint, buf.String())
		}
	}
}

func TestWarnIfNodeLocalDNSDetected_TransientError_DowngradedToDebug(t *testing.T) {
	t.Parallel()

	// A flaky API server during startup must not produce a misleading
	// 'node-local-dns detected' WARN — the probe should silently fall back
	// to Debug so operators don't chase a phantom misconfiguration.
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "configmaps", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errSyntheticAPIServer
	})

	logger, buf := captureWarnLog(t)

	coredns.WarnIfNodeLocalDNSDetected(t.Context(), client, logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("transient probe error must not produce a WARN: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("transient probe error should still surface at DEBUG: %s", buf.String())
	}
}

func TestWarnIfNodeLocalDNSDetected_NilLoggerSafe(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil logger must not panic, got: %v", r)
		}
	}()

	client := fake.NewSimpleClientset()
	coredns.WarnIfNodeLocalDNSDetected(t.Context(), client, nil)
}

func TestWarnIfNodeLocalDNSDetected_EnvOverride_HitsAlternateConfigMap(t *testing.T) {
	// Not parallel — Setenv mutates process-wide state. Operators with
	// node-local-dns deployed in a non-default namespace need this knob.
	t.Setenv("OUROBOROS_NODE_LOCAL_DNS_NAMESPACE", "dns-system")
	t.Setenv("OUROBOROS_NODE_LOCAL_DNS_CONFIGMAP", "nodelocaldns")

	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nodelocaldns", Namespace: "dns-system"},
		Data:       map[string]string{"Corefile": "(...)"},
	})
	logger, buf := captureWarnLog(t)

	coredns.WarnIfNodeLocalDNSDetected(t.Context(), client, logger)

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("env override must let the probe find the alternate ConfigMap; got: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "dns-system/nodelocaldns") {
		t.Fatalf("warn line should reference the actual ConfigMap location: %s", buf.String())
	}
}

var errSyntheticAPIServer = errors.New("synthetic apiserver error")
