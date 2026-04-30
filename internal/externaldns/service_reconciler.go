package externaldns

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ServiceReconcilerConfig is the constructor input for ServiceReconciler.
// Mirrors ReconcilerConfig but uses a typed core client (Services are a
// core/v1 type — no dynamic client needed) and adds AnnotationPrefix
// because Service-mode external-dns instances key off the prefix.
type ServiceReconcilerConfig struct {
	Client           kubernetes.Interface
	Namespace        string
	Instance         string
	Targets          []string
	TTL              int64
	Source           Source
	Annotations      map[string]string
	Labels           map[string]string
	AnnotationPrefix string
	Log              *slog.Logger
}

// ServiceReconciler emits annotated headless Services per host (one per
// hostname, dual-stack carried in a single comma-separated 'target'
// annotation) and prunes ouroboros-owned Services that no longer
// correspond to a host. Used by external-dns instances with
// --source=service that filter via --annotation-prefix.
type ServiceReconciler struct {
	client           kubernetes.Interface
	namespace        string
	instance         string
	targets          []string
	ttl              int64
	source           Source
	annotations      map[string]string
	labels           map[string]string
	annotationPrefix string
	log              *slog.Logger
}

// NewServiceReconciler validates cfg and returns a ServiceReconciler.
// Same fail-fast semantics as the DNSEndpoint Reconciler — missing
// client, empty AnnotationPrefix, or empty Targets are rejected at
// startup so the controller does not spin emitting bad Services.
func NewServiceReconciler(cfg *ServiceReconcilerConfig) (*ServiceReconciler, error) {
	if cfg.Client == nil {
		return nil, errors.New("NewServiceReconciler: typed core client is required")
	}

	if cfg.Namespace == "" {
		return nil, errors.New("NewServiceReconciler: namespace is required")
	}

	if cfg.Instance == "" {
		return nil, errors.New("NewServiceReconciler: instance is required")
	}

	if len(cfg.Targets) == 0 {
		return nil, errors.New("NewServiceReconciler: at least one target IP is required")
	}

	if cfg.AnnotationPrefix == "" {
		return nil, errors.New("NewServiceReconciler: AnnotationPrefix is required (Services would be unaddressable)")
	}

	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	source := cfg.Source
	if source == "" {
		source = SourceController
	}

	return &ServiceReconciler{
		client:           cfg.Client,
		namespace:        cfg.Namespace,
		instance:         cfg.Instance,
		targets:          cfg.Targets,
		ttl:              cfg.TTL,
		source:           source,
		annotations:      cfg.Annotations,
		labels:           cfg.Labels,
		annotationPrefix: cfg.AnnotationPrefix,
		log:              log,
	}, nil
}

// Reconcile applies the desired host set to Service objects via
// Get→Update (Create-on-NotFound), then prunes ouroboros-owned Services
// whose name no longer matches the desired set.
//
// Two layers of mass-prune defence:
//
//  1. hosts is non-empty but every Build failed → desired is empty.
//     Catastrophic config (or upstream regression). Return an error so
//     the workqueue retries; do not prune.
//
//  2. hosts is empty (operator deleted all routes, replaced every
//     hostname with a wildcard, or the informer's cache is briefly
//     out-of-sync) and at least one ouroboros-owned Service exists.
//     This is the realistic accident-vs-intent ambiguity: a config
//     change can wipe all hostnames in one commit, and pruning silently
//     would erase every published DNS record. Skip prune, log a Warn
//     telling the operator that the cluster owns N records they can
//     remove explicitly via 'helm uninstall' or by adding back at
//     least one hostname. Apply still runs (it's a no-op when desired
//     is empty), so the reconciler returns cleanly.
func (rec *ServiceReconciler) Reconcile(ctx context.Context, hosts []string) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return errors.Wrap(ctxErr, "service reconcile: context already canceled")
	}

	desired := rec.buildDesired(hosts)

	owned, listErr := rec.listOwned(ctx)
	if listErr != nil {
		return errors.Wrap(listErr, "list ouroboros-owned Services")
	}

	skip, guardErr := rec.guardMassPrune(hosts, desired, owned)
	if guardErr != nil {
		return guardErr
	}

	if skip {
		return nil
	}

	for name := range desired {
		svc := desired[name]

		applyErr := rec.apply(ctx, svc)
		if applyErr != nil {
			return applyErr
		}
	}

	return rec.pruneFromList(ctx, desired, owned)
}

func (rec *ServiceReconciler) guardMassPrune(
	hosts []string,
	desired map[string]*corev1.Service,
	owned []corev1.Service,
) (bool, error) {
	if len(desired) > 0 || len(owned) == 0 {
		return false, nil
	}

	if len(hosts) == 0 {
		rec.log.Warn(
			"service reconcile: hosts list is empty but ouroboros-owned Services exist; "+
				"skipping prune to avoid silent mass-delete. If this is intentional, "+
				"run 'helm uninstall' to invoke the cleanup hook; otherwise re-add at "+
				"least one Ingress/HTTPRoute hostname.",
			slog.Int("ownedCount", len(owned)),
			slog.String("namespace", rec.namespace),
		)

		return true, nil
	}

	return false, errors.New(
		"service reconcile: every host failed to build a Service — " +
			"refusing to run prune (would delete every ouroboros-owned Service); " +
			"check earlier 'skipping host with build failure' warnings for the cause")
}

func (rec *ServiceReconciler) buildDesired(hosts []string) map[string]*corev1.Service {
	desired := make(map[string]*corev1.Service, len(hosts))

	for _, host := range hosts {
		svc, err := BuildService(&BuildServiceOpts{
			Host:             host,
			Targets:          rec.targets,
			TTL:              rec.ttl,
			Source:           rec.source,
			Instance:         rec.instance,
			Namespace:        rec.namespace,
			Annotations:      rec.annotations,
			Labels:           rec.labels,
			AnnotationPrefix: rec.annotationPrefix,
		})
		if err != nil {
			rec.log.Warn("externaldns service: skipping host with build failure",
				slog.String("host", host), slog.String("error", err.Error()))

			continue
		}

		desired[svc.Name] = svc
	}

	return desired
}

func (rec *ServiceReconciler) apply(ctx context.Context, svc *corev1.Service) error {
	existing, err := rec.client.CoreV1().Services(rec.namespace).
		Get(ctx, svc.Name, metav1.GetOptions{})

	if apierrors.IsNotFound(err) {
		_, createErr := rec.client.CoreV1().Services(rec.namespace).
			Create(ctx, svc, metav1.CreateOptions{FieldManager: fieldManager})
		if createErr == nil || apierrors.IsAlreadyExists(createErr) {
			return nil
		}

		return errors.Wrapf(createErr, "create Service %s/%s", rec.namespace, svc.Name)
	}

	if err != nil {
		return errors.Wrapf(err, "get Service %s/%s", rec.namespace, svc.Name)
	}

	// Defence against name collision with a foreign Service. The prune
	// path label-filters by ouroboros ownership, but apply() must do the
	// same before Update — without this check, a Service named
	// 'ouroboros-<rfc1123-host>' that belongs to another team in a
	// shared externalDns.namespace would be silently overwritten,
	// wiping its selector / ports and replacing labels. Refuse loudly:
	// the workqueue will keep retrying with the same error until the
	// operator renames the foreign object or moves externalDns.namespace.
	if !isOwnedService(existing, rec.instance) {
		return errors.Errorf(
			"refusing to overwrite Service %s/%s — name collides with a non-ouroboros-owned object "+
				"(missing or wrong %s=%s / %s=%s labels). Rename the foreign Service or change "+
				"externalDns.namespace; ouroboros will not silently take ownership of an existing object.",
			rec.namespace, svc.Name,
			LabelManagedBy, ManagedByValue, LabelInstance, rec.instance)
	}

	// Equality short-circuit: skip Update when our owned annotations and
	// labels already match. external-dns watches by resourceVersion and
	// re-publishes records on every generation bump, so a no-op Update
	// per resync interval (default 10 min × N hosts) translates to
	// upstream provider churn. Spec is excluded from the comparison
	// because we always reset apiserver-defaulted fields below — that
	// reset is also a no-op once existing reflects what we wrote on
	// the previous reconcile.
	if reflect.DeepEqual(existing.Labels, svc.Labels) &&
		reflect.DeepEqual(existing.Annotations, svc.Annotations) {
		return nil
	}

	// Preserve apiserver-defaulted spec fields verbatim. ClusterIP and
	// ClusterIPs are immutable post-creation. IPFamilies and
	// IPFamilyPolicy are immutable on dual-stack clusters with
	// RequireDualStack — Update with these fields zeroed returns
	// 'Invalid value: []core.IPFamily{}: primary ipFamily can not be
	// unset' on real apiservers (fake.NewSimpleClientset doesn't
	// validate, but real ones do). InternalTrafficPolicy and
	// SessionAffinity are not strictly immutable but get apiserver
	// defaults on Create — resetting them every reconcile would bump
	// generation when nothing meaningful changed. external-dns ignores
	// spec entirely, keying only off annotations + labels, so dragging
	// these fields forward from existing is invisible upstream.
	svc.ResourceVersion = existing.ResourceVersion
	svc.Spec.ClusterIP = existing.Spec.ClusterIP
	svc.Spec.ClusterIPs = existing.Spec.ClusterIPs
	svc.Spec.IPFamilies = existing.Spec.IPFamilies
	svc.Spec.IPFamilyPolicy = existing.Spec.IPFamilyPolicy
	svc.Spec.InternalTrafficPolicy = existing.Spec.InternalTrafficPolicy
	svc.Spec.SessionAffinity = existing.Spec.SessionAffinity

	_, updateErr := rec.client.CoreV1().Services(rec.namespace).
		Update(ctx, svc, metav1.UpdateOptions{FieldManager: fieldManager})
	if updateErr != nil && !apierrors.IsNotFound(updateErr) {
		return errors.Wrapf(updateErr, "update Service %s/%s", rec.namespace, svc.Name)
	}

	return nil
}

// listOwned returns the current set of ouroboros-owned Services in the
// records namespace. Pulled out of prune so Reconcile can use the same
// list both for the empty-hosts mass-prune guard and for the actual
// prune pass — no double round-trip.
func (rec *ServiceReconciler) listOwned(ctx context.Context) ([]corev1.Service, error) {
	list, err := rec.client.CoreV1().Services(rec.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: OwnershipSelector(rec.instance),
	})
	if err != nil {
		return nil, errors.Wrap(err, "list services")
	}

	out := make([]corev1.Service, 0, len(list.Items))

	for index := range list.Items {
		item := list.Items[index]
		if !isOwnedService(&item, rec.instance) {
			continue
		}

		out = append(out, item)
	}

	return out, nil
}

func (rec *ServiceReconciler) pruneFromList(
	ctx context.Context,
	desired map[string]*corev1.Service,
	owned []corev1.Service,
) error {
	for index := range owned {
		item := &owned[index]

		if _, want := desired[item.Name]; want {
			continue
		}

		delErr := rec.client.CoreV1().Services(rec.namespace).
			Delete(ctx, item.Name, metav1.DeleteOptions{})
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			return errors.Wrapf(delErr, "delete stale Service %s", item.Name)
		}
	}

	return nil
}

// isOwnedService is the typed twin of IsOwnedByOuroboros — same
// label-based contract, but on a *corev1.Service (which has typed
// labels access).
func isOwnedService(svc *corev1.Service, instance string) bool {
	if svc == nil {
		return false
	}

	labels := svc.Labels
	if labels == nil {
		return false
	}

	return labels[LabelManagedBy] == ManagedByValue && labels[LabelInstance] == instance
}
