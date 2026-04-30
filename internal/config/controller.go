package config

import (
	"flag"
	"regexp"
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

	// ModeExternalDNS emits externaldns.k8s.io/v1alpha1.DNSEndpoint CRs.
	ModeExternalDNS Mode = "external-dns"
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

	// ExternalDNS* fields apply when Mode == ModeExternalDNS.
	ExternalDNSNamespace    string
	ExternalDNSRecordTTL    int64
	ExternalDNSProxyIP      string
	ExternalDNSProxyService string
	ExternalDNSAnnotations  map[string]string
}

const (
	defaultExternalDNSRecordTTL int64 = 60
	maxExternalDNSAnnotations         = 32
	maxRecordTTLSeconds         int64 = 86400
)

// DefaultController returns the safe defaults.
func DefaultController() ControllerConfig {
	const defaultResync = 10 * time.Minute

	return ControllerConfig{
		Mode:                    ModeCoreDNS,
		ResyncPeriod:            defaultResync,
		CorednsNamespace:        "kube-system",
		CorednsConfigMap:        "coredns",
		CorednsKey:              "Corefile",
		ProxyFQDN:               "ouroboros-proxy.ouroboros.svc.cluster.local.",
		EtcHostsPath:            "/host/etc/hosts",
		ExternalDNSRecordTTL:    defaultExternalDNSRecordTTL,
		ExternalDNSProxyService: "ouroboros-proxy",
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
	case ModeExternalDNS:
		return c.validateExternalDNSMode()
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

var (
	// dns1123LabelRE matches lowercase RFC 1123 labels — the constraint
	// Kubernetes namespaces must satisfy.
	dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	// annotationKeyRE matches the valid character set for Kubernetes
	// annotation keys. Single-segment legacy keys are accepted; we do not
	// require the prefix/name split here.
	annotationKeyRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_-]*$`)
)

func (c *ControllerConfig) validateExternalDNSMode() error {
	if c.ExternalDNSProxyIP == "" && c.ExternalDNSProxyService == "" {
		return errors.New(
			"external-dns mode requires either proxy-ip or external-dns-proxy-service " +
				"to be set so the reconciler can resolve a stable A-record target",
		)
	}

	if c.ExternalDNSRecordTTL < 1 || c.ExternalDNSRecordTTL > maxRecordTTLSeconds {
		return errors.Errorf(
			"external-dns-record-ttl=%d outside [1, %d] seconds",
			c.ExternalDNSRecordTTL, maxRecordTTLSeconds)
	}

	if c.ExternalDNSNamespace != "" && !dns1123LabelRE.MatchString(c.ExternalDNSNamespace) {
		return errors.Errorf("external-dns-namespace %q is not a valid RFC 1123 label", c.ExternalDNSNamespace)
	}

	if len(c.ExternalDNSAnnotations) > maxExternalDNSAnnotations {
		return errors.Errorf(
			"external-dns-annotation: %d entries exceeds the safety bound of %d",
			len(c.ExternalDNSAnnotations), maxExternalDNSAnnotations)
	}

	for key := range c.ExternalDNSAnnotations {
		if !annotationKeyRE.MatchString(key) {
			return errors.Errorf("external-dns-annotation key %q has invalid characters", key)
		}
	}

	return nil
}

// AnnotationFlag implements flag.Value so --external-dns-annotation can be
// repeated to build up a map.
type AnnotationFlag struct {
	Map map[string]string
}

// String renders the current set in a stable way for flag.PrintDefaults. The
// flag default is the empty map.
func (annoFlag *AnnotationFlag) String() string {
	if annoFlag == nil || len(annoFlag.Map) == 0 {
		return ""
	}

	parts := make([]string, 0, len(annoFlag.Map))
	for key, value := range annoFlag.Map {
		parts = append(parts, key+"="+value)
	}

	return strings.Join(parts, ",")
}

// Set parses one "key=value" pair and adds it to the map. Repeated keys
// overwrite — last one wins, mirroring how flag values would normally
// behave when supplied twice.
func (annoFlag *AnnotationFlag) Set(raw string) error {
	idx := strings.IndexByte(raw, '=')
	if idx <= 0 || idx == len(raw)-1 {
		return errors.Errorf("expected key=value, got %q", raw)
	}

	if annoFlag.Map == nil {
		annoFlag.Map = make(map[string]string)
	}

	annoFlag.Map[raw[:idx]] = raw[idx+1:]

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
	flagSet.StringVar(&mode, "mode", mode, `reconcile mode: "coredns", "etc-hosts" or "external-dns"`)
	flagSet.StringVar(&cfg.KubeConfig, "kubeconfig", cfg.KubeConfig, "kubeconfig path (empty = in-cluster)")
	flagSet.BoolVar(&cfg.EnableGatewayAPI, "gateway-api", cfg.EnableGatewayAPI, "watch Gateway API resources in addition to Ingress")
	flagSet.DurationVar(&cfg.ResyncPeriod, "resync", cfg.ResyncPeriod, "informer resync period")
	flagSet.StringVar(&cfg.CorednsNamespace, "coredns-namespace", cfg.CorednsNamespace, "namespace of the CoreDNS ConfigMap")
	flagSet.StringVar(&cfg.CorednsConfigMap, "coredns-configmap", cfg.CorednsConfigMap, "name of the CoreDNS ConfigMap")
	flagSet.StringVar(&cfg.CorednsKey, "coredns-key", cfg.CorednsKey, "data key of the Corefile inside the ConfigMap")
	flagSet.StringVar(&cfg.ProxyFQDN, "proxy-fqdn", cfg.ProxyFQDN, "FQDN to redirect rewrites to (must end in '.')")
	flagSet.StringVar(&cfg.EtcHostsPath, "etc-hosts", cfg.EtcHostsPath, "path to host-mounted /etc/hosts (etc-hosts mode)")
	flagSet.StringVar(&cfg.ProxyIP, "proxy-ip", cfg.ProxyIP, "ClusterIP of the ouroboros-proxy Service (etc-hosts mode)")

	flagSet.StringVar(&cfg.ExternalDNSNamespace, "external-dns-namespace", cfg.ExternalDNSNamespace,
		"namespace where DNSEndpoint CRs are written (default: controller's own namespace)")
	flagSet.Int64Var(&cfg.ExternalDNSRecordTTL, "external-dns-record-ttl", cfg.ExternalDNSRecordTTL,
		"record TTL on emitted DNSEndpoint records (seconds, [1, 86400])")
	flagSet.StringVar(&cfg.ExternalDNSProxyIP, "external-dns-proxy-ip", cfg.ExternalDNSProxyIP,
		"override ClusterIP target for emitted records (default: discovered from proxy Service)")
	flagSet.StringVar(&cfg.ExternalDNSProxyService, "external-dns-proxy-service", cfg.ExternalDNSProxyService,
		"name of the proxy Service to discover ClusterIP from")

	annotations := AnnotationFlag{Map: cfg.ExternalDNSAnnotations}
	flagSet.Var(&annotations, "external-dns-annotation",
		"key=value annotation to attach to every emitted DNSEndpoint (repeatable)")

	parseErr := flagSet.Parse(args)
	if parseErr != nil {
		return ControllerConfig{}, errors.Wrap(parseErr, "parse controller flags")
	}

	cfg.Mode = Mode(mode)
	cfg.ExternalDNSAnnotations = annotations.Map

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

	envString("CONTROLLER_EXTERNAL_DNS_NAMESPACE", &cfg.ExternalDNSNamespace)
	envInt64(&errs, "CONTROLLER_EXTERNAL_DNS_RECORD_TTL", &cfg.ExternalDNSRecordTTL)
	envString("CONTROLLER_EXTERNAL_DNS_PROXY_IP", &cfg.ExternalDNSProxyIP)
	envString("CONTROLLER_EXTERNAL_DNS_PROXY_SERVICE", &cfg.ExternalDNSProxyService)

	return errs.err()
}
