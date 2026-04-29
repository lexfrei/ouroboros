// Package k8s constructs Kubernetes and Gateway-API clientsets from either an
// in-cluster ServiceAccount or a kubeconfig file.
package k8s

import (
	"github.com/cockroachdb/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// Clients bundles the typed clients ouroboros uses.
type Clients struct {
	Core    kubernetes.Interface
	Gateway gatewayclient.Interface
}

// Build constructs the typed clients. kubeconfigPath == "" forces in-cluster
// configuration.
func Build(kubeconfigPath string) (Clients, error) {
	cfg, cfgErr := loadConfig(kubeconfigPath)
	if cfgErr != nil {
		return Clients{}, cfgErr
	}

	core, coreErr := kubernetes.NewForConfig(cfg)
	if coreErr != nil {
		return Clients{}, errors.Wrap(coreErr, "build core client")
	}

	gw, gwErr := gatewayclient.NewForConfig(cfg)
	if gwErr != nil {
		return Clients{}, errors.Wrap(gwErr, "build gateway-api client")
	}

	return Clients{Core: core, Gateway: gw}, nil
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
