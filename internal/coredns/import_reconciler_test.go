package coredns_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lexfrei/ouroboros/internal/coredns"
)

const (
	importNS  = "kube-system"
	importCM  = "coredns-custom"
	importKey = "ouroboros.override"
)

func newImportConfigMap(content string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      importCM,
			Namespace: importNS,
		},
	}

	if content != "" {
		cm.Data = map[string]string{importKey: content}
	}

	return cm
}

func mustGetImportData(t *testing.T, client *fake.Clientset) string {
	t.Helper()

	cm, err := client.CoreV1().ConfigMaps(importNS).Get(t.Context(), importCM, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}

	return cm.Data[importKey]
}

func TestImportReconciler_AddsContentOnFirstRun(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newImportConfigMap(""))
	rec := coredns.NewImportReconciler(client, importNS, importCM, importKey, defaultTarget, nil)

	changed, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !changed {
		t.Error("changed = false, want true on first reconcile")
	}

	got := mustGetImportData(t, client)

	want := "rewrite name foo.example.com " + defaultTarget
	if !strings.Contains(got, want) {
		t.Errorf("rewrite missing from ConfigMap data:\nwant substring: %s\ngot:\n%s", want, got)
	}

	if strings.Contains(got, ".:53") {
		t.Errorf("import data must NOT contain a server-block wrapper, got:\n%s", got)
	}
}

func TestImportReconciler_DoesNotUpdateWhenIdentical(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newImportConfigMap(""))
	rec := coredns.NewImportReconciler(client, importNS, importCM, importKey, defaultTarget, nil)

	_, firstErr := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if firstErr != nil {
		t.Fatalf("first Reconcile: %v", firstErr)
	}

	var updateCalls atomic.Int32

	client.PrependReactor("update", "configmaps", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		updateCalls.Add(1)

		return false, nil, nil
	})

	changed, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	if changed {
		t.Error("changed = true on identical reconcile, want false")
	}

	if got := updateCalls.Load(); got != 0 {
		t.Errorf("ConfigMap.Update called %d times on no-op reconcile, want 0", got)
	}
}

func TestImportReconciler_RemovesEntriesWhenHostsEmpty(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newImportConfigMap(""))
	rec := coredns.NewImportReconciler(client, importNS, importCM, importKey, defaultTarget, nil)

	_, seedErr := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if seedErr != nil {
		t.Fatalf("seed Reconcile: %v", seedErr)
	}

	changed, err := rec.Reconcile(t.Context(), nil)
	if err != nil {
		t.Fatalf("clear Reconcile: %v", err)
	}

	if !changed {
		t.Error("changed = false when removing entries, want true")
	}

	got := mustGetImportData(t, client)
	if strings.Contains(got, "rewrite name foo.example.com") {
		t.Errorf("rewrite line still present after empty-hosts reconcile:\n%s", got)
	}
}

func TestImportReconciler_MissingConfigMap_ReturnsError(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	rec := coredns.NewImportReconciler(client, importNS, importCM, importKey, defaultTarget, nil)

	_, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err == nil {
		t.Fatal("expected error when ConfigMap is missing (chart owns creation)")
	}
}

func TestImportReconciler_RetriesOnConflict(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newImportConfigMap(""))

	var attempts atomic.Int32

	client.PrependReactor("update", "configmaps", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		if attempts.Add(1) == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "", Resource: "configmaps"},
				importCM,
				errSyntheticConflict,
			)
		}

		return false, nil, nil
	})

	rec := coredns.NewImportReconciler(client, importNS, importCM, importKey, defaultTarget, nil)

	changed, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !changed {
		t.Error("changed = false after retry, want true")
	}

	if got := attempts.Load(); got < 2 {
		t.Errorf("Update attempts = %d, want at least 2 (one conflict + retry)", got)
	}
}

func TestImportReconciler_PropagatesContextCancel(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newImportConfigMap(""))
	rec := coredns.NewImportReconciler(client, importNS, importCM, importKey, defaultTarget, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := rec.Reconcile(ctx, []string{"foo.example.com"})
	if err == nil {
		t.Fatal("expected error when context is canceled, got nil")
	}
}

func TestBuildImportSnippet_NormalisesAndSorts(t *testing.T) {
	t.Parallel()

	got, err := coredns.BuildImportSnippet(
		[]string{"B.example.com", "a.example.com", "B.example.com", "  c.example.com  "},
		defaultTarget,
	)
	if err != nil {
		t.Fatalf("BuildImportSnippet: %v", err)
	}

	want := "rewrite name a.example.com " + defaultTarget + "\n" +
		"rewrite name b.example.com " + defaultTarget + "\n" +
		"rewrite name c.example.com " + defaultTarget + "\n"
	if got != want {
		t.Errorf("BuildImportSnippet:\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestBuildImportSnippet_DropsWildcards(t *testing.T) {
	t.Parallel()

	got, err := coredns.BuildImportSnippet(
		[]string{"*.example.com", "foo.example.com", "*?.example.com"},
		defaultTarget,
	)
	if err != nil {
		t.Fatalf("BuildImportSnippet: %v", err)
	}

	if strings.Contains(got, "*") {
		t.Errorf("wildcard host leaked into snippet:\n%s", got)
	}

	if !strings.Contains(got, "rewrite name foo.example.com "+defaultTarget) {
		t.Errorf("non-wildcard host missing:\n%s", got)
	}
}

func TestBuildImportSnippet_RequiresTrailingDotInTarget(t *testing.T) {
	t.Parallel()

	_, err := coredns.BuildImportSnippet([]string{"foo.example.com"}, "ouroboros-proxy.svc")
	if err == nil {
		t.Fatal("BuildImportSnippet must reject targets without a trailing dot (rewrite name needs FQDN)")
	}
}

func TestBuildImportSnippet_EmptyHostsReturnsEmpty(t *testing.T) {
	t.Parallel()

	got, err := coredns.BuildImportSnippet(nil, defaultTarget)
	if err != nil {
		t.Fatalf("BuildImportSnippet(nil): %v", err)
	}

	if got != "" {
		t.Errorf("empty host list must yield empty snippet, got:\n%s", got)
	}
}
