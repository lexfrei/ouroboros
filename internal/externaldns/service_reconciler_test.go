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

func TestServiceReconciler_NoDrift_NoCreatesOrDeletesOnSecondPass(t *testing.T) {
	t.Parallel()

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
		if action.GetVerb() == "create" || action.GetVerb() == verbDeleteSvc {
			t.Fatalf("second pass produced %s — should be a no-op", action.GetVerb())
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
