package coredns

import (
	"context"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// nodeLocalDNSNamespace and nodeLocalDNSConfigMap are the well-known
// upstream defaults for node-local-dns. Changing them on a cluster is
// rare enough that a configurable override would be more confusing than
// useful — operators on non-default deployments can ignore the warning.
const (
	nodeLocalDNSNamespace = "kube-system"
	nodeLocalDNSConfigMap = "node-local-dns"
)

// WarnIfNodeLocalDNSDetected logs a Warn-level signal when the cluster has a
// node-local-dns ConfigMap. Coredns mode rewrites kube-system/coredns, but
// pods on a node-local-dns-equipped cluster ask the per-node cache first;
// for non-cluster.local queries (which is exactly the hairpin case),
// node-local-dns forwards UPSTREAM, bypassing CoreDNS entirely. The
// rewrite block ouroboros writes is invisible to those queries and the
// hairpin silently does not work.
//
// We do NOT auto-mutate node-local-dns: its Corefile uses pillar-template
// tokens (__PILLAR__UPSTREAM__SERVERS__) and zone scopes that are gnarly
// to safely transform without per-cluster knowledge. The warning points
// operators at the two reliable workarounds:
//
//  1. Switch controller.mode to external-dns — the DNSEndpoint records flow
//     through whatever provider/CCM the cluster uses, independent of
//     node-local-dns's caching layer.
//  2. Manually add the same rewrite directives to the node-local-dns
//     Corefile block(s) that handle external queries.
//
// Errors other than NotFound are downgraded to Debug so transient API
// hiccups during startup do not flood the log.
func WarnIfNodeLocalDNSDetected(ctx context.Context, client kubernetes.Interface, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	_, err := client.CoreV1().ConfigMaps(nodeLocalDNSNamespace).
		Get(ctx, nodeLocalDNSConfigMap, metav1.GetOptions{})

	if apierrors.IsNotFound(err) {
		return
	}

	if err != nil {
		log.Debug("node-local-dns detection probe failed",
			slog.String("namespace", nodeLocalDNSNamespace),
			slog.String("configmap", nodeLocalDNSConfigMap),
			slog.String("error", err.Error()))

		return
	}

	log.Warn(
		"node-local-dns is deployed; coredns mode does NOT cover pods that "+
			"resolve through it. Pods query node-local-dns first, which forwards "+
			"non-cluster.local queries upstream and bypasses the CoreDNS rewrite "+
			"block ouroboros writes. Hairpin will silently fail for those pods. "+
			"Recommendation: switch controller.mode to external-dns, OR add the "+
			"same rewrite directives manually to the node-local-dns Corefile.",
		slog.String("nodeLocalDNSConfigMap", nodeLocalDNSNamespace+"/"+nodeLocalDNSConfigMap),
	)
}
