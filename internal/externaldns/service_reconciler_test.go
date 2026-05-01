package externaldns_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/lexfrei/ouroboros/internal/externaldns"
)

const verbDeleteSvc = "delete"

var (
	errSyntheticServiceCreate = errors.New("synthetic service create failure")
	errSyntheticServiceUpdate = errors.New("synthetic service update failure")
)

func newServiceReconciler(t *testing.T, client kubernetes.Interface) *externaldns.ServiceReconciler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	rec, err := externaldns.NewServiceReconciler(&externaldns.ServiceReconcilerConfig{
		Client:           client,
		Namespace:        testNamespace,
		Instance:         testRelease,
		Targets:          []string{v4Target},
		TTL:              60,
		Source:           externaldns.SourceController,
		AnnotationPrefix: testAnnotationPrefix,
		Log:              logger,
	})
	if err != nil {
		t.Fatalf("NewServiceReconciler: %v", err)
	}

	return rec
}

func listOuroborosServices(t *testing.T, client kubernetes.Interface) []corev1.Service {
	t.Helper()

	got, err := client.CoreV1().Services(testNamespace).List(t.Context(), metav1.ListOptions{
		LabelSelector: externaldns.OwnershipSelector(testRelease),
	})
	if err != nil {
		t.Fatalf("list services: %v", err)
	}

	return got.Items
}

func TestServiceReconciler_FirstRun_CreatesAllServices(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com", "b.example.com", "c.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := listOuroborosServices(t, client)
	if len(got) != 3 {
		t.Fatalf("got %d Services, want 3", len(got))
	}

	for index := range got {
		if got[index].Spec.ClusterIP != corev1.ClusterIPNone {
			t.Errorf("Service %s should be headless, got ClusterIP=%q", got[index].Name, got[index].Spec.ClusterIP)
		}
	}
}

func TestServiceReconciler_NoDrift_NoMutationsOnSecondPass(t *testing.T) {
	t.Parallel()

	// Equality short-circuit guarantee: the second reconcile produces
	// zero mutating actions when nothing changed. Without the
	// short-circuit apply() would Update every Service every resync
	// (~10 min × N hosts), and external-dns would re-publish on every
	// generation bump. Expected verbs on a clean second pass are
	// 'list' (prune scan) + 'get' (apply pre-check) only.
	client := fake.NewSimpleClientset()
	rec := newServiceReconciler(t, client)

	hosts := []string{"a.example.com", "b.example.com"}

	firstErr := rec.Reconcile(t.Context(), hosts)
	if firstErr != nil {
		t.Fatalf("first Reconcile: %v", firstErr)
	}

	first := listOuroborosServices(t, client)
	client.ClearActions()

	secondErr := rec.Reconcile(t.Context(), hosts)
	if secondErr != nil {
		t.Fatalf("second Reconcile: %v", secondErr)
	}

	second := listOuroborosServices(t, client)

	if len(first) != len(second) {
		t.Fatalf("count drift: %d -> %d", len(first), len(second))
	}

	for _, action := range client.Actions() {
		verb := action.GetVerb()
		if verb == verbCreate || verb == verbUpdate || verb == verbPatch || verb == verbDeleteSvc {
			t.Fatalf("second pass produced %s — equality short-circuit should make this a no-op", verb)
		}
	}
}

func TestServiceReconciler_HostRemoved_DeletesOrphanedService(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	rec := newServiceReconciler(t, client)

	firstErr := rec.Reconcile(t.Context(), []string{"a.example.com", "b.example.com", "c.example.com"})
	if firstErr != nil {
		t.Fatalf("first Reconcile: %v", firstErr)
	}

	secondErr := rec.Reconcile(t.Context(), []string{"a.example.com", "b.example.com"})
	if secondErr != nil {
		t.Fatalf("second Reconcile: %v", secondErr)
	}

	got := listOuroborosServices(t, client)
	if len(got) != 2 {
		t.Fatalf("got %d Services, want 2 (c was removed)", len(got))
	}

	for _, item := range got {
		if strings.Contains(item.Name, "c-example-com") {
			t.Fatalf("c.example.com still present after host removal: %s", item.Name)
		}
	}
}

func TestServiceReconciler_OwnershipFilter_LeavesForeignAlone(t *testing.T) {
	t.Parallel()

	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      foreignRecordName,
			Namespace: testNamespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "external-dns-operator"},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	client := fake.NewSimpleClientset(foreign)
	rec := newServiceReconciler(t, client)

	reconcileErr := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if reconcileErr != nil {
		t.Fatalf("Reconcile: %v", reconcileErr)
	}

	all, err := client.CoreV1().Services(testNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var foundForeign bool

	for index := range all.Items {
		if all.Items[index].Name == foreignRecordName {
			foundForeign = true
		}
	}

	if !foundForeign {
		t.Fatal("foreign Service was deleted by stale-cleanup — must be left alone")
	}
}

func TestServiceReconciler_RejectsCanceledContext(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	rec := newServiceReconciler(t, client)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := rec.Reconcile(ctx, []string{"a.example.com"})
	if err == nil {
		t.Fatal("Reconcile: want error for canceled context")
	}
}

func TestServiceReconciler_CreateError_FailsLoudly(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "services", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errSyntheticServiceCreate
	})

	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err == nil {
		t.Fatal("Reconcile: want error when create fails")
	}

	if !strings.Contains(err.Error(), "synthetic service create failure") {
		t.Fatalf("error %q does not wrap synthetic failure", err.Error())
	}
}

func TestServiceReconciler_DeleteRace_NotFoundIsBenign(t *testing.T) {
	t.Parallel()

	stale := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ouroboros-stale-host-com",
			Namespace: testNamespace,
			Labels: map[string]string{
				externaldns.LabelManagedBy: managedByValue,
				externaldns.LabelInstance:  testRelease,
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	client := fake.NewSimpleClientset(stale)
	client.PrependReactor("delete", "services", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("services"), "ouroboros-stale-host-com")
	})

	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: 404-on-delete must be benign, got: %v", err)
	}
}

func TestServiceReconciler_UpdateError_NonNotFoundIsWrapped(t *testing.T) {
	t.Parallel()

	// Pre-seed an existing owned Service so apply() takes the
	// Get-success → Update path (not Create). A transient apiserver
	// failure on Update must surface to the workqueue for retry — only
	// IsNotFound is silently swallowed (race with prune).
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ouroboros-a-example-com",
			Namespace: testNamespace,
			Labels: map[string]string{
				externaldns.LabelManagedBy: managedByValue,
				externaldns.LabelInstance:  testRelease,
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	client := fake.NewSimpleClientset(existing)
	client.PrependReactor("update", "services", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errSyntheticServiceUpdate
	})

	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err == nil {
		t.Fatal("Reconcile: non-NotFound Update error must surface for workqueue retry")
	}

	if !strings.Contains(err.Error(), "synthetic service update failure") {
		t.Fatalf("error %q does not wrap synthetic Update failure", err.Error())
	}
}

func TestServiceReconciler_DualStack_PreservesIPFamiliesOnUpdate(t *testing.T) {
	t.Parallel()

	// Real dual-stack RequireDualStack apiservers reject Update with
	// IPFamilies zeroed: 'Invalid value: []core.IPFamily{}: primary
	// ipFamily can not be unset'. fake.NewSimpleClientset does not
	// validate, but we still must not zero these fields on the way out.
	// Pre-seed an owned Service with annotations that DIFFER from
	// what BuildService would render — that forces the equality
	// short-circuit to miss and apply() to take the Update path.
	// Capture the Update payload through a reactor and assert all
	// apiserver-defaulted fields survive.
	preferDualStack := corev1.IPFamilyPolicyPreferDualStack
	internalLocal := corev1.ServiceInternalTrafficPolicyLocal

	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ouroboros-a-example-com",
			Namespace: testNamespace,
			Labels: map[string]string{
				externaldns.LabelManagedBy: managedByValue,
				externaldns.LabelInstance:  testRelease,
			},
			Annotations: map[string]string{
				// stale value — drift will trigger Update
				externaldns.AnnotationSource: "stale",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeClusterIP,
			ClusterIP:             corev1.ClusterIPNone,
			ClusterIPs:            []string{corev1.ClusterIPNone},
			IPFamilies:            []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol},
			IPFamilyPolicy:        &preferDualStack,
			InternalTrafficPolicy: &internalLocal,
			SessionAffinity:       corev1.ServiceAffinityClientIP,
		},
	}

	client := fake.NewSimpleClientset(existing)

	var captured *corev1.Service

	client.PrependReactor("update", "services", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updateAction, ok := action.(clienttesting.UpdateAction)
		if !ok {
			t.Fatalf("update reactor: unexpected action type %T", action)
		}

		got, ok := updateAction.GetObject().(*corev1.Service)
		if !ok {
			t.Fatalf("update reactor: object not a *corev1.Service: %T", updateAction.GetObject())
		}

		captured = got.DeepCopy()
		// false = let the default reactor still write through so the
		// rest of the test sees a coherent store.
		return false, nil, nil
	})

	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if captured == nil {
		t.Fatal("Reconcile produced no Update — expected drift on stale annotation to trigger one")
	}

	if len(captured.Spec.IPFamilies) != 2 ||
		captured.Spec.IPFamilies[0] != corev1.IPv4Protocol ||
		captured.Spec.IPFamilies[1] != corev1.IPv6Protocol {
		t.Fatalf("IPFamilies not preserved: got %v, want [IPv4 IPv6]", captured.Spec.IPFamilies)
	}

	if captured.Spec.IPFamilyPolicy == nil || *captured.Spec.IPFamilyPolicy != preferDualStack {
		t.Fatalf("IPFamilyPolicy not preserved: got %v, want PreferDualStack", captured.Spec.IPFamilyPolicy)
	}

	if captured.Spec.InternalTrafficPolicy == nil || *captured.Spec.InternalTrafficPolicy != internalLocal {
		t.Fatalf("InternalTrafficPolicy not preserved: got %v", captured.Spec.InternalTrafficPolicy)
	}

	if captured.Spec.SessionAffinity != corev1.ServiceAffinityClientIP {
		t.Fatalf("SessionAffinity not preserved: got %v, want ClientIP", captured.Spec.SessionAffinity)
	}
}

func TestServiceReconciler_NameCollisionWithForeignService_RefusesOverwrite(t *testing.T) {
	t.Parallel()

	// Pre-seed a foreign Service whose name happens to match what
	// BuildService would render for "a.example.com". This is the
	// shared-namespace blast-radius scenario — the operator pointed
	// externalDns.namespace at a namespace that already contains a
	// load-bearing Service with our prefix, owned by a different team.
	// Without the ownership check, ouroboros would silently overwrite
	// labels + spec, breaking traffic to the foreign Service.
	foreignServiceName := "ouroboros-a-example-com"
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      foreignServiceName,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "another-team",
				"app.kubernetes.io/component":  "frontend",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "real-traffic"},
			Ports:    []corev1.ServicePort{{Port: 80, Name: "http"}},
		},
	}

	client := fake.NewSimpleClientset(foreign)
	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err == nil {
		t.Fatal("Reconcile: name collision with foreign Service must error, not silently overwrite")
	}

	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("error %q must explain the collision (expected 'refusing to overwrite' substring)", err.Error())
	}

	got, err := client.CoreV1().Services(testNamespace).
		Get(t.Context(), foreignServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get foreign Service: %v", err)
	}

	if got.Labels["app.kubernetes.io/managed-by"] != "another-team" {
		t.Fatalf("foreign Service labels were rewritten — got managed-by=%q",
			got.Labels["app.kubernetes.io/managed-by"])
	}

	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0].Port != 80 {
		t.Fatalf("foreign Service spec was overwritten — got ports=%+v", got.Spec.Ports)
	}

	if got.Spec.Selector["app"] != "real-traffic" {
		t.Fatalf("foreign Service selector was wiped — got %+v", got.Spec.Selector)
	}
}

func TestServiceReconciler_EmptyHosts_WithExistingOwned_SkipsPruneSilently(t *testing.T) {
	t.Parallel()

	// Realistic accident: operator removes all HTTPRoutes / Ingresses
	// (or replaces every hostname with a wildcard, which extract drops).
	// Reconcile is called with hosts=[] but the cluster still owns
	// records from the previous healthy state. Without this guard,
	// prune would silently delete every record. With it, the reconciler
	// logs a Warn telling the operator how to clean up explicitly and
	// returns cleanly so the workqueue doesn't loop.
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ouroboros-existing-host-com",
			Namespace: testNamespace,
			Labels: map[string]string{
				externaldns.LabelManagedBy: managedByValue,
				externaldns.LabelInstance:  testRelease,
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	client := fake.NewSimpleClientset(existing)
	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{})
	if err != nil {
		t.Fatalf("Reconcile must NOT error on the empty-hosts safety net path: %v", err)
	}

	got := listOuroborosServices(t, client)
	if len(got) != 1 {
		t.Fatalf("existing Service must NOT have been pruned during empty-hosts reconcile; got %d", len(got))
	}
}

func TestServiceReconciler_EmptyHosts_NoExistingOwned_NoOp(t *testing.T) {
	t.Parallel()

	// Fresh install with no routes and no records: empty-hosts guard
	// must NOT trip (nothing to protect). Reconcile returns cleanly,
	// no Services emitted.
	client := fake.NewSimpleClientset()
	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{})
	if err != nil {
		t.Fatalf("Reconcile on empty cluster must be a no-op: %v", err)
	}

	got := listOuroborosServices(t, client)
	if len(got) != 0 {
		t.Fatalf("nothing should be created from empty hosts; got %d", len(got))
	}
}

func TestServiceReconciler_ListOwnedError_FailsLoudly(t *testing.T) {
	t.Parallel()

	// listOwned moved to the top of Reconcile, so a list failure now
	// blocks the whole reconcile. Pin the contract: any list error
	// must surface, apply must NOT have run.
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "services", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errSyntheticServiceCreate // any error works
	})

	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"a.example.com"})
	if err == nil {
		t.Fatal("Reconcile must surface listOwned errors")
	}

	if !strings.Contains(err.Error(), "list ouroboros-owned Services") {
		t.Fatalf("error %q must wrap with 'list ouroboros-owned Services'", err.Error())
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == verbCreate || action.GetVerb() == verbUpdate {
			t.Fatalf("apply must NOT have run after listOwned error; saw %s", action.GetVerb())
		}
	}
}

func TestServiceReconciler_EmptyHosts_ForeignOnlyRecords_NoGuardTrip(t *testing.T) {
	t.Parallel()

	// Cluster has an unrelated Service with no ouroboros labels.
	// listOwned filters by ownership selector, so owned=[]. With
	// hosts=[] AND owned=[], the guard must NOT trip — there is
	// nothing to protect. Reconcile returns cleanly, foreign object
	// untouched.
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      foreignRecordName,
			Namespace: testNamespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "external-dns-operator"},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	client := fake.NewSimpleClientset(foreign)
	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{})
	if err != nil {
		t.Fatalf("Reconcile must NOT trip guard when only foreign records exist: %v", err)
	}

	all, listErr := client.CoreV1().Services(testNamespace).List(t.Context(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatalf("list: %v", listErr)
	}

	if len(all.Items) != 1 || all.Items[0].Name != foreignRecordName {
		t.Fatalf("foreign Service must remain untouched; got %v", all.Items)
	}
}

func TestServiceReconciler_AllBuildsFail_NoExistingOwned_SilentNoOp(t *testing.T) {
	t.Parallel()

	// Fresh cluster, no owned records, every host fails Build (wildcards).
	// Both guards skip (owned=[] short-circuits). Reconcile returns
	// cleanly with no actions taken.
	client := fake.NewSimpleClientset()
	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{"*.foo.example", "*.bar.example"})
	if err != nil {
		t.Fatalf("Reconcile must be a silent no-op when desired=[] && owned=[]: %v", err)
	}

	for _, action := range client.Actions() {
		verb := action.GetVerb()
		if verb == verbCreate || verb == verbUpdate || verb == verbPatch || verb == verbDeleteSvc {
			t.Fatalf("no mutating action expected on no-op path; got %s", verb)
		}
	}
}

func TestServiceReconciler_EmptyHosts_GuardSkipsApply_NoCreateActions(t *testing.T) {
	t.Parallel()

	// Pin the invariant: on the empty-hosts safety-net path, apply
	// must NOT run. Without this test, a future "fix" that removes
	// the early return after the guard would silently start calling
	// apply on an empty desired (no-op today, but a regression
	// vector if apply ever grows side-effects).
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ouroboros-existing-host-com",
			Namespace: testNamespace,
			Labels: map[string]string{
				externaldns.LabelManagedBy: managedByValue,
				externaldns.LabelInstance:  testRelease,
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	client := fake.NewSimpleClientset(existing)
	client.PrependReactor("create", "services", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		t.Fatal("create must NOT be called on empty-hosts safety-net path")

		return true, nil, nil
	})

	rec := newServiceReconciler(t, client)

	err := rec.Reconcile(t.Context(), []string{})
	if err != nil {
		t.Fatalf("Reconcile must NOT error on safety-net path: %v", err)
	}
}

func TestServiceReconciler_AllBuildsFail_RefusesToPrune(t *testing.T) {
	t.Parallel()

	// Defence-in-depth invariant: when every Build fails (e.g. a future
	// regression in validation), prune must not delete every owned
	// Service. Same shape as the DNSEndpoint-side defence.
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ouroboros-existing-host-com",
			Namespace: testNamespace,
			Labels: map[string]string{
				externaldns.LabelManagedBy: managedByValue,
				externaldns.LabelInstance:  testRelease,
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	client := fake.NewSimpleClientset(existing)
	rec := newServiceReconciler(t, client)

	// Every host is a wildcard → BuildService rejects each → desired is
	// empty even though hosts is non-empty. Reconcile must error out.
	err := rec.Reconcile(t.Context(), []string{"*.foo.com", "*.bar.com"})
	if err == nil {
		t.Fatal("Reconcile: empty-desired-with-non-empty-hosts must error to prevent delete-all")
	}

	got := listOuroborosServices(t, client)
	if len(got) != 1 {
		t.Fatalf("existing Service must NOT have been pruned during all-builds-fail; got %d", len(got))
	}
}

func TestNewServiceReconciler_RejectsMissingClient(t *testing.T) {
	t.Parallel()

	_, err := externaldns.NewServiceReconciler(&externaldns.ServiceReconcilerConfig{
		Namespace:        testNamespace,
		Instance:         testRelease,
		Targets:          []string{v4Target},
		AnnotationPrefix: testAnnotationPrefix,
	})
	if err == nil {
		t.Fatal("NewServiceReconciler: missing client must fail")
	}
}

func TestNewServiceReconciler_RejectsMissingAnnotationPrefix(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()

	_, err := externaldns.NewServiceReconciler(&externaldns.ServiceReconcilerConfig{
		Client:    client,
		Namespace: testNamespace,
		Instance:  testRelease,
		Targets:   []string{v4Target},
	})
	if err == nil {
		t.Fatal("NewServiceReconciler: empty AnnotationPrefix must fail (Service annotations would be unaddressable)")
	}
}
