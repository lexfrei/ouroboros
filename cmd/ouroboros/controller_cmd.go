package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/cockroachdb/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/lexfrei/ouroboros/internal/config"
	"github.com/lexfrei/ouroboros/internal/controller"
	"github.com/lexfrei/ouroboros/internal/coredns"
	"github.com/lexfrei/ouroboros/internal/externaldns"
	"github.com/lexfrei/ouroboros/internal/hosts"
	"github.com/lexfrei/ouroboros/internal/k8s"
)

// nodeLocalDNSProbeTimeout bounds the startup detection probe so a slow
// or partially unavailable apiserver does not stall the controller from
// reaching its main reconcile loop. The probe is best-effort — a missed
// detection only loses a Warn log line.
const nodeLocalDNSProbeTimeout = 5 * time.Second

// podNamespaceFile is where the projected ServiceAccount token volume
// auto-mounts the pod's own namespace when running in-cluster (kubelet
// adds this file alongside token and ca.crt). The proxy Service always
// lives in this same namespace (the chart renders it as a
// release-namespace resource), so this is also the right place to look
// it up.
const podNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// envInstance is the env var the chart sets to .Release.Name. Used as the
// instance label on every emitted DNSEndpoint so two ouroboros releases in
// the same namespace do not delete each other's records during prune.
const envInstance = "OUROBOROS_INSTANCE"

func runController(ctx context.Context, logger *slog.Logger, args []string) error {
	cfg, parseErr := config.ParseControllerFlags(args)
	if parseErr != nil {
		return errors.Wrap(parseErr, "parse controller flags")
	}

	clients, clientsErr := k8s.Build(cfg.KubeConfig)
	if clientsErr != nil {
		return errors.Wrap(clientsErr, "build clients")
	}

	// Best-effort startup probes — kept here (next to the lifecycle owner)
	// rather than inside the per-mode factories so 'build a reconciler' and
	// 'do I/O at startup' have clearly different lifecycles. Bounded by
	// nodeLocalDNSProbeTimeout so a slow apiserver cannot stall startup.
	if cfg.Mode == config.ModeCoreDNS {
		probeCtx, cancel := context.WithTimeout(ctx, nodeLocalDNSProbeTimeout)
		coredns.WarnIfNodeLocalDNSDetected(probeCtx, clients.Core, logger)
		cancel()
	}

	reconcile, reconcileErr := buildReconcileFunc(ctx, &cfg, clients, logger)
	if reconcileErr != nil {
		return reconcileErr
	}

	ctrl := controller.New(&controller.Options{
		Core:         clients.Core,
		Gateway:      clients.Gateway,
		EnableGW:     cfg.EnableGatewayAPI,
		Reconciler:   reconcile,
		ResyncPeriod: cfg.ResyncPeriod,
		Logger:       logger,
		IngressClass: cfg.IngressClass,
		GatewayClass: cfg.GatewayClass,
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

// externalDNSPlan holds everything the external-dns reconciler needs that is
// resolved at startup, before any Reconcile call. Pulled out into a struct so
// the resolution logic is testable in isolation from the run loop.
type externalDNSPlan struct {
	RecordsNamespace string
	ServiceNamespace string
	Targets          []string
	Instance         string
}

// resolveExternalDNSPlan computes the namespace where DNSEndpoints are
// written, the namespace where the proxy Service lives, the IP targets, and
// the instance label, applying the rules:
//
//   - Service lookup ALWAYS happens in the controller's own namespace (the
//     pod namespace, read via the downward API mount). The chart renders
//     the proxy Service in the release namespace, so this is the only
//     correct place to look. cfg.ExternalDNSNamespace governs ONLY where
//     DNSEndpoint CRs are emitted, not where the Service is found.
//   - cfg.ExternalDNSProxyIP overrides the resolution entirely and skips
//     the API call.
//   - cfg.ExternalDNSNamespace overrides the records namespace; defaults to
//     the pod namespace.
//   - The instance label MUST come from the env (chart sets it from
//     .Release.Name). external-dns mode is the only mode that uses
//     ownership labels for prune, so a missing env is a hard error here —
//     a silent fallback would let two releases delete each other's records.
func resolveExternalDNSPlan(
	ctx context.Context,
	core kubernetes.Interface,
	cfg *config.ControllerConfig,
	readNamespace func() (string, error),
	getInstance func() string,
) (*externalDNSPlan, error) {
	serviceNs, nsErr := readNamespace()
	if nsErr != nil {
		return nil, errors.Wrap(nsErr, "external-dns mode: read pod namespace for proxy Service lookup")
	}

	recordsNs := cfg.ExternalDNSNamespace
	if recordsNs == "" {
		recordsNs = serviceNs
	}

	instance := getInstance()
	if instance == "" {
		return nil, errors.Errorf(
			"external-dns mode: %s env var is required (chart sets it from .Release.Name); "+
				"a missing instance would let two ouroboros releases delete each other's records during prune",
			envInstance)
	}

	var targets []string
	if cfg.ExternalDNSProxyIP != "" {
		targets = []string{cfg.ExternalDNSProxyIP}
	} else {
		resolved, resolveErr := k8s.ResolveProxyClusterIPs(ctx, core, serviceNs, cfg.ExternalDNSProxyService)
		if resolveErr != nil {
			return nil, errors.Wrap(resolveErr, "external-dns mode: discover proxy ClusterIPs")
		}

		targets = resolved
	}

	return &externalDNSPlan{
		RecordsNamespace: recordsNs,
		ServiceNamespace: serviceNs,
		Targets:          targets,
		Instance:         instance,
	}, nil
}

func buildExternalDNSReconcile(
	ctx context.Context,
	cfg *config.ControllerConfig,
	clients k8s.Clients,
	logger *slog.Logger,
) (controller.ReconcileFunc, error) {
	plan, planErr := resolveExternalDNSPlan(ctx, clients.Core, cfg, readPodNamespace, instanceName)
	if planErr != nil {
		return nil, planErr
	}

	surfacer := externaldns.NewStatusSurfacer(logger)

	rec, recErr := externaldns.NewReconciler(&externaldns.ReconcilerConfig{
		Client:      clients.Dynamic,
		Namespace:   plan.RecordsNamespace,
		Instance:    plan.Instance,
		Targets:     plan.Targets,
		TTL:         cfg.ExternalDNSRecordTTL,
		Source:      externaldns.SourceController,
		Annotations: cfg.ExternalDNSAnnotations,
		Labels:      cfg.ExternalDNSLabels,
		Surfacer:    surfacer,
		Log:         logger,
	})
	if recErr != nil {
		return nil, errors.Wrap(recErr, "external-dns mode: build reconciler")
	}

	logger.Info("external-dns reconciler ready",
		"recordsNamespace", plan.RecordsNamespace,
		"serviceNamespace", plan.ServiceNamespace,
		"targets", plan.Targets,
		"ttl", cfg.ExternalDNSRecordTTL,
		"annotations", len(cfg.ExternalDNSAnnotations),
		"labels", len(cfg.ExternalDNSLabels),
		"instance", plan.Instance)

	return rec.Reconcile, nil
}

// readPodNamespace returns the namespace this controller is running in, by
// reading the downward-API-mounted file. Returns an error when running
// outside a cluster — callers must handle this rather than silently falling
// back to a default.
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

// instanceName returns the OUROBOROS_INSTANCE env value verbatim; an empty
// string signals "not set" to the caller, which must reject it explicitly.
// The chart always sets this to .Release.Name.
func instanceName() string {
	return os.Getenv(envInstance)
}
