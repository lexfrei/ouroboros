package coredns

import (
	"context"
	"log/slog"

	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// Reconciler synchronises the ouroboros block in the CoreDNS Corefile with
// a desired set of hostnames. The expected ConfigMap layout is the standard
// Kubernetes one (kube-system/coredns with a "Corefile" key) but namespace,
// name, and key are configurable.
type Reconciler struct {
	client      kubernetes.Interface
	namespace   string
	configMap   string
	corefileKey string
	target      string
	log         *slog.Logger
}

// NewReconciler builds a Reconciler. log == nil silences output.
func NewReconciler(client kubernetes.Interface, namespace, configMap, corefileKey, target string, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return &Reconciler{
		client:      client,
		namespace:   namespace,
		configMap:   configMap,
		corefileKey: corefileKey,
		target:      target,
		log:         log,
	}
}

// Reconcile fetches the CoreDNS ConfigMap, applies the ouroboros block for
// the supplied hosts, and writes back via Update with retry-on-conflict. It
// returns whether the ConfigMap content actually changed.
//
// Errors:
//   - ConfigMap missing
//   - Corefile key missing in ConfigMap.Data
//   - Apply rejects the Corefile (no .:53 server block, etc.)
//   - context canceled before/while updating
//   - retry budget exhausted on persistent conflicts
func (r *Reconciler) Reconcile(ctx context.Context, hosts []string) (bool, error) {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return false, errors.Wrap(ctxErr, "context canceled before reconcile")
	}

	var changed bool

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		didChange, err := r.reconcileOnce(ctx, hosts)
		if err != nil {
			return err
		}

		changed = didChange

		return nil
	})
	if retryErr != nil {
		return false, errors.Wrap(retryErr, "reconcile CoreDNS")
	}

	return changed, nil
}

func (r *Reconciler) reconcileOnce(ctx context.Context, hosts []string) (bool, error) {
	configMap, getErr := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, r.configMap, metav1.GetOptions{})
	if getErr != nil {
		return false, errors.Wrapf(getErr, "get configmap %s/%s", r.namespace, r.configMap)
	}

	current, ok := configMap.Data[r.corefileKey]
	if !ok {
		return false, errors.Errorf("configmap %s/%s has no %q key", r.namespace, r.configMap, r.corefileKey)
	}

	updated, didChange, applyErr := Apply(current, hosts, r.target)
	if applyErr != nil {
		return false, errors.Wrap(applyErr, "apply Corefile mutation")
	}

	if !didChange {
		return false, nil
	}

	r.warnIfNoReload(updated)

	configMap.Data[r.corefileKey] = updated

	_, updateErr := r.client.CoreV1().ConfigMaps(r.namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	if updateErr != nil {
		return false, r.wrapUpdateErr(updateErr)
	}

	return true, nil
}

func (r *Reconciler) warnIfNoReload(corefile string) {
	if HasReloadPlugin(corefile) {
		return
	}

	r.log.Warn(
		"CoreDNS Corefile lacks the 'reload' plugin; ouroboros block will be applied "+
			"but CoreDNS will not pick it up until pods are restarted manually",
		"namespace", r.namespace,
		"configmap", r.configMap,
	)
}

func (r *Reconciler) wrapUpdateErr(err error) error {
	if apierrors.IsConflict(err) {
		return errors.Wrapf(err, "update configmap %s/%s (conflict)", r.namespace, r.configMap)
	}

	return errors.Wrapf(err, "update configmap %s/%s", r.namespace, r.configMap)
}
