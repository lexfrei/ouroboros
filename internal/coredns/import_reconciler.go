package coredns

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ImportReconciler synchronises a plugin-only CoreDNS rewrite snippet stored
// in a *separate* ConfigMap that the main Corefile pulls in via the `import`
// directive (e.g. `import /etc/coredns/custom/*.override`). Unlike Reconciler,
// this mode never touches the main kube-system/coredns Corefile, so it does
// not race a Helm/Kustomize/Flux owner that re-renders that Corefile.
//
// The ConfigMap layout is one data key (default "ouroboros.override") whose
// value is a sequence of `rewrite name <host> <target>.` lines, sorted and
// deduplicated. The CoreDNS `import` plugin parses each file as a snippet of
// plugin directives within the surrounding server-block, so no `.:53 { ... }`
// wrapping is needed.
type ImportReconciler struct {
	client    kubernetes.Interface
	namespace string
	configMap string
	dataKey   string
	target    string
	log       *slog.Logger
}

// NewImportReconciler builds an ImportReconciler. log == nil silences output.
func NewImportReconciler(
	client kubernetes.Interface,
	namespace, configMap, dataKey, target string,
	log *slog.Logger,
) *ImportReconciler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return &ImportReconciler{
		client:    client,
		namespace: namespace,
		configMap: configMap,
		dataKey:   dataKey,
		target:    target,
		log:       log,
	}
}

// Reconcile fetches the import ConfigMap, computes the desired snippet for
// the supplied hosts, and writes it back via Update with retry-on-conflict.
// Missing ConfigMap is treated as a hard error — the chart owns creation of
// an empty ConfigMap on install. Returns whether the data actually changed.
//
// Errors:
//   - ConfigMap missing
//   - target empty or missing trailing dot
//   - context canceled before/while updating
//   - retry budget exhausted on persistent conflicts
func (r *ImportReconciler) Reconcile(ctx context.Context, hosts []string) (bool, error) {
	return reconcileWithRetry(ctx, "coredns-import", func() (bool, error) {
		return r.reconcileOnce(ctx, hosts)
	})
}

func (r *ImportReconciler) reconcileOnce(ctx context.Context, hosts []string) (bool, error) {
	configMap, getErr := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, r.configMap, metav1.GetOptions{})
	if getErr != nil {
		return false, errors.Wrapf(getErr, "get configmap %s/%s", r.namespace, r.configMap)
	}

	desired, buildErr := BuildImportSnippet(hosts, r.target)
	if buildErr != nil {
		return false, errors.Wrap(buildErr, "build import snippet")
	}

	if configMap.Data == nil {
		configMap.Data = make(map[string]string, 1)
	}

	if !ApplyImportData(configMap.Data, r.dataKey, desired) {
		return false, nil
	}

	_, updateErr := r.client.CoreV1().ConfigMaps(r.namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	if updateErr != nil {
		return false, wrapConfigMapUpdateErr(updateErr, r.namespace, r.configMap)
	}

	return true, nil
}

// ApplyImportData mutates data to reflect desired snippet content under
// dataKey and returns whether anything actually changed. Empty desired
// content removes the key (whether the existing value was empty or not).
// The data map must be non-nil.
//
// The four-branch contract:
//   - desired empty, no existing key  → noop, return false
//   - desired empty, key exists       → delete key, return true
//   - desired equals existing value   → noop, return false
//   - otherwise                       → write desired, return true
//
// Exported so unit tests can pin every branch in isolation; the empty-
// existing/empty-desired path used to fall through to the equality
// branch and silently retain the key (regression #3 from review pass 2).
func ApplyImportData(data map[string]string, dataKey, desired string) bool {
	existing, exists := data[dataKey]

	switch {
	case desired == "" && !exists:
		return false
	case desired == "":
		delete(data, dataKey)

		return true
	case desired == existing && exists:
		return false
	default:
		data[dataKey] = desired

		return true
	}
}

// BuildImportSnippet renders the plugin-only snippet for the import
// ConfigMap. The resulting content is one `rewrite name <host> <target>.`
// line per host, sorted, deduplicated, lowercased, and with wildcard hosts
// silently dropped (CoreDNS rewrite-name patterns are exact-match only).
//
// An empty host set yields an empty string — callers map that to "delete the
// data key" rather than "write empty data".
func BuildImportSnippet(hosts []string, target string) (string, error) {
	if target == "" {
		return "", errors.New("empty target")
	}

	if !strings.HasSuffix(target, ".") {
		return "", errors.New("target must be FQDN with trailing dot")
	}

	cleaned := normalizeHosts(hosts)
	if len(cleaned) == 0 {
		return "", nil
	}

	var builder strings.Builder

	for _, host := range cleaned {
		builder.WriteString("rewrite name ")
		builder.WriteString(host)
		builder.WriteByte(' ')
		builder.WriteString(target)
		builder.WriteByte('\n')
	}

	return builder.String(), nil
}
