// Package config parses CLI flags and environment variables into typed
// configuration structs for the ouroboros controller and proxy.
package config

import (
	"flag"
	"time"

	"github.com/cockroachdb/errors"
)

const envPrefix = "OUROBOROS_"

// ProxyConfig is the runtime configuration for `ouroboros proxy`.
type ProxyConfig struct {
	HTTPListen       string
	HTTPSListen      string
	HealthListen     string
	BackendHost      string
	BackendHTTPPort  int
	BackendHTTPSPort int
	DialTimeout      time.Duration
	ReadyTimeout     time.Duration
	ShutdownGrace    time.Duration
}

// DefaultProxy returns the upstream-compatible defaults.
func DefaultProxy() ProxyConfig {
	const (
		defaultHTTPPort      = 80
		defaultHTTPSPort     = 443
		defaultDialTimeout   = 5 * time.Second
		defaultReadyTimeout  = 2 * time.Second
		defaultShutdownGrace = 30 * time.Second
	)

	return ProxyConfig{
		HTTPListen:       ":8080",
		HTTPSListen:      ":8443",
		HealthListen:     ":8081",
		BackendHost:      "ingress-nginx-controller.ingress-nginx.svc.cluster.local",
		BackendHTTPPort:  defaultHTTPPort,
		BackendHTTPSPort: defaultHTTPSPort,
		DialTimeout:      defaultDialTimeout,
		ReadyTimeout:     defaultReadyTimeout,
		ShutdownGrace:    defaultShutdownGrace,
	}
}

// ParseProxyFlags parses argv (without the program name) and the OUROBOROS_*
// environment variables into a ProxyConfig. Flags override environment, which
// override defaults. Invalid env values fail fast (joined error) instead of
// being silently dropped.
func ParseProxyFlags(args []string) (ProxyConfig, error) {
	cfg := DefaultProxy()

	envErr := applyProxyEnv(&cfg)
	if envErr != nil {
		return ProxyConfig{}, errors.Wrap(envErr, "parse proxy env vars")
	}

	flagSet := flag.NewFlagSet("proxy", flag.ContinueOnError)
	flagSet.StringVar(&cfg.HTTPListen, "listen-http", cfg.HTTPListen, "HTTP listen address")
	flagSet.StringVar(&cfg.HTTPSListen, "listen-https", cfg.HTTPSListen, "HTTPS listen address")
	flagSet.StringVar(&cfg.HealthListen, "listen-health", cfg.HealthListen, "health endpoint listen address (\"\" disables)")
	flagSet.StringVar(&cfg.BackendHost, "target-host", cfg.BackendHost, "backend ingress-controller host")
	flagSet.IntVar(&cfg.BackendHTTPPort, "target-http-port", cfg.BackendHTTPPort, "backend HTTP port")
	flagSet.IntVar(&cfg.BackendHTTPSPort, "target-https-port", cfg.BackendHTTPSPort, "backend HTTPS port")
	flagSet.DurationVar(&cfg.DialTimeout, "dial-timeout", cfg.DialTimeout, "backend dial timeout")
	flagSet.DurationVar(&cfg.ReadyTimeout, "ready-timeout", cfg.ReadyTimeout, "/readyz dial timeout")
	flagSet.DurationVar(&cfg.ShutdownGrace, "shutdown-grace", cfg.ShutdownGrace, "graceful shutdown deadline")

	parseErr := flagSet.Parse(args)
	if parseErr != nil {
		return ProxyConfig{}, errors.Wrap(parseErr, "parse proxy flags")
	}

	return cfg, nil
}

func applyProxyEnv(cfg *ProxyConfig) error {
	var errs envErrors

	envString("PROXY_LISTEN_HTTP", &cfg.HTTPListen)
	envString("PROXY_LISTEN_HTTPS", &cfg.HTTPSListen)
	envString("PROXY_LISTEN_HEALTH", &cfg.HealthListen)
	envString("PROXY_TARGET_HOST", &cfg.BackendHost)
	envInt(&errs, "PROXY_TARGET_HTTP_PORT", &cfg.BackendHTTPPort)
	envInt(&errs, "PROXY_TARGET_HTTPS_PORT", &cfg.BackendHTTPSPort)
	envDuration(&errs, "PROXY_DIAL_TIMEOUT", &cfg.DialTimeout)
	envDuration(&errs, "PROXY_READY_TIMEOUT", &cfg.ReadyTimeout)
	envDuration(&errs, "PROXY_SHUTDOWN_GRACE", &cfg.ShutdownGrace)

	return errs.err()
}
