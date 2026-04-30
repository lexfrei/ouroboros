package externaldns_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lexfrei/ouroboros/internal/externaldns"
)

const (
	verbCreate = "create"
	verbDelete = "delete"
	verbUpdate = "update"
	verbPatch  = "patch"
)

var errSyntheticPatch = errors.New("synthetic patch failure")

func dnsEndpointGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "externaldns.k8s.io", Version: "v1alpha1", Kind: externaldns.Kind}
}

func newDynamicScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	// fake.NewSimpleDynamicClientWithCustomListKinds wants the list kind so it
	// can synthesise List calls; we register both the singular and the list
	// shape under the upstream CRD's GroupVersion.
	scheme.AddKnownTypeWithName(dnsEndpointGVK(), &unstructured.Unstructured{})
	listGVK := dnsEndpointGVK()
	listGVK.Kind = "DNSEndpointList"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	return scheme
}

func newFakeDynamic(t *testing.T, seed ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()

	scheme := newDynamicScheme(t)

	gvrToListKind := map[schema.GroupVersionResource]string{
		externaldns.GVR: "DNSEndpointList",
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, seed...)
}

func newReconciler(t *testing.T, client dynamic.Interface) *externaldns.Reconciler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	rec, err := externaldns.NewReconciler(&externaldns.ReconcilerConfig{
		Client:    client,
		Namespace: testNamespace,
		Instance:  testRelease,
		Targets:   []string{v4Target},
		TTL:       60,
		Source:    externaldns.SourceController,
		Log:       logger,
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}

	return rec
}

func listEndpoints(t *testing.T, client *dynamicfake.FakeDynamicClient) []unstructured.Unstructured {
	t.Helper()

	got, err := client.Resource(externaldns.GVR).Namespace(testNamespace).
		List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	return got.Items
}

func TestReconciler_FirstRun_CreatesAllEndpoints(t *testing.T) {
	t.Parallel()

	client := newFakeDynamic(t)
	rec := newReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com", "b.example.com", "c.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := listEndpoints(t, client)
	if len(got) != 3 {
		t.Fatalf("got %d endpoints, want 3", len(got))
	}
}

func TestReconciler_NoDrift_StateRemainsConsistent(t *testing.T) {
	t.Parallel()

	// Equality short-circuit guarantee: the second reconcile produces
	// zero mutating actions when nothing changed. external-dns watches
	// by resourceVersion and re-publishes records on every generation
	// bump, so a no-op Update per resync × N hosts would translate
	// directly to upstream provider churn. The fake client records
	// every dispatched verb — the expected verbs on a clean second
	// pass are 'list' (prune scan) + 'get' (apply pre-check) only.
	client := newFakeDynamic(t)
	rec := newReconciler(t, client)

	hosts := []string{"a.example.com", "b.example.com"}

	firstErr := rec.Reconcile(t.Context(), hosts)
	if firstErr != nil {
		t.Fatalf("first Reconcile: %v", firstErr)
	}

	first := listEndpoints(t, client)
	client.ClearActions()

	secondErr := rec.Reconcile(t.Context(), hosts)
	if secondErr != nil {
		t.Fatalf("second Reconcile: %v", secondErr)
	}

	second := listEndpoints(t, client)

	if len(first) != len(second) {
		t.Fatalf("count drift: %d -> %d", len(first), len(second))
	}

	for _, action := range client.Actions() {
		verb := action.GetVerb()
		if verb == verbCreate || verb == verbUpdate || verb == verbPatch || verb == verbDelete {
			t.Fatalf("second pass produced %s on %s — equality short-circuit should make this a no-op",
				verb, action.GetResource().Resource)
		}
	}
}

func TestReconciler_HostRemoved_DeletesEndpoint(t *testing.T) {
	t.Parallel()

	client := newFakeDynamic(t)
	rec := newReconciler(t, client)

	firstErr := rec.Reconcile(t.Context(), []string{"a.example.com", "b.example.com", "c.example.com"})
	if firstErr != nil {
		t.Fatalf("first Reconcile: %v", firstErr)
	}

	secondErr := rec.Reconcile(t.Context(), []string{"a.example.com", "b.example.com"})
	if secondErr != nil {
		t.Fatalf("second Reconcile: %v", secondErr)
	}

	got := listEndpoints(t, client)
	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2 (c was removed)", len(got))
	}

	for _, item := range got {
		if strings.Contains(item.GetName(), "c-example-com") {
			t.Fatalf("c.example.com still present after host removal: %s", item.GetName())
		}
	}
}

func TestReconciler_HostAdded_CreatesOne_LeavesExistingAlone(t *testing.T) {
	t.Parallel()

	client := newFakeDynamic(t)
	rec := newReconciler(t, client)

	firstErr := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if firstErr != nil {
		t.Fatalf("first Reconcile: %v", firstErr)
	}

	client.ClearActions()

	secondErr := rec.Reconcile(t.Context(), []string{"a.example.com", "b.example.com"})
	if secondErr != nil {
		t.Fatalf("incremental Reconcile: %v", secondErr)
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == verbDelete {
			t.Fatalf("incremental add produced a delete: %v", action)
		}
	}

	got := listEndpoints(t, client)
	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(got))
	}
}

func TestReconciler_OwnershipFilter_LeavesForeignAlone(t *testing.T) {
	t.Parallel()

	// Pre-seed an unrelated DNSEndpoint that someone else owns. Reconcile
	// must not delete it during stale cleanup, even though our hosts list
	// doesn't include its dnsName.
	foreign := &unstructured.Unstructured{}
	foreign.SetAPIVersion(externaldns.APIVersion)
	foreign.SetKind(externaldns.Kind)
	foreign.SetName(foreignRecordName)
	foreign.SetNamespace(testNamespace)
	foreign.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "external-dns-operator"})

	client := newFakeDynamic(t, foreign)
	rec := newReconciler(t, client)

	reconcileErr := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if reconcileErr != nil {
		t.Fatalf("Reconcile: %v", reconcileErr)
	}

	got := listEndpoints(t, client)

	// Expect 2: the foreign one + our own a.example.com
	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2 (foreign + ours): %v", len(got), got)
	}

	var foundForeign bool

	for _, item := range got {
		if item.GetName() == foreignRecordName {
			foundForeign = true
		}
	}

	if !foundForeign {
		t.Fatal("foreign DNSEndpoint was deleted by stale-cleanup — must be left alone")
	}
}

func TestReconciler_RejectsCanceledContext(t *testing.T) {
	t.Parallel()

	client := newFakeDynamic(t)
	rec := newReconciler(t, client)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := rec.Reconcile(ctx, []string{"a.example.com"})
	if err == nil {
		t.Fatal("Reconcile: want error for canceled context, got nil")
	}
}

func TestReconciler_CreateError_FailsLoudly(t *testing.T) {
	t.Parallel()

	// Apply path: Get returns NotFound on first reconcile → Reconciler
	// falls through to Create. If Create errors with anything other than
	// AlreadyExists, the workqueue must see the error so it retries.
	client := newFakeDynamic(t)
	client.PrependReactor("create", "dnsendpoints", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errSyntheticPatch
	})

	rec := newReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err == nil {
		t.Fatal("Reconcile: want error when create fails, got nil")
	}

	if !strings.Contains(err.Error(), "synthetic patch failure") {
		t.Fatalf("error %q does not wrap synthetic failure", err.Error())
	}
}

func TestReconciler_DeleteRace_NotFoundIsBenign(t *testing.T) {
	t.Parallel()

	// Seed a stale ouroboros-owned object so cleanup tries to delete it,
	// then have the fake client return 404 — simulates someone else
	// removing it between our list and delete. Reconcile must succeed.
	stale := &unstructured.Unstructured{}
	stale.SetAPIVersion(externaldns.APIVersion)
	stale.SetKind(externaldns.Kind)
	stale.SetName("ouroboros-stale-host-com")
	stale.SetNamespace(testNamespace)
	stale.SetLabels(map[string]string{
		externaldns.LabelManagedBy: managedByValue,
		externaldns.LabelInstance:  testRelease,
	})

	client := newFakeDynamic(t, stale)
	client.PrependReactor("delete", "dnsendpoints", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(externaldns.GVR.GroupResource(), "ouroboros-stale-host-com")
	})

	rec := newReconciler(t, client)

	// New host set does NOT include "stale-host-com" → cleanup will try
	// to delete the stale object → fake returns 404.
	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: 404-on-delete must be benign, got: %v", err)
	}
}

func TestReconciler_LargeHostSet(t *testing.T) {
	t.Parallel()

	client := newFakeDynamic(t)
	rec := newReconciler(t, client)

	hosts := make([]string, 200)
	for i := range hosts {
		hosts[i] = stringRepeat("h", 1) + intString(i) + ".example.com"
	}

	err := rec.Reconcile(t.Context(), hosts)
	if err != nil {
		t.Fatalf("Reconcile 200 hosts: %v", err)
	}

	got := listEndpoints(t, client)
	if len(got) != 200 {
		t.Fatalf("got %d endpoints, want 200", len(got))
	}
}

func stringRepeat(s string, n int) string { return strings.Repeat(s, n) }

func intString(value int) string {
	const base10 = 10

	if value == 0 {
		return "0"
	}

	var (
		digits [20]byte
		idx    = len(digits)
	)

	for value > 0 {
		idx--
		digits[idx] = byte('0' + value%base10)
		value /= base10
	}

	return string(digits[idx:])
}

func TestReconciler_DualStack_EmitsTwoObjects(t *testing.T) {
	t.Parallel()

	client := newFakeDynamic(t)

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	rec, err := externaldns.NewReconciler(&externaldns.ReconcilerConfig{
		Client:    client,
		Namespace: testNamespace,
		Instance:  testRelease,
		Targets:   []string{v4Target, v6Target},
		TTL:       60,
		Source:    externaldns.SourceController,
		Log:       logger,
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}

	reconcileErr := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if reconcileErr != nil {
		t.Fatalf("Reconcile: %v", reconcileErr)
	}

	got := listEndpoints(t, client)
	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2 (one A, one AAAA)", len(got))
	}
}

func TestReconciler_EmptyHosts_WithExistingOwned_SkipsPruneSilently(t *testing.T) {
	t.Parallel()

	// Realistic accident: operator removes all HTTPRoutes / Ingresses
	// (or replaces every hostname with a wildcard, which extract drops).
	// Reconcile is called with hosts=[] but the cluster still owns
	// DNSEndpoints from the previous healthy state. Without this guard,
	// prune would silently delete every record. With it, Reconcile
	// logs a Warn and returns cleanly.
	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion(externaldns.APIVersion)
	existing.SetKind(externaldns.Kind)
	existing.SetName("ouroboros-existing")
	existing.SetNamespace(testNamespace)
	existing.SetLabels(map[string]string{
		externaldns.LabelManagedBy: managedByValue,
		externaldns.LabelInstance:  testRelease,
	})

	client := newFakeDynamic(t, existing)
	rec := newReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{})
	if err != nil {
		t.Fatalf("Reconcile must NOT error on the empty-hosts safety net path: %v", err)
	}

	got := listEndpoints(t, client)
	if len(got) != 1 {
		t.Fatalf("existing DNSEndpoint must NOT have been pruned during empty-hosts reconcile; got %d", len(got))
	}
}

func TestReconciler_EmptyHosts_NoExistingOwned_NoOp(t *testing.T) {
	t.Parallel()

	// Fresh install with no routes and no records: empty-hosts guard
	// must NOT trip (nothing to protect). Reconcile returns cleanly,
	// no DNSEndpoints emitted.
	client := newFakeDynamic(t)
	rec := newReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{})
	if err != nil {
		t.Fatalf("Reconcile on empty cluster must be a no-op: %v", err)
	}

	got := listEndpoints(t, client)
	if len(got) != 0 {
		t.Fatalf("nothing should be created from empty hosts; got %d", len(got))
	}
}

func TestReconciler_AllBuildsFail_RefusesToPrune(t *testing.T) {
	t.Parallel()

	// Seed an existing ouroboros-owned DNSEndpoint that prune would have
	// deleted under the old behaviour. Then drive Reconcile with hosts that
	// every fail Build (wildcard hosts are unconditionally rejected by
	// validateHost). The reconciler MUST return an error and leave the
	// existing record alone — losing every record because of one bad input
	// is the catastrophic failure /branch-review flagged.
	stale := &unstructured.Unstructured{}
	stale.SetAPIVersion(externaldns.APIVersion)
	stale.SetKind(externaldns.Kind)
	stale.SetName("ouroboros-stale")
	stale.SetNamespace(testNamespace)
	stale.SetLabels(map[string]string{
		externaldns.LabelManagedBy: managedByValue,
		externaldns.LabelInstance:  testRelease,
	})

	client := newFakeDynamic(t, stale)
	rec := newReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"*.bad.example.com", "*.also-bad.example.com"})
	if err == nil {
		t.Fatal("Reconcile: every-host-fails must return an error, not silently delete-all")
	}

	got := listEndpoints(t, client)
	if len(got) == 0 {
		t.Fatal("Reconcile pruned the existing record despite zero successful builds — would wipe production DNS records")
	}
}

func TestReconciler_NameCollisionWithForeignEndpoint_RefusesOverwrite(t *testing.T) {
	t.Parallel()

	// Pre-seed a foreign DNSEndpoint whose name happens to match what
	// BuildEndpoints would render for "a.example.com". This is the
	// shared-namespace blast-radius scenario for the CRD path: the
	// operator pointed externalDns.namespace at a namespace that
	// already contains a CR with our name, owned by a different team
	// or a previous release with a different .Release.Name. Without
	// the ownership check, ouroboros would silently rewrite labels +
	// spec via Update, claiming ownership.
	foreign := &unstructured.Unstructured{}
	foreign.SetAPIVersion(externaldns.APIVersion)
	foreign.SetKind(externaldns.Kind)
	foreign.SetName(collidingObjectName)
	foreign.SetNamespace(testNamespace)
	foreign.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": foreignManagedByLabel,
	})

	foreignSpec := map[string]any{
		"endpoints": []any{
			map[string]any{
				"dnsName":    "a.example.com",
				"recordType": "A",
				"targets":    []any{"203.0.113.42"},
			},
		},
	}

	specErr := unstructured.SetNestedMap(foreign.Object, foreignSpec, "spec")
	if specErr != nil {
		t.Fatalf("seed foreign spec: %v", specErr)
	}

	client := newFakeDynamic(t, foreign)
	rec := newReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err == nil {
		t.Fatal("Reconcile: name collision with foreign DNSEndpoint must error, not silently overwrite")
	}

	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("error %q must explain the collision (expected 'refusing to overwrite' substring)", err.Error())
	}

	got, err := client.Resource(externaldns.GVR).Namespace(testNamespace).
		Get(t.Context(), collidingObjectName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get foreign endpoint: %v", err)
	}

	if got.GetLabels()["app.kubernetes.io/managed-by"] != foreignManagedByLabel {
		t.Fatalf("foreign DNSEndpoint labels were rewritten — got managed-by=%q",
			got.GetLabels()["app.kubernetes.io/managed-by"])
	}

	endpoints, found, err := unstructured.NestedSlice(got.Object, "spec", "endpoints")
	if err != nil || !found {
		t.Fatalf("get foreign spec.endpoints: found=%v err=%v", found, err)
	}

	first, ok := endpoints[0].(map[string]any)
	if !ok {
		t.Fatalf("foreign endpoints[0] not a map: %T", endpoints[0])
	}

	targets, _, _ := unstructured.NestedStringSlice(first, "targets")
	if len(targets) != 1 || targets[0] != "203.0.113.42" {
		t.Fatalf("foreign target rewritten — got %v, want [203.0.113.42]", targets)
	}
}

func TestNewReconciler_RejectsMissingClient(t *testing.T) {
	t.Parallel()

	_, err := externaldns.NewReconciler(&externaldns.ReconcilerConfig{
		Namespace: testNamespace, Instance: testRelease,
		Targets: []string{v4Target}, Source: externaldns.SourceController,
	})
	if err == nil {
		t.Fatal("NewReconciler: want error for nil client, got nil")
	}
}

func TestNewReconciler_RejectsMissingTargets(t *testing.T) {
	t.Parallel()

	client := newFakeDynamic(t)

	_, err := externaldns.NewReconciler(&externaldns.ReconcilerConfig{
		Client: client, Namespace: testNamespace, Instance: testRelease,
		Targets: nil, Source: externaldns.SourceController,
	})
	if err == nil {
		t.Fatal("NewReconciler: want error for empty targets, got nil")
	}
}
