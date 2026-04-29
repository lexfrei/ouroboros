package config

import (
	"flag"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// Mode selects which reconciler the controller uses.
type Mode string

const (
	// ModeCoreDNS rewrites kube-system/coredns ConfigMap.
	ModeCoreDNS Mode = "coredns"

	// ModeEtcHosts writes to a host-mounted /etc/hosts file (DaemonSet mode).
	ModeEtcHosts Mode = "etc-hosts"
)

// ControllerConfig is the runtime configuration for `ouroboros controller`.
type ControllerConfig struct {
	Mode             Mode
	KubeConfig       string
	EnableGatewayAPI bool
	ResyncPeriod     time.Duration

	CorednsNamespace string
	CorednsConfigMap string
	CorednsKey       string
	ProxyFQDN        string

	EtcHostsPath string
	ProxyIP      string
}

// DefaultController returns the safe defaults.
func DefaultController() ControllerConfig {
	const defaultResync = 10 * time.Minute

	return ControllerConfig{
		Mode:             ModeCoreDNS,
		ResyncPeriod:     defaultResync,
		CorednsNamespace: "kube-system",
		CorednsConfigMap: "coredns",
		CorednsKey:       "Corefile",
		ProxyFQDN:        "ouroboros-proxy.ouroboros.svc.cluster.local.",
		EtcHostsPath:     "/host/etc/hosts",
	}
}

// Validate enforces invariants that ParseControllerFlags cannot express
// declaratively.
func (c *ControllerConfig) Validate() error {
	switch c.Mode {
	case ModeCoreDNS:
		return c.validateCoreDNSMode()
	case ModeEtcHosts:
		return c.validateEtcHostsMode()
	default:
		return errors.Errorf("unknown mode %q", c.Mode)
	}
}

func (c *ControllerConfig) validateCoreDNSMode() error {
	if c.ProxyFQDN == "" {
		return errors.New("proxy-fqdn is required for coredns mode")
	}

	if !strings.HasSuffix(c.ProxyFQDN, ".") {
		return errors.Errorf(
			"proxy-fqdn %q must end with a trailing dot (CoreDNS rewrite name targets are FQDN)",
			c.ProxyFQDN,
		)
	}

	if c.CorednsNamespace == "" || c.CorednsConfigMap == "" || c.CorednsKey == "" {
		return errors.New("coredns namespace, configmap and key must be non-empty")
	}

	return nil
}

func (c *ControllerConfig) validateEtcHostsMode() error {
	if c.EtcHostsPath == "" {
		return errors.New("etc-hosts path is required for etc-hosts mode")
	}

	if c.ProxyIP == "" {
		return errors.New("proxy-ip is required for etc-hosts mode")
	}

	return nil
}

// ParseControllerFlags parses argv (without program name) and OUROBOROS_*
// environment variables into a ControllerConfig. Invalid env values fail
// fast (joined error) instead of being silently dropped.
func ParseControllerFlags(args []string) (ControllerConfig, error) {
	cfg := DefaultController()

	envErr := applyControllerEnv(&cfg)
	if envErr != nil {
		return ControllerConfig{}, errors.Wrap(envErr, "parse controller env vars")
	}

	flagSet := flag.NewFlagSet("controller", flag.ContinueOnError)

	mode := string(cfg.Mode)
	flagSet.StringVar(&mode, "mode", mode, `reconcile mode: "coredns" or "etc-hosts"`)
	flagSet.StringVar(&cfg.KubeConfig, "kubeconfig", cfg.KubeConfig, "kubeconfig path (empty = in-cluster)")
	flagSet.BoolVar(&cfg.EnableGatewayAPI, "gateway-api", cfg.EnableGatewayAPI, "watch Gateway API resources in addition to Ingress")
	flagSet.DurationVar(&cfg.ResyncPeriod, "resync", cfg.ResyncPeriod, "informer resync period")
	flagSet.StringVar(&cfg.CorednsNamespace, "coredns-namespace", cfg.CorednsNamespace, "namespace of the CoreDNS ConfigMap")
	flagSet.StringVar(&cfg.CorednsConfigMap, "coredns-configmap", cfg.CorednsConfigMap, "name of the CoreDNS ConfigMap")
	flagSet.StringVar(&cfg.CorednsKey, "coredns-key", cfg.CorednsKey, "data key of the Corefile inside the ConfigMap")
	flagSet.StringVar(&cfg.ProxyFQDN, "proxy-fqdn", cfg.ProxyFQDN, "FQDN to redirect rewrites to (must end in '.')")
	flagSet.StringVar(&cfg.EtcHostsPath, "etc-hosts", cfg.EtcHostsPath, "path to host-mounted /etc/hosts (etc-hosts mode)")
	flagSet.StringVar(&cfg.ProxyIP, "proxy-ip", cfg.ProxyIP, "ClusterIP of the ouroboros-proxy Service (etc-hosts mode)")

	parseErr := flagSet.Parse(args)
	if parseErr != nil {
		return ControllerConfig{}, errors.Wrap(parseErr, "parse controller flags")
	}

	cfg.Mode = Mode(mode)

	validateErr := cfg.Validate()
	if validateErr != nil {
		return ControllerConfig{}, validateErr
	}

	return cfg, nil
}

func applyControllerEnv(cfg *ControllerConfig) error {
	var (
		errs    envErrors
		modeStr string
	)

	envString("CONTROLLER_MODE", &modeStr)

	if modeStr != "" {
		cfg.Mode = Mode(modeStr)
	}

	envString("CONTROLLER_KUBECONFIG", &cfg.KubeConfig)
	envBool(&errs, "CONTROLLER_GATEWAY_API", &cfg.EnableGatewayAPI)
	envDuration(&errs, "CONTROLLER_RESYNC", &cfg.ResyncPeriod)
	envString("CONTROLLER_COREDNS_NAMESPACE", &cfg.CorednsNamespace)
	envString("CONTROLLER_COREDNS_CONFIGMAP", &cfg.CorednsConfigMap)
	envString("CONTROLLER_COREDNS_KEY", &cfg.CorednsKey)
	envString("CONTROLLER_PROXY_FQDN", &cfg.ProxyFQDN)
	envString("CONTROLLER_ETC_HOSTS", &cfg.EtcHostsPath)
	envString("CONTROLLER_PROXY_IP", &cfg.ProxyIP)

	return errs.err()
}
