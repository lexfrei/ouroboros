package externaldns_test

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/lexfrei/ouroboros/internal/externaldns"
)

func unsWithLabels(name string, labels map[string]string) *unstructured.Unstructured {
	uns := &unstructured.Unstructured{}
	uns.SetName(name)
	uns.SetNamespace(testNamespace)
	uns.SetLabels(labels)

	return uns
}

func TestOwnershipSelector_FormatsLabelSelector(t *testing.T) {
	t.Parallel()

	got := externaldns.OwnershipSelector(testRelease)

	// The selector must match a Kubernetes label-selector grammar — equal
	// signs, comma-separated, no spaces. A typo here would cause the list
	// call to either match everything (deleting non-ouroboros records) or
	// nothing (silently leaking strays).
	want := "app.kubernetes.io/managed-by=ouroboros,ouroboros.lexfrei.tech/instance=myrelease"
	if got != want {
		t.Fatalf("OwnershipSelector(myrelease) = %q, want %q", got, want)
	}
}

func TestOwnershipSelectorAsMap_HasBothLabels(t *testing.T) {
	t.Parallel()

	// The label-map form is what controllers feed to a metav1.ListOptions
	// when building structured selectors instead of raw strings.
	got := externaldns.OwnershipSelectorAsMap(testRelease)

	if got[externaldns.LabelManagedBy] != managedByValue {
		t.Fatalf("missing managed-by: %v", got)
	}

	if got[externaldns.LabelInstance] != testRelease {
		t.Fatalf("missing instance: %v", got)
	}

	// MatchLabels also has to accept the map (canary check that the keys
	// don't include forbidden chars).
	_, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: got})
	if err != nil {
		t.Fatalf("LabelSelectorAsSelector rejected our map: %v", err)
	}
}

func TestIsOwnedByOuroboros_AcceptsMatchingLabels(t *testing.T) {
	t.Parallel()

	uns := unsWithLabels("dnsendpoint-1", map[string]string{
		externaldns.LabelManagedBy: "ouroboros",
		externaldns.LabelInstance:  testRelease,
	})

	if !externaldns.IsOwnedByOuroboros(uns, testRelease) {
		t.Fatal("expected match for matching labels")
	}
}

func TestIsOwnedByOuroboros_RejectsUnrelated(t *testing.T) {
	t.Parallel()

	// A pre-existing DNSEndpoint that some other controller owns must not
	// be deleted — that is the whole point of the ownership check during
	// stale cleanup.
	uns := unsWithLabels("foreign-dnsendpoint", map[string]string{
		"app.kubernetes.io/managed-by": "external-dns-operator",
	})

	if externaldns.IsOwnedByOuroboros(uns, testRelease) {
		t.Fatal("ownership predicate must reject foreign object")
	}
}

func TestIsOwnedByOuroboros_RejectsWrongInstance(t *testing.T) {
	t.Parallel()

	// Two ouroboros releases in the same namespace would clash — the
	// instance label is the disambiguator. Reconciler N must not delete
	// objects belonging to reconciler M.
	uns := unsWithLabels("other-release", map[string]string{
		externaldns.LabelManagedBy: "ouroboros",
		externaldns.LabelInstance:  "other-release",
	})

	if externaldns.IsOwnedByOuroboros(uns, testRelease) {
		t.Fatal("ownership predicate must scope by instance")
	}
}

func TestIsOwnedByOuroboros_RejectsMissingLabels(t *testing.T) {
	t.Parallel()

	uns := unsWithLabels("no-labels", nil)

	if externaldns.IsOwnedByOuroboros(uns, testRelease) {
		t.Fatal("ownership predicate must reject objects with no labels")
	}
}

func TestIsOwnedByOuroboros_RejectsNilObject(t *testing.T) {
	t.Parallel()

	if externaldns.IsOwnedByOuroboros(nil, testRelease) {
		t.Fatal("ownership predicate must safely reject nil")
	}
}

// TestOwnership_RoundTripsThroughBuild pins the invariant that the
// managed-by VALUE Build writes is exactly what OwnershipSelector
// queries for and IsOwnedByOuroboros recognises. A rename of one
// occurrence without the others would split-brain reconcile: Build
// creates new endpoints, prune fails to find them, old endpoints
// survive as orphans forever. ManagedByValue exists to be the single
// source of truth — this test is what proves it (Go side).
//
// The chart side is pinned in charts/ouroboros/tests/cleanup-hook_test.yaml
// ('cleanup hook selector pins managed-by literal'), which asserts the
// helm helper still emits the exact literal 'ouroboros'.
func TestOwnership_RoundTripsThroughBuild(t *testing.T) {
	t.Parallel()

	endpoints := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	uns, err := endpoints[0].ToUnstructured()
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}

	if !externaldns.IsOwnedByOuroboros(uns, testInstance) {
		t.Fatalf("IsOwnedByOuroboros must recognise an endpoint Build just produced; labels=%v",
			uns.GetLabels())
	}

	selector := externaldns.OwnershipSelector(testInstance)

	want := externaldns.LabelManagedBy + "=" + managedByValue
	if !strings.Contains(selector, want) {
		t.Fatalf("OwnershipSelector %q does not encode %q — single-source-of-truth broken", selector, want)
	}

	// Hard-pin the literal value of ManagedByValue. The helm helper
	// 'ouroboros.managedByValue' in _helpers.tpl emits the same string;
	// changing one without the other would silently desynchronise the
	// cleanup-hook selector from the labels Build writes. The chart-side
	// test pins the helm helper end; this assert pins the Go end. Either
	// one tripping signals a desync.
	if externaldns.ManagedByValue != managedByValue {
		t.Fatalf("ManagedByValue = %q, want %q — also update charts/ouroboros/templates/_helpers.tpl 'ouroboros.managedByValue' helper",
			externaldns.ManagedByValue, managedByValue)
	}
}
