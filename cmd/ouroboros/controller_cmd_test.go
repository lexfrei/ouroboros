package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lexfrei/ouroboros/internal/config"
)

const (
	releaseNS    = "ouroboros"
	recordsNS    = "dns-records"
	proxyName    = "ouroboros-proxy"
	v4ClusterIP  = "10.42.0.7"
	v6ClusterIP  = "fd00::7"
	testInstance = "myrelease"
)

func newProxyService(clusterIP string, clusterIPs ...string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: proxyName, Namespace: releaseNS},
		Spec:       corev1.ServiceSpec{ClusterIP: clusterIP, ClusterIPs: clusterIPs},
	}
}

func staticNamespace(ns string) func() (string, error) {
	return func() (string, error) { return ns, nil }
}

func staticInstance(value string) func() string {
	return func() string { return value }
}

// TestResolveExternalDNSPlan_DualStack_PassesAllTargets covers the
// blocker where buildExternalDNSReconcile used the singular
// ResolveProxyClusterIP and silently lost the AAAA record on dual-stack
// clusters even though README and Build() promise dual-stack support.
func TestResolveExternalDNSPlan_DualStack_PassesAllTargets(t *testing.T) {
	t.Parallel()

	core := fake.NewSimpleClientset(newProxyService(v4ClusterIP, v4ClusterIP, v6ClusterIP))
	cfg := &config.ControllerConfig{
		Mode:                    config.ModeExternalDNS,
		ExternalDNSProxyService: proxyName,
		ExternalDNSRecordTTL:    60,
	}

	plan, err := resolveExternalDNSPlan(t.Context(), core, cfg,
		staticNamespace(releaseNS), staticInstance(testInstance))
	if err != nil {
		t.Fatalf("resolveExternalDNSPlan: %v", err)
	}

	if len(plan.Targets) != 2 {
		t.Fatalf("got %d targets, want 2 (A + AAAA)", len(plan.Targets))
	}

	if plan.Targets[0] != v4ClusterIP || plan.Targets[1] != v6ClusterIP {
		t.Fatalf("got targets %v, want [%s %s]", plan.Targets, v4ClusterIP, v6ClusterIP)
	}
}

// TestResolveExternalDNSPlan_NamespaceOverride_ServiceLookupStaysInPodNamespace
// covers the blocker where externalDns.namespace=other made the controller
// look up the proxy Service in 'other' instead of the release namespace,
// where the chart actually renders it. Service lookup MUST stay in the
// pod's own namespace; only DNSEndpoint emission honours the override.
func TestResolveExternalDNSPlan_NamespaceOverride_ServiceLookupStaysInPodNamespace(t *testing.T) {
	t.Parallel()

	// Service lives in the release namespace; nothing exists in recordsNS.
	core := fake.NewSimpleClientset(newProxyService(v4ClusterIP))
	cfg := &config.ControllerConfig{
		Mode:                    config.ModeExternalDNS,
		ExternalDNSNamespace:    recordsNS,
		ExternalDNSProxyService: proxyName,
		ExternalDNSRecordTTL:    60,
	}

	plan, err := resolveExternalDNSPlan(t.Context(), core, cfg,
		staticNamespace(releaseNS), staticInstance(testInstance))
	if err != nil {
		t.Fatalf("resolveExternalDNSPlan: %v", err)
	}

	if plan.RecordsNamespace != recordsNS {
		t.Errorf("RecordsNamespace = %q, want %q", plan.RecordsNamespace, recordsNS)
	}

	if plan.ServiceNamespace != releaseNS {
		t.Errorf("ServiceNamespace = %q, want %q (must follow pod namespace, not the override)",
			plan.ServiceNamespace, releaseNS)
	}

	if len(plan.Targets) != 1 || plan.Targets[0] != v4ClusterIP {
		t.Errorf("Targets = %v, want [%s] (Service lookup must succeed in pod namespace)",
			plan.Targets, v4ClusterIP)
	}
}

// TestResolveExternalDNSPlan_RejectsMissingInstance covers the blocker
// where instanceName() silently fell back to the static string
// "ouroboros". Two releases sharing that label would prune each other's
// records — the failure mode the ownership labels exist to prevent.
func TestResolveExternalDNSPlan_RejectsMissingInstance(t *testing.T) {
	t.Parallel()

	core := fake.NewSimpleClientset(newProxyService(v4ClusterIP))
	cfg := &config.ControllerConfig{
		Mode:                    config.ModeExternalDNS,
		ExternalDNSProxyService: proxyName,
		ExternalDNSRecordTTL:    60,
	}

	_, err := resolveExternalDNSPlan(t.Context(), core, cfg,
		staticNamespace(releaseNS), staticInstance(""))
	if err == nil {
		t.Fatal("resolveExternalDNSPlan: missing OUROBOROS_INSTANCE must be a hard error")
	}

	if !strings.Contains(err.Error(), envInstance) {
		t.Fatalf("error %q does not mention the env var name", err.Error())
	}
}

// TestResolveExternalDNSPlan_ProxyIPOverride_SkipsServiceLookup checks
// that the operator-supplied proxy IP wins outright and the API call is
// not made.
func TestResolveExternalDNSPlan_ProxyIPOverride_SkipsServiceLookup(t *testing.T) {
	t.Parallel()

	// No Service in the fake — if the resolver tries to look one up, the
	// test fails. The override must short-circuit cleanly.
	core := fake.NewSimpleClientset()
	cfg := &config.ControllerConfig{
		Mode:                 config.ModeExternalDNS,
		ExternalDNSProxyIP:   v4ClusterIP,
		ExternalDNSRecordTTL: 60,
	}

	plan, err := resolveExternalDNSPlan(t.Context(), core, cfg,
		staticNamespace(releaseNS), staticInstance(testInstance))
	if err != nil {
		t.Fatalf("resolveExternalDNSPlan: %v", err)
	}

	if len(plan.Targets) != 1 || plan.Targets[0] != v4ClusterIP {
		t.Fatalf("got targets %v, want [%s] (override must win)", plan.Targets, v4ClusterIP)
	}
}

// TestResolveExternalDNSPlan_ServiceMissing_FailsClearly verifies that an
// unresolvable proxy Service produces a wrapped error mentioning the
// namespace and Service name so the operator can debug RBAC/typos.
func TestResolveExternalDNSPlan_ServiceMissing_FailsClearly(t *testing.T) {
	t.Parallel()

	core := fake.NewSimpleClientset()
	cfg := &config.ControllerConfig{
		Mode:                    config.ModeExternalDNS,
		ExternalDNSProxyService: proxyName,
		ExternalDNSRecordTTL:    60,
	}

	_, err := resolveExternalDNSPlan(t.Context(), core, cfg,
		staticNamespace(releaseNS), staticInstance(testInstance))
	if err == nil {
		t.Fatal("resolveExternalDNSPlan: missing Service must produce a hard error")
	}

	if !strings.Contains(err.Error(), proxyName) {
		t.Fatalf("error %q does not mention the Service name", err.Error())
	}
}

// TestResolveExternalDNSPlan_PodNamespaceUnreadable_Fails asserts the
// plan resolution fails fast when the downward API file is unreadable
// (e.g. running outside a cluster without an explicit namespace), rather
// than picking a confusing default.
func TestResolveExternalDNSPlan_PodNamespaceUnreadable_Fails(t *testing.T) {
	t.Parallel()

	core := fake.NewSimpleClientset(newProxyService(v4ClusterIP))
	cfg := &config.ControllerConfig{
		Mode:                    config.ModeExternalDNS,
		ExternalDNSProxyService: proxyName,
		ExternalDNSRecordTTL:    60,
	}

	failingNamespace := func() (string, error) {
		return "", errFakeNamespace
	}

	_, err := resolveExternalDNSPlan(t.Context(), core, cfg, failingNamespace, staticInstance(testInstance))
	if err == nil {
		t.Fatal("resolveExternalDNSPlan: unreadable pod namespace must produce an error")
	}
}
