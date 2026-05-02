package coredns_test

import (
	"bytes"
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

const reloadProbeNS = "kube-system"

func TestWarnIfCorednsReloadMissing_NoConfigMap_StaysSilent(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	buf := bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	coredns.WarnIfCorednsReloadMissing(t.Context(), client, reloadProbeNS, "coredns", "Corefile", logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("missing CoreDNS ConfigMap should not produce a Warn (probe is best-effort); got: %s", buf.String())
	}
}

func TestWarnIfCorednsReloadMissing_ForbiddenGet_DowngradedToDebug(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "configmaps", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, &kubeForbiddenError{}
	})

	buf := bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	coredns.WarnIfCorednsReloadMissing(t.Context(), client, reloadProbeNS, "coredns", "Corefile", logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("RBAC Forbidden must downgrade to Debug, not Warn; got: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("RBAC Forbidden should still log a Debug breadcrumb; got: %s", buf.String())
	}
}

func TestWarnIfCorednsReloadMissing_ReloadPresent_StaysSilent(t *testing.T) {
	t.Parallel()

	corefileWithReload := `.:53 {
    errors
    health
    kubernetes cluster.local in-addr.arpa ip6.arpa
    forward . /etc/resolv.conf
    cache 30
    loop
    reload
    loadbalance
}
`
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: reloadProbeNS},
		Data:       map[string]string{"Corefile": corefileWithReload},
	})

	buf := bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	coredns.WarnIfCorednsReloadMissing(t.Context(), client, reloadProbeNS, "coredns", "Corefile", logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("Corefile with reload plugin must not Warn; got: %s", buf.String())
	}
}

func TestWarnIfCorednsReloadMissing_ReloadAbsent_LogsWarning(t *testing.T) {
	t.Parallel()

	corefileNoReload := `.:53 {
    errors
    kubernetes cluster.local in-addr.arpa ip6.arpa
    forward . /etc/resolv.conf
    cache 30
    loop
    loadbalance
    import /etc/coredns/custom/*.override
}
`
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: reloadProbeNS},
		Data:       map[string]string{"Corefile": corefileNoReload},
	})

	buf := bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	coredns.WarnIfCorednsReloadMissing(t.Context(), client, reloadProbeNS, "coredns", "Corefile", logger)

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("Corefile without reload plugin must Warn; got: %s", buf.String())
	}

	for _, hint := range []string{"reload", "restart"} {
		if !strings.Contains(strings.ToLower(buf.String()), hint) {
			t.Errorf("Warn line should mention %q to point operators at the fix; got: %s", hint, buf.String())
		}
	}
}

func TestWarnIfCorednsReloadMissing_NilLoggerSafe(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	// Must not panic on nil logger.
	coredns.WarnIfCorednsReloadMissing(t.Context(), client, reloadProbeNS, "coredns", "Corefile", nil)
}

// kubeForbiddenError satisfies apierrors.IsForbidden so the probe takes the
// downgrade-to-Debug branch.
type kubeForbiddenError struct{}

func (e *kubeForbiddenError) Error() string { return "forbidden" }

func (e *kubeForbiddenError) Status() metav1.Status {
	return metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    403,
		Reason:  metav1.StatusReasonForbidden,
		Message: "forbidden",
		Details: &metav1.StatusDetails{
			Group: "",
			Kind:  "configmaps",
		},
	}
}
