package externaldns

import (
	"context"
	"log/slog"
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
		surfacer:    cfg.Surfacer,
		log:         log,
	}, nil
}

// Reconcile applies the desired host set to DNSEndpoint CRs using
// server-side apply, then deletes any ouroboros-owned objects whose name no
// longer matches the desired set. The reconciler is safe to call from a
// rate-limited workqueue; every call is a single-pass: build → SSA → list →
// prune → status surface.
func (rec *Reconciler) Reconcile(ctx context.Context, hosts []string) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return errors.Wrap(ctxErr, "externaldns reconcile: context already canceled")
	}

	desired := rec.buildDesired(hosts)

	for name := range desired {
		endpoint := desired[name]

		applyErr := rec.apply(ctx, &endpoint)
		if applyErr != nil {
			return applyErr
		}
	}

	surviving, pruneErr := rec.prune(ctx, desired)
	if pruneErr != nil {
		return pruneErr
	}

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
		_, createErr := rec.client.Resource(GVR).Namespace(rec.namespace).
			Create(ctx, uns, metav1.CreateOptions{FieldManager: fieldManager})
		if createErr == nil || apierrors.IsAlreadyExists(createErr) {
			return nil
		}

		return errors.Wrapf(createErr, "create DNSEndpoint %s/%s", rec.namespace, endpoint.Name)
	}

	if getErr != nil {
		return errors.Wrapf(getErr, "get DNSEndpoint %s/%s", rec.namespace, endpoint.Name)
	}

	uns.SetResourceVersion(existing.GetResourceVersion())
	uns.SetUID(existing.GetUID())

	_, updateErr := rec.client.Resource(GVR).Namespace(rec.namespace).
		Update(ctx, uns, metav1.UpdateOptions{FieldManager: fieldManager})
	if updateErr != nil && !apierrors.IsNotFound(updateErr) {
		return errors.Wrapf(updateErr, "update DNSEndpoint %s/%s", rec.namespace, endpoint.Name)
	}

	return nil
}

func (rec *Reconciler) prune(ctx context.Context, desired map[string]Endpoint) ([]*unstructured.Unstructured, error) {
	list, err := rec.client.Resource(GVR).Namespace(rec.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: OwnershipSelector(rec.instance),
	})
	if err != nil {
		return nil, errors.Wrap(err, "list ouroboros-owned DNSEndpoints")
	}

	surviving := make([]*unstructured.Unstructured, 0, len(list.Items))

	for index := range list.Items {
		item := &list.Items[index]

		if !IsOwnedByOuroboros(item, rec.instance) {
			// LabelSelector should already exclude these, but defence in
			// depth: never delete what we don't own.
			continue
		}

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
