package coredns

import (
	"context"
	"log/slog"

	"github.com/cockroachdb/errors"
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
func (r *ImportReconciler) Reconcile(_ context.Context, _ []string) (bool, error) {
	return false, errors.New("not implemented")
}

// BuildImportSnippet renders the plugin-only snippet for the import
// ConfigMap. The resulting content is one `rewrite name <host> <target>.`
// line per host, sorted, deduplicated, lowercased, and with wildcard hosts
// silently dropped (CoreDNS rewrite-name patterns are exact-match only).
//
// An empty host set yields an empty string — callers map that to "delete the
// data key" rather than "write empty data".
func BuildImportSnippet(_ []string, _ string) (string, error) {
	return "", errors.New("not implemented")
}
