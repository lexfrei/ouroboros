package k8s_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lexfrei/ouroboros/internal/k8s"
)

const (
	proxyNS   = "ouroboros"
	proxyName = "ouroboros-proxy"
)

func newService(clusterIP string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyName,
			Namespace: proxyNS,
		},
		Spec: corev1.ServiceSpec{ClusterIP: clusterIP},
	}
}

func TestResolveProxyClusterIP_Found(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newService("10.42.0.7"))

	got, err := k8s.ResolveProxyClusterIP(t.Context(), client, proxyNS, proxyName)
	if err != nil {
		t.Fatalf("ResolveProxyClusterIP: %v", err)
	}

	if got != "10.42.0.7" {
		t.Fatalf("got ClusterIP %q, want %q", got, "10.42.0.7")
	}
}

func TestResolveProxyClusterIP_MissingService_ReturnsError(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()

	_, err := k8s.ResolveProxyClusterIP(t.Context(), client, proxyNS, proxyName)
	if err == nil {
		t.Fatal("ResolveProxyClusterIP: want error for missing Service, got nil")
	}

	// The caller (controller startup) needs the operator-friendly hint —
	// "service ouroboros/ouroboros-proxy not found" — not just a kube-apiserver
	// 404. Assert both pieces appear in the message.
	msg := err.Error()
	if !strings.Contains(msg, proxyNS) || !strings.Contains(msg, proxyName) {
		t.Fatalf("error %q lacks namespace/name context", msg)
	}
}

func TestResolveProxyClusterIP_HeadlessService_ReturnsError(t *testing.T) {
	t.Parallel()

	// Headless Services advertise ClusterIP "None"; an A-record DNSEndpoint
	// pointed at "None" would push junk to the DNS provider. Refuse it at
	// resolve-time so the controller fails fast, instead of writing bad records.
	client := fake.NewSimpleClientset(newService(corev1.ClusterIPNone))

	_, err := k8s.ResolveProxyClusterIP(t.Context(), client, proxyNS, proxyName)
	if err == nil {
		t.Fatal("ResolveProxyClusterIP: want error for headless Service, got nil")
	}

	if !strings.Contains(err.Error(), "headless") {
		t.Fatalf("error %q does not mention 'headless'", err.Error())
	}
}

func TestResolveProxyClusterIP_EmptyClusterIP_ReturnsError(t *testing.T) {
	t.Parallel()

	// A Service that has not yet been allocated a ClusterIP returns "" from
	// .spec.clusterIP. We must not write an empty A-target.
	client := fake.NewSimpleClientset(newService(""))

	_, err := k8s.ResolveProxyClusterIP(t.Context(), client, proxyNS, proxyName)
	if err == nil {
		t.Fatal("ResolveProxyClusterIP: want error for empty ClusterIP, got nil")
	}
}

func TestResolveProxyClusterIP_RejectsCanceledContext(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newService("10.42.0.7"))
	ctx, cancel := newCanceledContext(t)
	defer cancel()

	_, err := k8s.ResolveProxyClusterIP(ctx, client, proxyNS, proxyName)
	if err == nil {
		t.Fatal("ResolveProxyClusterIP: want error for canceled context, got nil")
	}
}
