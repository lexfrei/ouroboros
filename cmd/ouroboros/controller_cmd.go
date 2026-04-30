package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/cockroachdb/errors"

	"github.com/lexfrei/ouroboros/internal/config"
	"github.com/lexfrei/ouroboros/internal/controller"
	"github.com/lexfrei/ouroboros/internal/coredns"
	"github.com/lexfrei/ouroboros/internal/externaldns"
	"github.com/lexfrei/ouroboros/internal/hosts"
	"github.com/lexfrei/ouroboros/internal/k8s"
)

// podNamespaceFile is where the downward API mounts the pod's own namespace
// when the controller is running in-cluster. Used as the fallback for
// ExternalDNSNamespace when the operator did not specify one.
const podNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

func runController(ctx context.Context, logger *slog.Logger, args []string) error {
	cfg, parseErr := config.ParseControllerFlags(args)
	if parseErr != nil {
		return errors.Wrap(parseErr, "parse controller flags")
	}

	clients, clientsErr := k8s.Build(cfg.KubeConfig)
	if clientsErr != nil {
		return errors.Wrap(clientsErr, "build clients")
	}

	reconcile, reconcileErr := buildReconcileFunc(ctx, &cfg, clients, logger)
	if reconcileErr != nil {
		return reconcileErr
	}

	ctrl := controller.New(controller.Options{
		Core:         clients.Core,
		Gateway:      clients.Gateway,
		EnableGW:     cfg.EnableGatewayAPI,
		Reconciler:   reconcile,
		ResyncPeriod: cfg.ResyncPeriod,
		Logger:       logger,
	})

	logger.Info("controller starting",
		"mode", string(cfg.Mode),
		"gateway-api", cfg.EnableGatewayAPI,
		"resync", cfg.ResyncPeriod)

	runErr := ctrl.Run(ctx)
	if runErr != nil {
		return errors.Wrap(runErr, "controller run")
	}

	return nil
}

func buildReconcileFunc(
	ctx context.Context,
	cfg *config.ControllerConfig,
	clients k8s.Clients,
	logger *slog.Logger,
) (controller.ReconcileFunc, error) {
	switch cfg.Mode {
	case config.ModeCoreDNS:
		return buildCoreDNSReconcile(cfg, clients, logger), nil
	case config.ModeEtcHosts:
		rec := &hosts.Reconciler{Path: cfg.EtcHostsPath, ProxyIP: cfg.ProxyIP}

		return rec.Reconcile, nil
	case config.ModeExternalDNS:
		return buildExternalDNSReconcile(ctx, cfg, clients, logger)
	default:
		return nil, errors.Errorf("unknown mode %q", cfg.Mode)
	}
}

func buildCoreDNSReconcile(
	cfg *config.ControllerConfig,
	clients k8s.Clients,
	logger *slog.Logger,
) controller.ReconcileFunc {
	rec := coredns.NewReconciler(
		clients.Core,
		cfg.CorednsNamespace,
		cfg.CorednsConfigMap,
		cfg.CorednsKey,
		cfg.ProxyFQDN,
		logger,
	)

	return func(ctx context.Context, names []string) error {
		_, err := rec.Reconcile(ctx, names)
		if err != nil {
			return errors.Wrap(err, "coredns reconcile")
		}

		return nil
	}
}

func buildExternalDNSReconcile(
	ctx context.Context,
	cfg *config.ControllerConfig,
	clients k8s.Clients,
	logger *slog.Logger,
) (controller.ReconcileFunc, error) {
	namespace := cfg.ExternalDNSNamespace
	if namespace == "" {
		ns, nsErr := readPodNamespace()
		if nsErr != nil {
			return nil, errors.Wrap(nsErr, "external-dns mode: ExternalDNSNamespace not set and pod namespace lookup failed")
		}

		namespace = ns
	}

	target := cfg.ExternalDNSProxyIP
	if target == "" {
		resolved, resolveErr := k8s.ResolveProxyClusterIP(ctx, clients.Core, namespace, cfg.ExternalDNSProxyService)
		if resolveErr != nil {
			return nil, errors.Wrap(resolveErr, "external-dns mode: discover proxy ClusterIP")
		}

		target = resolved
	}

	surfacer := externaldns.NewStatusSurfacer(logger)

	rec, recErr := externaldns.NewReconciler(&externaldns.ReconcilerConfig{
		Client:      clients.Dynamic,
		Namespace:   namespace,
		Instance:    instanceName(),
		Targets:     []string{target},
		TTL:         cfg.ExternalDNSRecordTTL,
		Source:      externaldns.SourceController,
		Annotations: cfg.ExternalDNSAnnotations,
		Surfacer:    surfacer,
		Log:         logger,
	})
	if recErr != nil {
		return nil, errors.Wrap(recErr, "external-dns mode: build reconciler")
	}

	logger.Info("external-dns reconciler ready",
		"namespace", namespace,
		"target", target,
		"ttl", cfg.ExternalDNSRecordTTL,
		"annotations", len(cfg.ExternalDNSAnnotations))

	return rec.Reconcile, nil
}

// readPodNamespace returns the namespace this controller is running in, by
// reading the downward-API-mounted file. Returns an error when running
// outside a cluster and the operator hasn't provided an explicit namespace.
func readPodNamespace() (string, error) {
	bytes, err := os.ReadFile(podNamespaceFile)
	if err != nil {
		return "", errors.Wrapf(err, "read %s", podNamespaceFile)
	}

	ns := string(bytes)
	if ns == "" {
		return "", errors.Errorf("%s is empty", podNamespaceFile)
	}

	return ns, nil
}

// instanceName returns the helm release identifier for ownership labels.
// It is fed in via the OUROBOROS_INSTANCE env var which the chart sets
// from .Release.Name. The fallback "ouroboros" is sufficient for
// out-of-chart deployments where there is no helm release.
func instanceName() string {
	const envInstance = "OUROBOROS_INSTANCE"

	value := os.Getenv(envInstance)
	if value != "" {
		return value
	}

	return "ouroboros"
}
