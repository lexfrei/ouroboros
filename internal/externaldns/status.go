package externaldns

import (
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Status surfacing tunables. The DNSEndpoint CRD's status sub-resource only
// exposes observedGeneration — there is no Published condition we can read.
// We approximate "external-dns is unhealthy" with generation drift: if
// .status.observedGeneration lags behind .metadata.generation for longer
// than gracePeriod, external-dns is either down, lost RBAC, or its provider
// is rejecting writes. The operator needs to see this in ouroboros logs
// because external-dns may be deployed in a different namespace they aren't
// watching.
const (
	gracePeriod  = 60 * time.Second
	DedupeWindow = 5 * time.Minute
)

// StatusSurfacer emits slog warnings for DNSEndpoints that external-dns has
// not converged within gracePeriod. Repeated warnings for the same object
// are de-duplicated within DedupeWindow so a flapping condition does not
// flood the log.
type StatusSurfacer struct {
	log     *slog.Logger
	mu      sync.Mutex
	lastLog map[string]time.Time
}

// NewStatusSurfacer builds a surfacer. log == nil silences output, matching
// the Reconciler contract elsewhere in this package.
func NewStatusSurfacer(log *slog.Logger) *StatusSurfacer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return &StatusSurfacer{
		log:     log,
		lastLog: make(map[string]time.Time),
	}
}

// Surface walks the supplied DNSEndpoints and emits a warning for any whose
// observedGeneration trails the spec generation past gracePeriod. The
// reconciler calls this once per pass, after the list-and-prune step, so
// status surfacing piggybacks on existing API traffic without a new
// informer.
//
// Surface also prunes lastLog entries for objects no longer present in the
// supplied set. Without this, releasing-and-recreating short-lived
// hostnames (preview environments, CI clusters) would leak entries into the
// dedupe map indefinitely.
func (surfacer *StatusSurfacer) Surface(objects []*unstructured.Unstructured, now time.Time) {
	if len(objects) == 0 {
		surfacer.pruneStaleEntries(nil)

		return
	}

	live := make(map[string]struct{}, len(objects))

	for _, obj := range objects {
		if obj == nil {
			continue
		}

		live[obj.GetName()] = struct{}{}
		surfacer.surfaceOne(obj, now)
	}

	surfacer.pruneStaleEntries(live)
}

// pruneStaleEntries drops dedupe entries whose owning object is no longer
// in the supplied live set. Called from Surface so the map never grows
// unboundedly across release cycles.
func (surfacer *StatusSurfacer) pruneStaleEntries(live map[string]struct{}) {
	surfacer.mu.Lock()
	defer surfacer.mu.Unlock()

	for name := range surfacer.lastLog {
		if _, alive := live[name]; !alive {
			delete(surfacer.lastLog, name)
		}
	}
}

func (surfacer *StatusSurfacer) surfaceOne(obj *unstructured.Unstructured, now time.Time) {
	if obj == nil {
		return
	}

	specGen := obj.GetGeneration()

	observedGen, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if err != nil {
		// Field exists but with the wrong type — log once and move on.
		surfacer.maybeWarn(obj.GetName(), now, "DNSEndpoint status.observedGeneration not an int64",
			slog.Int64("specGeneration", specGen),
			slog.String("error", err.Error()))

		return
	}

	if found && observedGen >= specGen {
		return
	}

	createdAt := obj.GetCreationTimestamp().Time
	if !createdAt.IsZero() && now.Sub(createdAt) < gracePeriod {
		return
	}

	if !found {
		surfacer.maybeWarn(obj.GetName(), now,
			"DNSEndpoint has no status.observedGeneration after grace period — external-dns may not be running or may lack RBAC to update status",
			slog.Int64("specGeneration", specGen))

		return
	}

	surfacer.maybeWarn(obj.GetName(), now,
		"DNSEndpoint observedGeneration trails spec generation — external-dns may be unhealthy or its provider is rejecting writes",
		slog.Int64("specGeneration", specGen),
		slog.Int64("observedGeneration", observedGen))
}

func (surfacer *StatusSurfacer) maybeWarn(name string, now time.Time, msg string, attrs ...slog.Attr) {
	surfacer.mu.Lock()
	defer surfacer.mu.Unlock()

	last, seen := surfacer.lastLog[name]
	if seen && now.Sub(last) < DedupeWindow {
		return
	}

	surfacer.lastLog[name] = now

	args := make([]any, 0, len(attrs)+1)
	args = append(args, slog.String("dnsEndpoint", name))

	for _, attr := range attrs {
		args = append(args, attr)
	}

	surfacer.log.Warn(msg, args...)
}
