package externaldns

import (
	"context"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// WarnIfOtherOutputHasOrphans logs a Warn at startup when ouroboros-owned
// objects of the OTHER output kind exist in the records namespace —
// signaling that the operator switched externalDns.output between
// 'crd' and 'service' on a live release without manually cleaning up
// the previous kind. The Reconciler only manages its active kind, and
// the post-delete cleanup hook only fires on `helm uninstall`, so a
// flip leaves stale records behind unless the operator runs a manual
// kubectl delete.
//
// activeOutput is the new mode (one of "crd", "service"); the function
// probes the OPPOSITE kind. The probe is best-effort: an apiserver
// error or 403 (e.g. RBAC for the other kind missing — expected, since
// the chart only grants verbs for the active kind) is downgraded to
// Debug.
func WarnIfOtherOutputHasOrphans(
	ctx context.Context,
	core kubernetes.Interface,
	dyn dynamic.Interface,
	namespace, instance, activeOutput string,
	log *slog.Logger,
) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	switch activeOutput {
	case "service":
		warnIfDNSEndpointOrphans(ctx, dyn, namespace, instance, log)
	case "crd":
		warnIfServiceOrphans(ctx, core, namespace, instance, log)
	}
}

func warnIfDNSEndpointOrphans(ctx context.Context, dyn dynamic.Interface, namespace, instance string, log *slog.Logger) {
	if dyn == nil {
		return
	}

	list, err := dyn.Resource(GVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: OwnershipSelector(instance),
		Limit:         1,
	})

	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		// CRD missing entirely (clean install) or no RBAC for the other
		// kind (chart correctly didn't grant it) — both safe quiet.
		return
	}

	if err != nil {
		log.Debug("orphan probe (DNSEndpoint) failed", slog.String("error", err.Error()))

		return
	}

	if len(list.Items) == 0 {
		return
	}

	log.Warn(
		"externalDns.output=service is active but ouroboros-owned DNSEndpoint CRs "+
			"still exist in the records namespace from a previous output=crd "+
			"deployment. external-dns will continue honouring those CRs and DNS "+
			"will bifurcate. Clean up with: kubectl --namespace="+namespace+
			" delete dnsendpoints.externaldns.k8s.io "+
			"--selector='"+OwnershipSelector(instance)+"'",
		slog.Int("orphanCount", len(list.Items)),
		slog.String("namespace", namespace),
	)
}

func warnIfServiceOrphans(ctx context.Context, core kubernetes.Interface, namespace, instance string, log *slog.Logger) {
	if core == nil {
		return
	}

	list, err := core.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: OwnershipSelector(instance),
		Limit:         1,
	})

	if apierrors.IsForbidden(err) {
		return
	}

	if err != nil {
		log.Debug("orphan probe (Service) failed", slog.String("error", err.Error()))

		return
	}

	if len(list.Items) == 0 {
		return
	}

	log.Warn(
		"externalDns.output=crd is active but ouroboros-owned Services still "+
			"exist in the records namespace from a previous output=service "+
			"deployment. external-dns will continue honouring those Services and "+
			"DNS will bifurcate. Clean up with: kubectl --namespace="+namespace+
			" delete services "+
			"--selector='"+OwnershipSelector(instance)+"'",
		slog.Int("orphanCount", len(list.Items)),
		slog.String("namespace", namespace),
	)
}
