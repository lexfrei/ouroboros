package main

import (
	"context"
	"log/slog"

	"github.com/cockroachdb/errors"

	"github.com/lexfrei/ouroboros/internal/config"
	"github.com/lexfrei/ouroboros/internal/controller"
	"github.com/lexfrei/ouroboros/internal/coredns"
	"github.com/lexfrei/ouroboros/internal/hosts"
	"github.com/lexfrei/ouroboros/internal/k8s"
)

func runController(ctx context.Context, logger *slog.Logger, args []string) error {
	cfg, parseErr := config.ParseControllerFlags(args)
	if parseErr != nil {
		return errors.Wrap(parseErr, "parse controller flags")
	}

	clients, clientsErr := k8s.Build(cfg.KubeConfig)
	if clientsErr != nil {
		return errors.Wrap(clientsErr, "build clients")
	}

	reconcile, reconcileErr := buildReconcileFunc(&cfg, clients, logger)
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

func buildReconcileFunc(cfg *config.ControllerConfig, clients k8s.Clients, logger *slog.Logger) (controller.ReconcileFunc, error) {
	switch cfg.Mode {
	case config.ModeCoreDNS:
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
		}, nil
	case config.ModeEtcHosts:
		rec := &hosts.Reconciler{Path: cfg.EtcHostsPath, ProxyIP: cfg.ProxyIP}

		return rec.Reconcile, nil
	default:
		return nil, errors.Errorf("unknown mode %q", cfg.Mode)
	}
}
