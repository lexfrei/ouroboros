package externaldns

import (
	"context"
	"log/slog"
	"reflect"
	"time"

	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// fieldManager is the field-manager string ouroboros uses on every Create
// and Update; the kube-apiserver tracks ownership under this name even
// without server-side-apply, which lets operators audit changes via
// kubectl --show-managed-fields.
const fieldManager = "ouroboros"

// ReconcilerConfig is the constructor input. It is a value-type so tests
// and the cmd wiring can fill it inline without splitting state across
// many positional arguments.
type ReconcilerConfig struct {
	Client      dynamic.Interface
	Namespace   string
	Instance    string
	Targets     []string
	TTL         int64
	Source      Source
	Annotations map[string]string
	Labels      map[string]string
	Surfacer    *StatusSurfacer
	Log         *slog.Logger
}

// Reconciler emits DNSEndpoint CRs for the desired host set via SSA and
// prunes ouroboros-owned objects that no longer correspond to a host.
// It does not maintain any informer of its own — input flows in through
// the Reconcile method, which the controller's workqueue calls.
type Reconciler struct {
	client      dynamic.Interface
	namespace   string
	instance    string
	targets     []string
	ttl         int64
	source      Source
	annotations map[string]string
	labels      map[string]string
	surfacer    *StatusSurfacer
	log         *slog.Logger
}

// NewReconciler validates cfg and returns a Reconciler. Missing client,
// targets, namespace, or instance are rejected at startup so the controller
// fails fast instead of writing junk records or silently doing nothing.
func NewReconciler(cfg *ReconcilerConfig) (*Reconciler, error) {
	if cfg.Client == nil {
		return nil, errors.New("NewReconciler: dynamic client is required")
	}

	if cfg.Namespace == "" {
		return nil, errors.New("NewReconciler: namespace is required")
	}

	if cfg.Instance == "" {
		return nil, errors.New("NewReconciler: instance is required")
	}

	if len(cfg.Targets) == 0 {
		return nil, errors.New("NewReconciler: at least one target IP is required")
	}

	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	source := cfg.Source
	if source == "" {
		source = SourceController
	}

	return &Reconciler{
		client:      cfg.Client,
		namespace:   cfg.Namespace,
		instance:    cfg.Instance,
		targets:     cfg.Targets,
		ttl:         cfg.TTL,
		source:      source,
		annotations: cfg.Annotations,
		labels:      cfg.Labels,
		surfacer:    cfg.Surfacer,
		log:         log,
	}, nil
}

// Reconcile applies the desired host set to DNSEndpoint CRs via Get→Update
// (Create-on-NotFound), then deletes any ouroboros-owned objects whose name
// no longer matches the desired set. The reconciler is safe to call from a
// rate-limited workqueue; every call is a single-pass: list-owned → guard →
// apply → prune → status surface.
//
// Get→Update over server-side-apply: ouroboros owns these objects
// exclusively (label-scoped), so SSA's conflict-free guarantee is not
// useful here, and Get→Update works with the dynamic fake client out of
// the box. The trade-off is two API calls per object instead of one, which
// is acceptable for the small object counts ouroboros emits in practice.
//
// Two layers of mass-prune defence:
//
//  1. hosts is non-empty but every Build failed → desired is empty. Return
//     an error so the workqueue retries; never prune.
//
//  2. hosts is empty (operator deleted all routes, or replaced every
//     hostname with a wildcard — controller.ExtractHostnames drops
//     wildcards) and at least one ouroboros-owned record exists. Skip
//     both apply and prune, log a Warn telling the operator to either
//     uninstall the chart/manifests or restore at least one hostname.
//     Status surfacing still runs over the unchanged set so
//     unhealthy-record warnings don't go silent during the empty-hosts
//     window. Note: 'informer cache stale on startup' is NOT a real
//     trigger — the controller's WaitForCacheSync precedes the first
//     reconcile, so a fresh start observes a fully synced cache.
func (rec *Reconciler) Reconcile(ctx context.Context, hosts []string) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return errors.Wrap(ctxErr, "externaldns reconcile: context already canceled")
	}

	desired := rec.buildDesired(hosts)

	owned, listErr := rec.listOwned(ctx)
	if listErr != nil {
		return errors.Wrap(listErr, "list ouroboros-owned DNSEndpoints")
	}

	skip, guardErr := rec.guardMassPrune(hosts, desired, owned)
	if guardErr != nil {
		return guardErr
	}

	if skip {
		// Status surfacing must keep running on the safety-net path —
		// otherwise unhealthy-record warnings go silent for the entire
		// empty-hosts window (slow rollback, partial revert, GitOps
		// pause). owned is the survivor set by definition: we declined
		// to prune it, no apply mutated it.
		if rec.surfacer != nil {
			rec.surfacer.Surface(owned, time.Now())
		}

		return nil
	}

	for name := range desired {
		endpoint := desired[name]

		applyErr := rec.apply(ctx, &endpoint)
		if applyErr != nil {
			return applyErr
		}
	}

	surviving, pruneErr := rec.pruneFromList(ctx, desired, owned)
	if pruneErr != nil {
		return pruneErr
	}

	// Surface gets the post-list snapshot, which means objects we just
	// Update'd may still report the old observedGeneration in this view.
	// That's tolerable: the grace-period (60s) and dedupe window (5min) in
	// StatusSurfacer absorb the transient mismatch, and the warning would
	// repeat next reconcile if the drift turns out to be real.
	if rec.surfacer != nil {
		rec.surfacer.Surface(surviving, time.Now())
	}

	return nil
}

func (rec *Reconciler) buildDesired(hosts []string) map[string]Endpoint {
	desired := make(map[string]Endpoint, len(hosts))

	for _, host := range hosts {
		endpoints, err := Build(&BuildOpts{
			Host:        host,
			Targets:     rec.targets,
			TTL:         rec.ttl,
			Source:      rec.source,
			Instance:    rec.instance,
			Namespace:   rec.namespace,
			Annotations: rec.annotations,
			Labels:      rec.labels,
		})
		if err != nil {
			rec.log.Warn("externaldns: skipping host with build failure",
				slog.String("host", host), slog.String("error", err.Error()))

			continue
		}

		for _, endpoint := range endpoints {
			desired[endpoint.Name] = endpoint
		}
	}

	return desired
}

func (rec *Reconciler) apply(ctx context.Context, endpoint *Endpoint) error {
	uns, convErr := endpoint.ToUnstructured()
	if convErr != nil {
		return errors.Wrapf(convErr, "render DNSEndpoint %s", endpoint.Name)
	}

	// Get→Update over server-side-apply: ouroboros is the only writer to
	// these objects (label-scoped), so SSA's conflict-free guarantees buy
	// nothing here, and Get→Update works with the dynamic fake client out
	// of the box. AlreadyExists / NotFound races between the Get, Create
	// and Update calls are treated as benign so a concurrent operator
	// running uninstall mid-reconcile does not crash the loop.
	existing, getErr := rec.client.Resource(GVR).Namespace(rec.namespace).
		Get(ctx, endpoint.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		return rec.create(ctx, endpoint.Name, uns)
	}

	if getErr != nil {
		return errors.Wrapf(getErr, "get DNSEndpoint %s/%s", rec.namespace, endpoint.Name)
	}

	// Same defence as the Service reconciler: name collision with a
	// foreign DNSEndpoint (different instance, or no managed-by label
	// at all) must NOT trigger a silent takeover. The prune path
	// label-filters; apply() must too. Operator must rename the foreign
	// CR or change externalDns.namespace.
	if !IsOwnedByOuroboros(existing, rec.instance) {
		return errors.Errorf(
			"refusing to overwrite DNSEndpoint %s/%s — name collides with a non-ouroboros-owned object "+
				"(missing or wrong %s=%s / %s=%s labels). Rename the foreign DNSEndpoint or change "+
				"externalDns.namespace; ouroboros will not silently take ownership of an existing object.",
			rec.namespace, endpoint.Name,
			LabelManagedBy, ManagedByValue, LabelInstance, rec.instance)
	}

	// Equality short-circuit: skip Update when our owned labels,
	// annotations, and spec already match. external-dns watches by
	// resourceVersion and re-publishes records on every generation
	// bump, so a no-op Update per resync interval (default 10 min ×
	// N hosts) translates to upstream provider churn.
	if reflect.DeepEqual(existing.GetLabels(), uns.GetLabels()) &&
		reflect.DeepEqual(existing.GetAnnotations(), uns.GetAnnotations()) &&
		reflect.DeepEqual(existing.Object["spec"], uns.Object["spec"]) {
		return nil
	}

	return rec.update(ctx, endpoint.Name, uns, existing)
}

func (rec *Reconciler) create(ctx context.Context, name string, uns *unstructured.Unstructured) error {
	_, createErr := rec.client.Resource(GVR).Namespace(rec.namespace).
		Create(ctx, uns, metav1.CreateOptions{FieldManager: fieldManager})
	if createErr == nil || apierrors.IsAlreadyExists(createErr) {
		return nil
	}

	return errors.Wrapf(createErr, "create DNSEndpoint %s/%s", rec.namespace, name)
}

func (rec *Reconciler) update(
	ctx context.Context,
	name string,
	uns, existing *unstructured.Unstructured,
) error {
	uns.SetResourceVersion(existing.GetResourceVersion())
	uns.SetUID(existing.GetUID())

	_, updateErr := rec.client.Resource(GVR).Namespace(rec.namespace).
		Update(ctx, uns, metav1.UpdateOptions{FieldManager: fieldManager})
	if updateErr != nil && !apierrors.IsNotFound(updateErr) {
		return errors.Wrapf(updateErr, "update DNSEndpoint %s/%s", rec.namespace, name)
	}

	return nil
}

// guardMassPrune decides whether the upcoming prune pass should run.
// Returns (skip=true, nil) when the empty-hosts safety net fires (no
// hosts but owned records exist) — the caller should return cleanly.
// Returns (skip=false, error) when every Build failed despite a
// non-empty hosts list — the workqueue retries.
// Returns (skip=false, nil) for the normal path.
func (rec *Reconciler) guardMassPrune(
	hosts []string,
	desired map[string]Endpoint,
	owned []*unstructured.Unstructured,
) (bool, error) {
	if len(desired) > 0 || len(owned) == 0 {
		return false, nil
	}

	if len(hosts) == 0 {
		rec.log.Warn(
			"externaldns reconcile: hosts list is empty but ouroboros-owned DNSEndpoints exist; "+
				"skipping prune to avoid silent mass-delete. If this is intentional, "+
				"uninstall the chart (or remove the manifests); the cleanup hook will "+
				"reap the records. Otherwise re-add at least one Ingress/HTTPRoute hostname.",
			slog.Int("ownedCount", len(owned)),
			slog.String("namespace", rec.namespace),
		)

		return true, nil
	}

	return false, errors.New(
		"externaldns reconcile: every host failed to build a DNSEndpoint — " +
			"refusing to run prune (would delete every ouroboros-owned record); " +
			"check earlier 'skipping host with build failure' warnings for the cause")
}

// listOwned returns the current set of ouroboros-owned DNSEndpoints in
// the records namespace. Pulled out of prune so Reconcile can use the
// same list both for the empty-hosts mass-prune guard and for the
// actual prune pass — no double round-trip.
func (rec *Reconciler) listOwned(ctx context.Context) ([]*unstructured.Unstructured, error) {
	list, err := rec.client.Resource(GVR).Namespace(rec.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: OwnershipSelector(rec.instance),
	})
	if err != nil {
		return nil, errors.Wrap(err, "list dnsendpoints")
	}

	out := make([]*unstructured.Unstructured, 0, len(list.Items))

	for index := range list.Items {
		item := &list.Items[index]
		if !IsOwnedByOuroboros(item, rec.instance) {
			// LabelSelector should already exclude these, but defence in
			// depth: never delete what we don't own.
			continue
		}

		out = append(out, item)
	}

	return out, nil
}

// pruneFromList deletes ouroboros-owned objects in `owned` whose names are
// not in `desired`, returning the survivors (objects still desired) for
// the status surfacer. Caller supplies the pre-listed `owned` slice so
// the empty-hosts guard in Reconcile can decide based on the same view.
func (rec *Reconciler) pruneFromList(
	ctx context.Context,
	desired map[string]Endpoint,
	owned []*unstructured.Unstructured,
) ([]*unstructured.Unstructured, error) {
	surviving := make([]*unstructured.Unstructured, 0, len(owned))

	for _, item := range owned {
		if _, want := desired[item.GetName()]; want {
			surviving = append(surviving, item)

			continue
		}

		delErr := rec.client.Resource(GVR).Namespace(rec.namespace).
			Delete(ctx, item.GetName(), metav1.DeleteOptions{})
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			return nil, errors.Wrapf(delErr, "delete stale DNSEndpoint %s", item.GetName())
		}
	}

	return surviving, nil
}
