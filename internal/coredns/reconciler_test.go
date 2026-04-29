package coredns_test

import (
	"context"
	"errors"
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
	corednsNS   = "kube-system"
	corednsCM   = "coredns"
	corefileKey = "Corefile"
)

var errSyntheticConflict = errors.New("synthetic conflict for test")

func newConfigMap(corefile string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      corednsCM,
			Namespace: corednsNS,
		},
		Data: map[string]string{corefileKey: corefile},
	}
}

func mustGetCorefile(t *testing.T, client *fake.Clientset) string {
	t.Helper()

	cm, err := client.CoreV1().ConfigMaps(corednsNS).Get(t.Context(), corednsCM, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}

	return cm.Data[corefileKey]
}

func TestReconciler_AddsBlockOnFirstRun(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newConfigMap(corefileMinimal))

	rec := coredns.NewReconciler(client, corednsNS, corednsCM, corefileKey, defaultTarget, nil)

	changed, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !changed {
		t.Error("changed = false, want true on first reconcile")
	}

	got := mustGetCorefile(t, client)
	if !strings.Contains(got, "rewrite name foo.example.com "+defaultTarget) {
		t.Errorf("rewrite missing from ConfigMap:\n%s", got)
	}
}

func TestReconciler_DoesNotUpdateWhenIdentical(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newConfigMap(corefileMinimal))
	rec := coredns.NewReconciler(client, corednsNS, corednsCM, corefileKey, defaultTarget, nil)

	_, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
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

func TestReconciler_MissingConfigMap_ReturnsError(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	rec := coredns.NewReconciler(client, corednsNS, corednsCM, corefileKey, defaultTarget, nil)

	_, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err == nil {
		t.Fatal("expected error when ConfigMap is missing")
	}
}

func TestReconciler_MissingCorefileKey_ReturnsError(t *testing.T) {
	t.Parallel()

	cm := newConfigMap(corefileMinimal)
	delete(cm.Data, corefileKey)

	client := fake.NewSimpleClientset(cm)
	rec := coredns.NewReconciler(client, corednsNS, corednsCM, corefileKey, defaultTarget, nil)

	_, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err == nil {
		t.Fatal("expected error when Corefile key is absent")
	}
}

func TestReconciler_RetriesOnConflict(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newConfigMap(corefileMinimal))

	var attempts atomic.Int32

	client.PrependReactor("update", "configmaps", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		if attempts.Add(1) == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "", Resource: "configmaps"},
				corednsCM,
				errSyntheticConflict,
			)
		}

		return false, nil, nil
	})

	rec := coredns.NewReconciler(client, corednsNS, corednsCM, corefileKey, defaultTarget, nil)

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

func TestReconciler_PropagatesContextCancel(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newConfigMap(corefileMinimal))
	rec := coredns.NewReconciler(client, corednsNS, corednsCM, corefileKey, defaultTarget, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := rec.Reconcile(ctx, []string{"foo.example.com"})
	if err == nil {
		t.Fatal("expected error when context is canceled, got nil")
	}
}

func TestReconciler_RemovesBlockWhenHostsEmpty(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(newConfigMap(corefileMinimal))
	rec := coredns.NewReconciler(client, corednsNS, corednsCM, corefileKey, defaultTarget, nil)

	_, err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	changed, err := rec.Reconcile(t.Context(), nil)
	if err != nil {
		t.Fatalf("clear Reconcile: %v", err)
	}

	if !changed {
		t.Error("changed = false when removing block, want true")
	}

	got := mustGetCorefile(t, client)
	if strings.Contains(got, beginMarker) {
		t.Errorf("BEGIN marker still present after empty-hosts reconcile:\n%s", got)
	}
}
