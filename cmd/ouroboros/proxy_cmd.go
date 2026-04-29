package main

import (
	"context"
	"log/slog"

	"github.com/cockroachdb/errors"

	"github.com/lexfrei/ouroboros/internal/config"
	"github.com/lexfrei/ouroboros/internal/proxy"
)

func runProxy(ctx context.Context, logger *slog.Logger, args []string) error {
	cfg, parseErr := config.ParseProxyFlags(args)
	if parseErr != nil {
		return errors.Wrap(parseErr, "parse proxy flags")
	}

	server, newErr := proxy.New(ctx, proxy.Config{
		HTTPListen:       cfg.HTTPListen,
		HTTPSListen:      cfg.HTTPSListen,
		HealthListen:     cfg.HealthListen,
		BackendHost:      cfg.BackendHost,
		BackendHTTPPort:  cfg.BackendHTTPPort,
		BackendHTTPSPort: cfg.BackendHTTPSPort,
		DialTimeout:      cfg.DialTimeout,
		ReadyTimeout:     cfg.ReadyTimeout,
		ShutdownGrace:    cfg.ShutdownGrace,
		Logger:           logger,
	})
	if newErr != nil {
		return errors.Wrap(newErr, "init proxy")
	}

	logger.Info("proxy starting",
		"http", server.HTTPAddr(),
		"https", server.HTTPSAddr(),
		"health", server.HealthAddr(),
		"backend", cfg.BackendHost)

	runErr := server.Run(ctx)
	if runErr != nil {
		return errors.Wrap(runErr, "proxy run")
	}

	return nil
}
