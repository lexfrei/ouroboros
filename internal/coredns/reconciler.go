package coredns

import (
	"context"
	"log/slog"

	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
	return reconcileWithRetry(ctx, "CoreDNS", func() (bool, error) {
		return r.reconcileOnce(ctx, hosts)
	})
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
		return false, wrapConfigMapUpdateErr(updateErr, r.namespace, r.configMap)
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
