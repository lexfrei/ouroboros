// Package k8s constructs Kubernetes and Gateway-API clientsets from either an
// in-cluster ServiceAccount or a kubeconfig file.
package k8s

import (
	"github.com/cockroachdb/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// Clients bundles the typed and dynamic clients ouroboros uses. Dynamic is
// needed for external-dns mode, which writes DNSEndpoint CRs without pulling
// in the full external-dns dep tree.
type Clients struct {
	Core    kubernetes.Interface
	Gateway gatewayclient.Interface
	Dynamic dynamic.Interface
}

// Build constructs the typed and dynamic clients. kubeconfigPath == "" forces
// in-cluster configuration.
func Build(kubeconfigPath string) (Clients, error) {
	cfg, cfgErr := loadConfig(kubeconfigPath)
	if cfgErr != nil {
		return Clients{}, cfgErr
	}

	core, coreErr := kubernetes.NewForConfig(cfg)
	if coreErr != nil {
		return Clients{}, errors.Wrap(coreErr, "build core client")
	}

	gateway, gwErr := gatewayclient.NewForConfig(cfg)
	if gwErr != nil {
		return Clients{}, errors.Wrap(gwErr, "build gateway-api client")
	}

	dyn, dynErr := dynamic.NewForConfig(cfg)
	if dynErr != nil {
		return Clients{}, errors.Wrap(dynErr, "build dynamic client")
	}

	return Clients{Core: core, Gateway: gateway, Dynamic: dyn}, nil
}

func loadConfig(path string) (*rest.Config, error) {
	if path == "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, errors.Wrap(err, "in-cluster kubeconfig")
		}

		return cfg, nil
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, errors.Wrapf(err, "load kubeconfig %s", path)
	}

	return cfg, nil
}
