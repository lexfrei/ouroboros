package externaldns

import (
	"context"
	"log/slog"

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
// Same defence-in-depth as the DNSEndpoint reconciler: when every Build
// fails (catastrophic config), refuse to prune so we do not delete
// every owned Service in the namespace.
func (rec *ServiceReconciler) Reconcile(ctx context.Context, hosts []string) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return errors.Wrap(ctxErr, "service reconcile: context already canceled")
	}

	desired := rec.buildDesired(hosts)

	if len(hosts) > 0 && len(desired) == 0 {
		return errors.New(
			"service reconcile: every host failed to build a Service — " +
				"refusing to run prune (would delete every ouroboros-owned Service); " +
				"check earlier 'skipping host with build failure' warnings for the cause")
	}

	for name := range desired {
		svc := desired[name]

		applyErr := rec.apply(ctx, svc)
		if applyErr != nil {
			return applyErr
		}
	}

	return rec.prune(ctx, desired)
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

	// Preserve resourceVersion + ClusterIP; Service IP is immutable post
	// creation, and on a headless Service it is set to "None" anyway,
	// so this only protects against an accidental allocation if the
	// existing Service has a different ClusterIP type.
	svc.ResourceVersion = existing.ResourceVersion
	svc.Spec.ClusterIP = existing.Spec.ClusterIP
	svc.Spec.ClusterIPs = existing.Spec.ClusterIPs

	_, updateErr := rec.client.CoreV1().Services(rec.namespace).
		Update(ctx, svc, metav1.UpdateOptions{FieldManager: fieldManager})
	if updateErr != nil && !apierrors.IsNotFound(updateErr) {
		return errors.Wrapf(updateErr, "update Service %s/%s", rec.namespace, svc.Name)
	}

	return nil
}

func (rec *ServiceReconciler) prune(ctx context.Context, desired map[string]*corev1.Service) error {
	list, err := rec.client.CoreV1().Services(rec.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: OwnershipSelector(rec.instance),
	})
	if err != nil {
		return errors.Wrap(err, "list ouroboros-owned Services")
	}

	for index := range list.Items {
		item := &list.Items[index]

		if !isOwnedService(item, rec.instance) {
			continue
		}

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
