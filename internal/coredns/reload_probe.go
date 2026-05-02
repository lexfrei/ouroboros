package coredns

import (
	"context"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WarnIfCorednsReloadMissing logs a Warn-level signal when the main CoreDNS
// Corefile lacks the [`reload` plugin](https://coredns.io/plugins/reload/).
// Without it, CoreDNS does not pick up ConfigMap changes and ouroboros
// edits (whether inline in coredns mode or via the import directive in
// coredns-import mode) only take effect after a manual pod restart — a
// silent failure mode this probe surfaces at startup.
//
// The probe is best-effort:
//   - NotFound (Corefile not at the expected location): silent. The
//     operator may have CoreDNS in a different namespace; ouroboros
//     can't help here.
//   - Forbidden (RBAC does not grant Get on the main coredns ConfigMap;
//     this is the expected case in coredns-import mode where the chart
//     deliberately narrows the controller's RBAC to the import
//     ConfigMap only): downgraded to Debug.
//   - Other transient errors: also Debug, to avoid flooding logs at
//     startup against a slow apiserver.
//   - Successful Get with reload plugin present: silent.
//   - Successful Get with reload plugin absent: Warn.
//
// The corefileKey argument is the data-key inside the ConfigMap that
// holds the Corefile (typically "Corefile"); supplied by the caller so
// the probe stays generic over distros that name it differently.
func WarnIfCorednsReloadMissing(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, configMap, corefileKey string,
	log *slog.Logger,
) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	configMapObj, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, configMap, metav1.GetOptions{})
	if err != nil {
		debugReloadProbeError(log, namespace, configMap, err)

		return
	}

	corefile, hasKey := configMapObj.Data[corefileKey]
	if !hasKey {
		log.Debug("CoreDNS ConfigMap is missing the Corefile data key; skipping reload-plugin probe",
			slog.String("namespace", namespace),
			slog.String("configmap", configMap),
			slog.String("key", corefileKey))

		return
	}

	if HasReloadPlugin(corefile) {
		return
	}

	log.Warn(
		"CoreDNS Corefile lacks the 'reload' plugin; ouroboros writes will succeed "+
			"but CoreDNS will not pick them up until pods are restarted manually. "+
			"Add the 'reload' plugin to the .:53 server block, or restart CoreDNS "+
			"pods after each ouroboros reconcile.",
		slog.String("namespace", namespace),
		slog.String("configmap", configMap),
	)
}

func debugReloadProbeError(log *slog.Logger, namespace, configMap string, err error) {
	if apierrors.IsNotFound(err) {
		return
	}

	if apierrors.IsForbidden(err) {
		log.Debug("CoreDNS reload-plugin probe denied by RBAC; cannot verify reload is configured",
			slog.String("namespace", namespace),
			slog.String("configmap", configMap),
		)

		return
	}

	log.Debug("CoreDNS reload-plugin probe failed",
		slog.String("namespace", namespace),
		slog.String("configmap", configMap),
		slog.String("error", err.Error()))
}
