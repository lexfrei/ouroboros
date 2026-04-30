package externaldns_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lexfrei/ouroboros/internal/externaldns"
)

var (
	errSyntheticOrphanProbe = errors.New("synthetic orphan probe failure")
	errSyntheticForbidden   = errors.New("no rbac (test)")
)

func captureOrphanLog(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, buf
}

func ownedService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				externaldns.LabelManagedBy: managedByValue,
				externaldns.LabelInstance:  testRelease,
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}
}

func TestWarnOrphans_CrdActive_ServiceOrphansLogged(t *testing.T) {
	t.Parallel()

	core := fake.NewSimpleClientset(ownedService("ouroboros-foo-com"))

	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), core, nil,
		testNamespace, testRelease, "crd", logger)

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("expected WARN when crd is active and Service orphans exist; got: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "kubectl --namespace "+testNamespace+" delete services") {
		t.Fatalf("warning must include the copy-pasteable kubectl delete services command "+
			"with project-style space-separated --namespace flag; got: %s", buf.String())
	}
}

func TestWarnOrphans_NilDynamic_StaysSilent(t *testing.T) {
	t.Parallel()

	// Defence against a future wiring regression: if k8s.Build ever
	// returns a nil dynamic client (today it always wires one), the
	// probe must skip rather than panic.
	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), nil, nil,
		testNamespace, testRelease, "service", logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("nil dynamic client must not produce a warning; got: %s", buf.String())
	}
}

func TestWarnOrphans_DNSEndpointForbidden_StaysSilent(t *testing.T) {
	t.Parallel()

	// In output=service mode the chart correctly does NOT grant
	// dnsendpoints verbs, so the probe's List returns 403. This is
	// the expected production path — not noise to surface, even at
	// Debug. (The asymmetry with the Service-side probe — which only
	// warns at all when there are real orphans — is fine: the CRD's
	// missing RBAC is a deliberate chart minimisation, not a bug.)
	scheme := newDynamicScheme(t)

	gvrToListKind := map[schema.GroupVersionResource]string{
		externaldns.GVR: "DNSEndpointList",
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
	dyn.PrependReactor("list", "dnsendpoints", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "externaldns.k8s.io", Resource: "dnsendpoints"},
			"", errSyntheticForbidden)
	})

	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), nil, dyn,
		testNamespace, testRelease, "service", logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("403 on DNSEndpoint list must not surface (chart correctly skips this RBAC); got: %s", buf.String())
	}

	if strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("403 is the expected path — must not even surface at DEBUG; got: %s", buf.String())
	}
}

func TestWarnOrphans_DNSEndpointCRDMissing_StaysSilent(t *testing.T) {
	t.Parallel()

	// External-dns operator who wants service-output mode but never
	// installed the DNSEndpoint CRD. List returns IsNotFound. Probe
	// must skip — no orphans are possible if the kind doesn't exist.
	scheme := newDynamicScheme(t)

	gvrToListKind := map[schema.GroupVersionResource]string{
		externaldns.GVR: "DNSEndpointList",
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
	dyn.PrependReactor("list", "dnsendpoints", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "externaldns.k8s.io", Resource: "dnsendpoints"}, "")
	})

	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), nil, dyn,
		testNamespace, testRelease, "service", logger)

	if strings.Contains(buf.String(), "level=WARN") || strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("missing-CRD must produce no log output; got: %s", buf.String())
	}
}

func TestWarnOrphans_DNSEndpointActive_LogsWarn(t *testing.T) {
	t.Parallel()

	// The output=crd → service flip leaves DNSEndpoint orphans behind.
	// In service mode the probe runs against the dynamic client and
	// must Warn with a copy-pasteable cleanup command.
	scheme := newDynamicScheme(t)

	orphan := &unstructured.Unstructured{}
	orphan.SetAPIVersion(externaldns.APIVersion)
	orphan.SetKind(externaldns.Kind)
	orphan.SetName("ouroboros-orphan-com")
	orphan.SetNamespace(testNamespace)
	orphan.SetLabels(map[string]string{
		externaldns.LabelManagedBy: managedByValue,
		externaldns.LabelInstance:  testRelease,
	})

	gvrToListKind := map[schema.GroupVersionResource]string{
		externaldns.GVR: "DNSEndpointList",
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, orphan)

	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), nil, dyn,
		testNamespace, testRelease, "service", logger)

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("expected WARN when service is active and DNSEndpoint orphans exist; got: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "kubectl --namespace "+testNamespace+" delete dnsendpoints") {
		t.Fatalf("warning must include the copy-pasteable kubectl delete dnsendpoints command "+
			"with project-style space-separated --namespace flag; got: %s", buf.String())
	}
}

func TestWarnOrphans_NoOrphans_StaysSilent(t *testing.T) {
	t.Parallel()

	core := fake.NewSimpleClientset()

	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), core, nil,
		testNamespace, testRelease, "crd", logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("clean cluster must not produce orphan warning; got: %s", buf.String())
	}
}

func TestWarnOrphans_TransientError_DowngradedToDebug(t *testing.T) {
	t.Parallel()

	core := fake.NewSimpleClientset()
	core.PrependReactor("list", "services", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errSyntheticOrphanProbe
	})

	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), core, nil,
		testNamespace, testRelease, "crd", logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("transient probe error must not surface as WARN; got: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("transient error should still surface at DEBUG: %s", buf.String())
	}
}

func TestWarnOrphans_ForbiddenError_StaysSilent(t *testing.T) {
	t.Parallel()

	// 403 is the expected outcome when the other kind's RBAC was never
	// granted (chart correctly minimal). Must not surface.
	core := fake.NewSimpleClientset()
	core.PrependReactor("list", "services", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("services"), "", errSyntheticForbidden)
	})

	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), core, nil,
		testNamespace, testRelease, "crd", logger)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("403 must not surface (chart correctly skipped this RBAC); got: %s", buf.String())
	}
	// Forbidden is not even Debug-worthy noise — the chart explicitly
	// avoids granting the other kind's verbs, so 403 is expected.
}

func TestWarnOrphans_NilLoggerSafe(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil logger must not panic, got: %v", r)
		}
	}()

	core := fake.NewSimpleClientset(ownedService("svc"))

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), core, nil,
		testNamespace, testRelease, "crd", nil)
}

func TestWarnOrphans_UnknownActiveOutput_NoOp(t *testing.T) {
	t.Parallel()

	core := fake.NewSimpleClientset(ownedService("svc"))

	logger, buf := captureOrphanLog(t)

	externaldns.WarnIfOtherOutputHasOrphans(t.Context(), core, nil,
		testNamespace, testRelease, "etc-hosts", logger)

	if buf.Len() != 0 {
		t.Fatalf("unrecognised activeOutput should be a no-op; got: %s", buf.String())
	}
}

// Just to silence unused-warning if test loaders are picky.
var _ = unstructured.Unstructured{}
