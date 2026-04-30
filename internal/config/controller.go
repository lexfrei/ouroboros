package config

import (
	"flag"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// Reserved annotation keys that ouroboros refuses to accept through the
// operator passthrough. ouroboros.lexfrei.tech/source is set internally;
// external-dns.alpha.kubernetes.io/target would override the proxy
// ClusterIP target ouroboros emits, defeating the purpose of the chart.
//
//nolint:gochecknoglobals // immutable, package-private allow-list.
var reservedAnnotationKeys = []string{
	"ouroboros.lexfrei.tech/source",
	"external-dns.alpha.kubernetes.io/target",
}

// Reserved label keys that ouroboros owns. managed-by is the chart-wide
// ownership marker; instance scopes prune to a single release.
//
//nolint:gochecknoglobals // immutable, package-private allow-list.
var reservedLabelKeys = []string{
	"app.kubernetes.io/managed-by",
	"ouroboros.lexfrei.tech/instance",
}

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

	// IngressClass scopes hostname extraction to Ingresses whose
	// spec.ingressClassName matches it. Empty disables the filter.
	IngressClass string
	// GatewayClass scopes hostname extraction to Gateways whose
	// spec.gatewayClassName matches it (and HTTPRoutes attached to them).
	GatewayClass string

	// ExternalDNS* fields apply when Mode == ModeExternalDNS.
	ExternalDNSNamespace    string
	ExternalDNSRecordTTL    int64
	ExternalDNSProxyIP      string
	ExternalDNSProxyService string
	ExternalDNSAnnotations  map[string]string
	// ExternalDNSLabels are arbitrary metadata.labels copied verbatim onto
	// every emitted DNSEndpoint. Use case: multi-instance external-dns
	// deployments that filter their CRD source via --label-filter (e.g.
	// 'external-dns-instance=internal-dns' so ouroboros's hairpin records
	// route to the internal-zone controller while a separate instance
	// handles public DNS).
	ExternalDNSLabels map[string]string
}

const (
	defaultExternalDNSRecordTTL int64 = 60
	// maxExternalDNSPassthrough caps both the annotation map and the
	// label map at the same chart-level safety bound. The metadata.name
	// length isn't the constraint here — sheer entry count is, to keep
	// rendered Deployment args manageable.
	maxExternalDNSPassthrough       = 32
	maxRecordTTLSeconds       int64 = 86400
	maxDNS1123LabelLen              = 63
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
	if c.GatewayClass != "" && !c.EnableGatewayAPI {
		return errors.New(
			"gateway-class is set but gateway-api is disabled — the filter is a no-op without --gateway-api; " +
				"either enable Gateway-API watching or remove the gateway-class flag")
	}

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

	// labelNameRE matches the "name" part of a Kubernetes label key
	// (RFC 1123 label-style: alphanumeric with optional dashes /
	// underscores / dots in the middle, ≤63 chars enforced separately).
	// Stricter than annotationKeyRE because the apiserver applies
	// stricter rules to label-keys; catching it locally avoids the
	// delete-all-on-bad-input failure mode.
	labelNameRE = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`)

	// labelPrefixRE matches the "prefix" part of a Kubernetes label key
	// (DNS-1123 subdomain: lowercase alphanumeric, dashes between
	// segments, segments separated by dots; ≤253 chars enforced
	// separately). Strictly tighter than annotationKeyRE — uppercase
	// and underscores are NOT allowed in a label-key prefix even though
	// annotation-key prefixes accept them. Apiserver enforces this and
	// rejects mismatched keys with a confusing error; catching locally
	// keeps the controller out of the delete-all defence layer.
	labelPrefixRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

func (c *ControllerConfig) validateExternalDNSMode() error {
	targetErr := c.validateExternalDNSTarget()
	if targetErr != nil {
		return targetErr
	}

	scalarErr := c.validateExternalDNSScalars()
	if scalarErr != nil {
		return scalarErr
	}

	if len(c.ExternalDNSAnnotations) > maxExternalDNSPassthrough {
		return errors.Errorf(
			"external-dns-annotation: %d entries exceeds the safety bound of %d",
			len(c.ExternalDNSAnnotations), maxExternalDNSPassthrough)
	}

	annoErr := validateAnnotationKeys(c.ExternalDNSAnnotations)
	if annoErr != nil {
		return annoErr
	}

	return validateLabelKeys(c.ExternalDNSLabels)
}

func validateLabelKeys(labels map[string]string) error {
	if len(labels) > maxExternalDNSPassthrough {
		return errors.Errorf(
			"external-dns-label: %d entries exceeds the safety bound of %d",
			len(labels), maxExternalDNSPassthrough)
	}

	for key := range labels {
		validateErr := validateLabelKey(key)
		if validateErr != nil {
			return validateErr
		}

		if slices.Contains(reservedLabelKeys, key) {
			return errors.Errorf(
				"external-dns-label key %q is owned by ouroboros for ownership/prune scoping — pick a different key",
				key)
		}
	}

	return nil
}

// validateLabelKey enforces the apiserver's label-key rules locally so a
// bad key fails at flag-parse time, not at the SSA call inside the
// reconcile loop (where it would trigger the delete-all defence layer).
func validateLabelKey(key string) error {
	prefix, name, hasPrefix := strings.Cut(key, "/")
	if !hasPrefix {
		name = prefix
		prefix = ""
	}

	if name == "" || len(name) > maxDNS1123LabelLen {
		return errors.Errorf(
			"external-dns-label key %q: name part must be 1..63 chars (got %d)",
			key, len(name))
	}

	if !labelNameRE.MatchString(name) {
		return errors.Errorf("external-dns-label key %q has invalid name part %q", key, name)
	}

	if !hasPrefix {
		return nil
	}

	const maxLabelPrefixLen = 253
	if prefix == "" || len(prefix) > maxLabelPrefixLen {
		return errors.Errorf(
			"external-dns-label key %q: prefix part must be 1..%d chars (got %d)",
			key, maxLabelPrefixLen, len(prefix))
	}

	if !labelPrefixRE.MatchString(prefix) {
		return errors.Errorf(
			"external-dns-label key %q has invalid prefix part %q "+
				"(must be a DNS-1123 subdomain: lowercase alphanumeric, "+
				"dashes between segments, segments separated by dots)",
			key, prefix)
	}

	return nil
}

func (c *ControllerConfig) validateExternalDNSTarget() error {
	if c.ExternalDNSProxyIP == "" && c.ExternalDNSProxyService == "" {
		return errors.New(
			"external-dns mode requires either proxy-ip or external-dns-proxy-service " +
				"to be set so the reconciler can resolve a stable A-record target",
		)
	}

	if c.ExternalDNSProxyIP != "" && net.ParseIP(c.ExternalDNSProxyIP) == nil {
		return errors.Errorf("external-dns-proxy-ip %q is not a valid IP literal", c.ExternalDNSProxyIP)
	}

	return nil
}

func (c *ControllerConfig) validateExternalDNSScalars() error {
	if c.ExternalDNSRecordTTL < 1 || c.ExternalDNSRecordTTL > maxRecordTTLSeconds {
		return errors.Errorf(
			"external-dns-record-ttl=%d outside [1, %d] seconds",
			c.ExternalDNSRecordTTL, maxRecordTTLSeconds)
	}

	if c.ExternalDNSNamespace != "" {
		if len(c.ExternalDNSNamespace) > maxDNS1123LabelLen {
			return errors.Errorf(
				"external-dns-namespace %q exceeds the %d-char RFC 1123 label limit",
				c.ExternalDNSNamespace, maxDNS1123LabelLen)
		}

		if !dns1123LabelRE.MatchString(c.ExternalDNSNamespace) {
			return errors.Errorf("external-dns-namespace %q is not a valid RFC 1123 label", c.ExternalDNSNamespace)
		}
	}

	return nil
}

func validateAnnotationKeys(annotations map[string]string) error {
	for key := range annotations {
		if !annotationKeyRE.MatchString(key) {
			return errors.Errorf("external-dns-annotation key %q has invalid characters", key)
		}

		if slices.Contains(reservedAnnotationKeys, key) {
			return errors.Errorf(
				"external-dns-annotation key %q is reserved by ouroboros — "+
					"a misconfigured passthrough would cause every Build to fail "+
					"and trigger a delete-all on subsequent reconcile",
				key)
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
	// Empty value (idx == len(raw)-1) is intentionally accepted: both
	// annotations and labels may carry empty strings as valid values
	// (e.g. a presence-marker label with no payload). Only a missing
	// key (idx <= 0) is rejected.
	if idx <= 0 {
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
	annotations := AnnotationFlag{Map: cfg.ExternalDNSAnnotations}
	labels := AnnotationFlag{Map: cfg.ExternalDNSLabels}

	registerCoreFlags(flagSet, &cfg, &mode)
	registerCorednsFlags(flagSet, &cfg)
	registerEtcHostsFlags(flagSet, &cfg)
	registerExternalDNSFlags(flagSet, &cfg, &annotations, &labels)

	parseErr := flagSet.Parse(args)
	if parseErr != nil {
		return ControllerConfig{}, errors.Wrap(parseErr, "parse controller flags")
	}

	cfg.Mode = Mode(mode)
	cfg.ExternalDNSAnnotations = annotations.Map
	cfg.ExternalDNSLabels = labels.Map

	validateErr := cfg.Validate()
	if validateErr != nil {
		return ControllerConfig{}, validateErr
	}

	return cfg, nil
}

func registerCoreFlags(flagSet *flag.FlagSet, cfg *ControllerConfig, mode *string) {
	flagSet.StringVar(mode, "mode", *mode, `reconcile mode: "coredns", "etc-hosts" or "external-dns"`)
	flagSet.StringVar(&cfg.KubeConfig, "kubeconfig", cfg.KubeConfig, "kubeconfig path (empty = in-cluster)")
	flagSet.BoolVar(&cfg.EnableGatewayAPI, "gateway-api", cfg.EnableGatewayAPI, "watch Gateway API resources in addition to Ingress")
	flagSet.DurationVar(&cfg.ResyncPeriod, "resync", cfg.ResyncPeriod, "informer resync period")
	flagSet.StringVar(&cfg.IngressClass, "ingress-class", cfg.IngressClass,
		"only watch Ingresses with this spec.ingressClassName (empty = all)")
	flagSet.StringVar(&cfg.GatewayClass, "gateway-class", cfg.GatewayClass,
		"only watch Gateways with this spec.gatewayClassName and attached HTTPRoutes (empty = all)")
}

func registerCorednsFlags(flagSet *flag.FlagSet, cfg *ControllerConfig) {
	flagSet.StringVar(&cfg.CorednsNamespace, "coredns-namespace", cfg.CorednsNamespace, "namespace of the CoreDNS ConfigMap")
	flagSet.StringVar(&cfg.CorednsConfigMap, "coredns-configmap", cfg.CorednsConfigMap, "name of the CoreDNS ConfigMap")
	flagSet.StringVar(&cfg.CorednsKey, "coredns-key", cfg.CorednsKey, "data key of the Corefile inside the ConfigMap")
	flagSet.StringVar(&cfg.ProxyFQDN, "proxy-fqdn", cfg.ProxyFQDN, "FQDN to redirect rewrites to (must end in '.')")
}

func registerEtcHostsFlags(flagSet *flag.FlagSet, cfg *ControllerConfig) {
	flagSet.StringVar(&cfg.EtcHostsPath, "etc-hosts", cfg.EtcHostsPath, "path to host-mounted /etc/hosts (etc-hosts mode)")
	flagSet.StringVar(&cfg.ProxyIP, "proxy-ip", cfg.ProxyIP, "ClusterIP of the ouroboros-proxy Service (etc-hosts mode)")
}

func registerExternalDNSFlags(flagSet *flag.FlagSet, cfg *ControllerConfig, annotations, labels *AnnotationFlag) {
	flagSet.StringVar(&cfg.ExternalDNSNamespace, "external-dns-namespace", cfg.ExternalDNSNamespace,
		"namespace where DNSEndpoint CRs are written (default: controller's own namespace)")
	flagSet.Int64Var(&cfg.ExternalDNSRecordTTL, "external-dns-record-ttl", cfg.ExternalDNSRecordTTL,
		"record TTL on emitted DNSEndpoint records (seconds, [1, 86400])")
	flagSet.StringVar(&cfg.ExternalDNSProxyIP, "external-dns-proxy-ip", cfg.ExternalDNSProxyIP,
		"override ClusterIP target for emitted records (default: discovered from proxy Service)")
	flagSet.StringVar(&cfg.ExternalDNSProxyService, "external-dns-proxy-service", cfg.ExternalDNSProxyService,
		"name of the proxy Service to discover ClusterIP from")
	flagSet.Var(annotations, "external-dns-annotation",
		"key=value annotation to attach to every emitted DNSEndpoint (repeatable)")
	flagSet.Var(labels, "external-dns-label",
		"key=value label to attach to every emitted DNSEndpoint (repeatable; for multi-instance external-dns --label-filter)")
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
	envString("CONTROLLER_INGRESS_CLASS", &cfg.IngressClass)
	envString("CONTROLLER_GATEWAY_CLASS", &cfg.GatewayClass)

	// ExternalDNSAnnotations and ExternalDNSLabels are set via repeatable
	// CLI flags (no env counterpart) — chart-only configuration surface.

	envString("CONTROLLER_EXTERNAL_DNS_NAMESPACE", &cfg.ExternalDNSNamespace)
	envInt64(&errs, "CONTROLLER_EXTERNAL_DNS_RECORD_TTL", &cfg.ExternalDNSRecordTTL)
	envString("CONTROLLER_EXTERNAL_DNS_PROXY_IP", &cfg.ExternalDNSProxyIP)
	envString("CONTROLLER_EXTERNAL_DNS_PROXY_SERVICE", &cfg.ExternalDNSProxyService)

	return errs.err()
}
