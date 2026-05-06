// Package config parses CLI flags and environment variables into typed
// configuration structs for the ouroboros controller and proxy.
package config

import (
	"flag"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

const envPrefix = "OUROBOROS_"

// ProxyConfig is the runtime configuration for `ouroboros proxy`.
type ProxyConfig struct {
	HTTPListen   string
	HTTPSListen  string
	HealthListen string

	// BackendHost is the explicit, fully-qualified backend host the proxy
	// dials when it accepts an inbound TCP connection. When non-empty,
	// ResolveBackendHost returns it unchanged. When empty, ResolveBackendHost
	// composes the host from BackendServiceName + BackendServiceNamespace +
	// ClusterDomain at startup, which lets the chart stay platform-agnostic
	// (omit cluster-domain entirely; auto-detect handles it).
	BackendHost string

	// BackendServiceName + BackendServiceNamespace identify the in-cluster
	// Service the proxy dials. Used by ResolveBackendHost to compose
	// BackendHost at runtime when the explicit flag was not passed. Charts
	// that omit --target-host MUST pass these two together with
	// --cluster-domain (or rely on auto-detect of the latter).
	BackendServiceName      string
	BackendServiceNamespace string

	// ClusterDomain is the kubelet --cluster-domain in effect. Empty at
	// flag-parse time triggers auto-detection from /etc/resolv.conf
	// (see DetectClusterDomain). Used only when composing BackendHost
	// from service-name+namespace; ignored when BackendHost is set
	// explicitly.
	ClusterDomain string

	BackendHTTPPort  int
	BackendHTTPSPort int
	DialTimeout      time.Duration
	ReadyTimeout     time.Duration
	ShutdownGrace    time.Duration
}

// ResolveBackendHost returns the backend host the proxy dials: either the
// explicit BackendHost if set, or composed from BackendServiceName +
// BackendServiceNamespace + ClusterDomain at runtime. The composed form
// has NO trailing dot — net.Dial does not accept FQDN-style trailing dots
// in target hostnames (different from CoreDNS rewrite name targets, which
// require one).
func (c *ProxyConfig) ResolveBackendHost() (string, error) {
	if c.BackendHost != "" {
		return c.BackendHost, nil
	}

	if c.BackendServiceName == "" {
		return "", errors.New(
			"resolve target-host: --target-host empty and --target-service-name empty " +
				"(set one explicitly OR pass --target-service-name + --target-service-namespace)",
		)
	}

	if c.BackendServiceNamespace == "" {
		return "", errors.New(
			"resolve target-host: --target-host empty and --target-service-namespace empty " +
				"(chart must set --target-service-namespace)",
		)
	}

	if c.ClusterDomain == "" {
		return "", errors.New(
			"resolve target-host: --target-host empty and --cluster-domain empty " +
				"(auto-detect must populate cluster-domain before ResolveBackendHost runs)",
		)
	}

	domain := strings.TrimSuffix(c.ClusterDomain, ".")

	return c.BackendServiceName + "." + c.BackendServiceNamespace + ".svc." + domain, nil
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
		HTTPListen:   ":8080",
		HTTPSListen:  ":8443",
		HealthListen: ":8081",
		// BackendHost deliberately empty by default — a non-empty seed
		// would short-circuit ResolveBackendHost and silently shadow
		// the chart-supplied --target-service-name + --target-service-namespace
		// flags, defeating the runtime-composition path that makes the
		// chart platform-agnostic. Operators/charts that want a baked
		// host pass --target-host explicitly.
		BackendHost:      "",
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
	flagSet.StringVar(&cfg.BackendHost, "target-host", cfg.BackendHost,
		"explicit backend ingress-controller host. When non-empty, takes precedence over --target-service-name. "+
			"When empty, the proxy composes the backend host at runtime from --target-service-name + "+
			"--target-service-namespace + --cluster-domain")
	flagSet.StringVar(&cfg.BackendServiceName, "target-service-name", cfg.BackendServiceName,
		"in-cluster Service name of the backend ingress controller. Used to compose --target-host at runtime "+
			"when the explicit flag is empty (platform-agnostic mode)")
	flagSet.StringVar(&cfg.BackendServiceNamespace, "target-service-namespace", cfg.BackendServiceNamespace,
		"in-cluster Namespace of the backend ingress controller Service. Used together with --target-service-name "+
			"and --cluster-domain to compose --target-host at runtime")
	flagSet.StringVar(&cfg.ClusterDomain, "cluster-domain", cfg.ClusterDomain,
		"Kubernetes cluster DNS domain (e.g. cluster.local, cozy.local). Empty = auto-detect from /etc/resolv.conf "+
			"(see DefaultClusterDomain fallback). Only consulted when composing --target-host from "+
			"--target-service-name + --target-service-namespace; ignored when --target-host is explicit")
	flagSet.IntVar(&cfg.BackendHTTPPort, "target-http-port", cfg.BackendHTTPPort, "backend HTTP port")
	flagSet.IntVar(&cfg.BackendHTTPSPort, "target-https-port", cfg.BackendHTTPSPort, "backend HTTPS port")
	flagSet.DurationVar(&cfg.DialTimeout, "dial-timeout", cfg.DialTimeout, "backend dial timeout")
	flagSet.DurationVar(&cfg.ReadyTimeout, "ready-timeout", cfg.ReadyTimeout, "/readyz dial timeout")
	flagSet.DurationVar(&cfg.ShutdownGrace, "shutdown-grace", cfg.ShutdownGrace, "graceful shutdown deadline")

	parseErr := flagSet.Parse(args)
	if parseErr != nil {
		return ProxyConfig{}, errors.Wrap(parseErr, "parse proxy flags")
	}

	// ClusterDomain stays empty if neither flag nor env supplied a value.
	// Auto-detect via /etc/resolv.conf so ResolveBackendHost has a concrete
	// cluster-domain to compose with.
	if cfg.ClusterDomain == "" {
		cfg.ClusterDomain = DetectClusterDomain("")
	}

	// Resolve BackendHost now: either explicit --target-host (returned
	// unchanged) or composed from --target-service-name + --target-service-namespace
	// + auto-detected --cluster-domain. Mutating cfg.BackendHost lets every
	// downstream consumer read the resolved value without duplicating the
	// resolution call. Fails fast at parse time on a malformed shape (e.g.
	// service-name without namespace).
	resolved, resolveErr := cfg.ResolveBackendHost()
	if resolveErr != nil {
		return ProxyConfig{}, errors.Wrap(resolveErr, "resolve target-host")
	}

	cfg.BackendHost = resolved

	return cfg, nil
}

func applyProxyEnv(cfg *ProxyConfig) error {
	var errs envErrors

	envString("PROXY_LISTEN_HTTP", &cfg.HTTPListen)
	envString("PROXY_LISTEN_HTTPS", &cfg.HTTPSListen)
	envString("PROXY_LISTEN_HEALTH", &cfg.HealthListen)
	envString("PROXY_TARGET_HOST", &cfg.BackendHost)
	envString("PROXY_TARGET_SERVICE_NAME", &cfg.BackendServiceName)
	envString("PROXY_TARGET_SERVICE_NAMESPACE", &cfg.BackendServiceNamespace)
	envString("PROXY_CLUSTER_DOMAIN", &cfg.ClusterDomain)
	envInt(&errs, "PROXY_TARGET_HTTP_PORT", &cfg.BackendHTTPPort)
	envInt(&errs, "PROXY_TARGET_HTTPS_PORT", &cfg.BackendHTTPSPort)
	envDuration(&errs, "PROXY_DIAL_TIMEOUT", &cfg.DialTimeout)
	envDuration(&errs, "PROXY_READY_TIMEOUT", &cfg.ReadyTimeout)
	envDuration(&errs, "PROXY_SHUTDOWN_GRACE", &cfg.ShutdownGrace)

	return errs.err()
}
