package externaldns_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/lexfrei/ouroboros/internal/externaldns"
)

func newDNSEndpointObject(name string, generation, observedGeneration int64, createdAgo time.Duration) *unstructured.Unstructured {
	uns := &unstructured.Unstructured{}
	uns.SetAPIVersion(externaldns.APIVersion)
	uns.SetKind(externaldns.Kind)
	uns.SetName(name)
	uns.SetNamespace(testNamespace)
	uns.SetGeneration(generation)
	uns.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-createdAgo)))

	if observedGeneration > 0 {
		_ = unstructured.SetNestedField(uns.Object, observedGeneration, "status", "observedGeneration")
	}

	uns.SetLabels(map[string]string{
		externaldns.LabelManagedBy: managedByValue,
		externaldns.LabelInstance:  testRelease,
	})

	return uns
}

func captureLogs(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, buf
}

func TestSurfaceStatus_ConvergedObject_NoWarning(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)

	// generation == observedGeneration → external-dns has reconciled this
	// version of the spec; nothing to report.
	obj := newDNSEndpointObject("dns-1", 5, 5, 10*time.Minute)
	surfacer.Surface([]*unstructured.Unstructured{obj}, time.Now())

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("converged object produced a warning: %s", buf.String())
	}
}

func TestSurfaceStatus_FreshObject_NoWarning(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)

	// Object was just created (5s ago) — external-dns may not have observed
	// it yet. The surfacer must wait out the grace window before crying
	// wolf, otherwise every reconcile right after Ingress creation produces
	// a warning that resolves itself in seconds.
	obj := newDNSEndpointObject("dns-2", 1, 0, 5*time.Second)
	surfacer.Surface([]*unstructured.Unstructured{obj}, time.Now())

	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("fresh object produced a warning before grace period: %s", buf.String())
	}
}

func TestSurfaceStatus_GenerationDrift_LogsWarning(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)

	// Spec was bumped to gen=3 but external-dns only observed gen=2 even
	// after 5 minutes. Real failure mode: external-dns crashed, lost RBAC,
	// or its provider rejected the record. Operator needs to see this.
	obj := newDNSEndpointObject("dns-3", 3, 2, 5*time.Minute)
	surfacer.Surface([]*unstructured.Unstructured{obj}, time.Now())

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("expected a WARN log for generation drift; got: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "dns-3") {
		t.Fatalf("warning does not mention object name: %s", buf.String())
	}
}

func TestSurfaceStatus_NoStatusYet_LogsWarningAfterGrace(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)

	// Object existed for 10 minutes with no observedGeneration written —
	// external-dns is not picking it up. Same severity as drift.
	obj := newDNSEndpointObject("dns-4", 1, 0, 10*time.Minute)
	surfacer.Surface([]*unstructured.Unstructured{obj}, time.Now())

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("expected a WARN log for missing status: %s", buf.String())
	}
}

func TestSurfaceStatus_DedupesIdenticalDrift(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)

	// Two reconciles within the dedupe window — only one log line.
	obj := newDNSEndpointObject("dns-5", 3, 2, 5*time.Minute)
	now := time.Now()
	surfacer.Surface([]*unstructured.Unstructured{obj}, now)
	surfacer.Surface([]*unstructured.Unstructured{obj}, now.Add(time.Minute))

	count := strings.Count(buf.String(), "level=WARN")
	if count != 1 {
		t.Fatalf("expected dedupe to one warn within window, got %d: %s", count, buf.String())
	}
}

func TestSurfaceStatus_RewarnsAfterDedupeWindow(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)

	// Same object, second reconcile after the dedupe window — operator
	// gets a fresh signal so a long-running failure does not silently
	// disappear from logs.
	obj := newDNSEndpointObject("dns-6", 3, 2, 5*time.Minute)
	now := time.Now()
	surfacer.Surface([]*unstructured.Unstructured{obj}, now)
	surfacer.Surface([]*unstructured.Unstructured{obj}, now.Add(externaldns.DedupeWindow+time.Second))

	count := strings.Count(buf.String(), "level=WARN")
	if count < 2 {
		t.Fatalf("expected re-warn after dedupe window, got %d: %s", count, buf.String())
	}
}

func TestSurfaceStatus_HandlesNilLogger(t *testing.T) {
	t.Parallel()

	// Constructor must never panic on nil logger — same contract as
	// every other Reconciler in this codebase.
	surfacer := externaldns.NewStatusSurfacer(nil)
	obj := newDNSEndpointObject("dns-7", 3, 2, 5*time.Minute)
	surfacer.Surface([]*unstructured.Unstructured{obj}, time.Now())
}

func TestSurfaceStatus_HandlesEmptyList(t *testing.T) {
	t.Parallel()

	logger, buf := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)
	surfacer.Surface(nil, time.Now())

	if buf.Len() != 0 {
		t.Fatalf("expected no log on empty input: %s", buf.String())
	}
}

func TestSurfaceStatus_DropsDedupeEntryWhenObjectGone(t *testing.T) {
	t.Parallel()

	// First reconcile sees a drifted object and warns (recording the
	// object in lastLog). Second reconcile lists a different set —
	// the original object is gone (host removed from Ingress).
	// Surface MUST evict that entry so the dedupe map does not leak
	// across release cycles.
	logger, _ := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)

	objA := newDNSEndpointObject("dns-A", 3, 2, 5*time.Minute)
	objB := newDNSEndpointObject("dns-B", 3, 2, 5*time.Minute)

	now := time.Now()
	surfacer.Surface([]*unstructured.Unstructured{objA, objB}, now)
	// Now A is removed; only B survives.
	surfacer.Surface([]*unstructured.Unstructured{objB}, now.Add(time.Minute))

	got := externaldns.LastLogSize(surfacer)
	if got != 1 {
		t.Fatalf("lastLog size = %d, want 1 (entry for A must be evicted)", got)
	}
}

func TestSurfaceStatus_EmptyListDropsAllEntries(t *testing.T) {
	t.Parallel()

	logger, _ := captureLogs(t)
	surfacer := externaldns.NewStatusSurfacer(logger)

	obj := newDNSEndpointObject("dns-A", 3, 2, 5*time.Minute)
	surfacer.Surface([]*unstructured.Unstructured{obj}, time.Now())

	if externaldns.LastLogSize(surfacer) != 1 {
		t.Fatalf("setup: lastLog size = %d, want 1", externaldns.LastLogSize(surfacer))
	}

	// Reconcile finds zero ouroboros-owned DNSEndpoints (e.g. all hosts
	// were removed). The dedupe map should empty out.
	surfacer.Surface(nil, time.Now())

	if got := externaldns.LastLogSize(surfacer); got != 0 {
		t.Fatalf("lastLog size = %d after empty Surface, want 0", got)
	}
}
