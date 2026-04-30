package k8s

import (
	"context"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ResolveProxyClusterIP looks up the ouroboros-proxy Service and returns its
// allocated ClusterIP. external-dns mode requires a stable A-record target;
// headless Services and Services that have not yet been allocated an IP are
// rejected with a wrapped error so the controller fails fast at startup
// instead of publishing junk DNS records.
func ResolveProxyClusterIP(ctx context.Context, client kubernetes.Interface, namespace, name string) (string, error) {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return "", errors.Wrap(ctxErr, "resolve proxy ClusterIP")
	}

	svc, err := client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", errors.Wrapf(err, "resolve proxy ClusterIP for service %s/%s", namespace, name)
	}

	clusterIP := svc.Spec.ClusterIP

	if clusterIP == corev1.ClusterIPNone {
		return "", errors.Errorf("service %s/%s is headless (ClusterIP=None) and cannot be used as an A-record target", namespace, name)
	}

	if clusterIP == "" {
		return "", errors.Errorf("service %s/%s has no ClusterIP allocated yet", namespace, name)
	}

	return clusterIP, nil
}

// ResolveProxyClusterIPs returns every ClusterIP allocated to the proxy
// Service. Dual-stack Services populate spec.clusterIPs[] with both v4 and v6
// addresses; ouroboros emits one DNSEndpoint per family. Older API servers
// may populate only spec.clusterIP — in that case the singular value is
// returned as a one-element slice. Headless and unallocated Services are
// rejected for the same reason as the singular variant.
func ResolveProxyClusterIPs(ctx context.Context, client kubernetes.Interface, namespace, name string) ([]string, error) {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return nil, errors.Wrap(ctxErr, "resolve proxy ClusterIPs")
	}

	svc, err := client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "resolve proxy ClusterIPs for service %s/%s", namespace, name)
	}

	if svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return nil, errors.Errorf("service %s/%s is headless (ClusterIP=None) and cannot be used as an A-record target", namespace, name)
	}

	ips := svc.Spec.ClusterIPs
	if len(ips) == 0 && svc.Spec.ClusterIP != "" {
		ips = []string{svc.Spec.ClusterIP}
	}

	if len(ips) == 0 {
		return nil, errors.Errorf("service %s/%s has no ClusterIP allocated yet", namespace, name)
	}

	return ips, nil
}
