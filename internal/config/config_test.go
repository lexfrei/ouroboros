package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/lexfrei/ouroboros/internal/config"
	"github.com/lexfrei/ouroboros/internal/externaldns"
)

func TestProxyDefaults(t *testing.T) {
	t.Parallel()

	got := config.DefaultProxy()
	if got.HTTPListen != ":8080" {
		t.Errorf("HTTPListen = %q, want :8080", got.HTTPListen)
	}

	if got.BackendHTTPPort != 80 {
		t.Errorf("BackendHTTPPort = %d, want 80", got.BackendHTTPPort)
	}
}

func TestParseProxyFlags_OverridesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseProxyFlags([]string{
		"--listen-http", ":9090",
		"--target-host", "custom.svc",
		"--target-http-port", "8081",
	})
	if err != nil {
		t.Fatalf("ParseProxyFlags: %v", err)
	}

	if cfg.HTTPListen != ":9090" {
		t.Errorf("HTTPListen = %q, want :9090", cfg.HTTPListen)
	}

	if cfg.BackendHost != "custom.svc" {
		t.Errorf("BackendHost = %q, want custom.svc", cfg.BackendHost)
	}

	if cfg.BackendHTTPPort != 8081 {
		t.Errorf("BackendHTTPPort = %d, want 8081", cfg.BackendHTTPPort)
	}
}

func TestParseProxyFlags_RejectsUnknownFlag(t *testing.T) {
	t.Parallel()

	_, err := config.ParseProxyFlags([]string{"--no-such-flag"})
	if err == nil {
		t.Fatal("expected error on unknown flag, got nil")
	}
}

func TestParseProxyFlags_HonoursEnv(t *testing.T) {
	t.Setenv("OUROBOROS_PROXY_TARGET_HOST", "envhost")
	t.Setenv("OUROBOROS_PROXY_DIAL_TIMEOUT", "7s")

	cfg, err := config.ParseProxyFlags(nil)
	if err != nil {
		t.Fatalf("ParseProxyFlags: %v", err)
	}

	if cfg.BackendHost != "envhost" {
		t.Errorf("BackendHost = %q, want envhost", cfg.BackendHost)
	}

	if cfg.DialTimeout != 7*time.Second {
		t.Errorf("DialTimeout = %v, want 7s", cfg.DialTimeout)
	}
}

func TestParseProxyFlags_FlagOverridesEnv(t *testing.T) {
	t.Setenv("OUROBOROS_PROXY_TARGET_HOST", "envhost")

	cfg, err := config.ParseProxyFlags([]string{"--target-host", "flaghost"})
	if err != nil {
		t.Fatalf("ParseProxyFlags: %v", err)
	}

	if cfg.BackendHost != "flaghost" {
		t.Errorf("BackendHost = %q, want flaghost (flag must override env)", cfg.BackendHost)
	}
}

func TestParseControllerFlags_DefaultsToCoreDNSMode(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags(nil)
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.Mode != config.ModeCoreDNS {
		t.Errorf("Mode = %q, want %q", cfg.Mode, config.ModeCoreDNS)
	}

	if cfg.CorednsNamespace != "kube-system" {
		t.Errorf("CorednsNamespace = %q, want kube-system", cfg.CorednsNamespace)
	}
}

func TestParseControllerFlags_EtcHostsRequiresProxyIP(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{"--mode", "etc-hosts"})
	if err == nil {
		t.Fatal("etc-hosts mode without proxy-ip must fail validation")
	}
}

func TestParseControllerFlags_EtcHostsModeAcceptsValidFlags(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		"--mode", "etc-hosts",
		"--etc-hosts", "/tmp/hosts",
		"--proxy-ip", "10.96.1.2",
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.Mode != config.ModeEtcHosts {
		t.Errorf("Mode = %q, want %q", cfg.Mode, config.ModeEtcHosts)
	}

	if cfg.ProxyIP != "10.96.1.2" {
		t.Errorf("ProxyIP = %q", cfg.ProxyIP)
	}
}

func TestParseControllerFlags_RejectsUnknownMode(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{"--mode", "bogus"})
	if err == nil {
		t.Fatal("unknown mode must fail validation")
	}
}

func TestParseProxyFlags_RejectsInvalidEnvDuration(t *testing.T) {
	t.Setenv("OUROBOROS_PROXY_DIAL_TIMEOUT", "5sec")

	_, err := config.ParseProxyFlags(nil)
	if err == nil {
		t.Fatal("invalid OUROBOROS_PROXY_DIAL_TIMEOUT must fail fast, not be silently ignored")
	}
}

func TestParseProxyFlags_RejectsInvalidEnvInt(t *testing.T) {
	t.Setenv("OUROBOROS_PROXY_TARGET_HTTP_PORT", "eighty")

	_, err := config.ParseProxyFlags(nil)
	if err == nil {
		t.Fatal("invalid OUROBOROS_PROXY_TARGET_HTTP_PORT must fail fast")
	}
}

func TestParseControllerFlags_GatewayClassRequiresGatewayAPI(t *testing.T) {
	t.Parallel()

	// Setting --gateway-class without --gateway-api is a silent no-op
	// because the Gateway informer never starts. Catch the misconfig at
	// parse time so the operator gets a clear error instead of staring
	// at an unfiltered controller and wondering why nothing is filtered.
	_, err := config.ParseControllerFlags([]string{
		"--gateway-class", "envoy-proxy",
	})
	if err == nil {
		t.Fatal("--gateway-class without --gateway-api must fail validation")
	}
}

func TestParseControllerFlags_HonoursIngressClassEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_INGRESS_CLASS", "nginx-proxy")

	cfg, err := config.ParseControllerFlags(nil)
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.IngressClass != "nginx-proxy" {
		t.Fatalf("IngressClass = %q, want nginx-proxy", cfg.IngressClass)
	}
}

func TestParseControllerFlags_HonoursGatewayClassEnv(t *testing.T) {
	// gateway-class requires gateway-api to be enabled — set both env
	// vars so the combined config is valid.
	t.Setenv("OUROBOROS_CONTROLLER_GATEWAY_API", "true")
	t.Setenv("OUROBOROS_CONTROLLER_GATEWAY_CLASS", "envoy-proxy")

	cfg, err := config.ParseControllerFlags(nil)
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.GatewayClass != "envoy-proxy" {
		t.Fatalf("GatewayClass = %q, want envoy-proxy", cfg.GatewayClass)
	}
}

func TestParseControllerFlags_RejectsInvalidEnvBool(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_GATEWAY_API", "tru")

	_, err := config.ParseControllerFlags(nil)
	if err == nil {
		t.Fatal("invalid OUROBOROS_CONTROLLER_GATEWAY_API must fail fast (typo would silently disable gateway-api)")
	}
}

func TestValidate_CorednsModeRequiresProxyFQDN(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("coredns mode without proxy-fqdn must fail validation")
	}
}

func TestValidate_CorednsModeRequiresTrailingDotInProxyFQDN(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = "ouroboros-proxy.ouroboros.svc.cluster.local"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("coredns mode with non-FQDN proxy-fqdn must fail validation at config time")
	}
}

func TestValidate_CorednsModeAcceptsFQDNWithTrailingDot(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = "ouroboros-proxy.ouroboros.svc.cluster.local."

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("trailing-dot FQDN must validate: %v", err)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RequiresProxyIPOrService(t *testing.T) {
	t.Parallel()

	// Default has ExternalDNSProxyService=ouroboros-proxy, so blank both to
	// reproduce the "operator forgot to set anything" failure mode.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-service", "",
	})
	if err == nil {
		t.Fatal("external-dns mode with neither proxy-ip nor service must fail")
	}
}

func TestParseControllerFlags_ExternalDNSMode_ProxyServiceDefaultIsValid(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{"--mode", "external-dns"})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.Mode != config.ModeExternalDNS {
		t.Fatalf("Mode = %q, want %q", cfg.Mode, config.ModeExternalDNS)
	}

	if cfg.ExternalDNSProxyService == "" {
		t.Fatal("default ExternalDNSProxyService should not be empty")
	}
}

func TestParseControllerFlags_ExternalDNSMode_ProxyIPSetIsValid(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSProxyIP != "10.42.0.7" {
		t.Fatalf("ExternalDNSProxyIP = %q, want 10.42.0.7", cfg.ExternalDNSProxyIP)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsInvalidNamespace(t *testing.T) {
	t.Parallel()

	// Uppercase namespace would crash kube-apiserver with a confusing error;
	// validation catches it locally.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-namespace", "BadNamespace",
	})
	if err == nil {
		t.Fatal("uppercase namespace must fail RFC 1123 validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsTTLOutOfRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ttl  string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"too-big", "86401"},
	}
	for _, tc := range cases {
		_, err := config.ParseControllerFlags([]string{
			"--mode", "external-dns",
			"--external-dns-proxy-ip", "10.42.0.7",
			"--external-dns-record-ttl", tc.ttl,
		})
		if err == nil {
			t.Errorf("ttl=%s must fail validation", tc.name)
		}
	}
}

func TestParseControllerFlags_ExternalDNSMode_AcceptsRepeatedAnnotationFlag(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-annotation", "external-dns.alpha.kubernetes.io/cloudflare-proxied=false",
		"--external-dns-annotation", "external-dns.alpha.kubernetes.io/aws-region=us-east-1",
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if got := cfg.ExternalDNSAnnotations["external-dns.alpha.kubernetes.io/cloudflare-proxied"]; got != "false" {
		t.Fatalf("cloudflare-proxied = %q, want false", got)
	}

	if got := cfg.ExternalDNSAnnotations["external-dns.alpha.kubernetes.io/aws-region"]; got != "us-east-1" {
		t.Fatalf("aws-region = %q, want us-east-1", got)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsAnnotationWithoutEquals(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-annotation", "no-equals-sign",
	})
	if err == nil {
		t.Fatal("annotation flag without '=' must fail")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsBadAnnotationKey(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-annotation", "bad key with spaces=value",
	})
	if err == nil {
		t.Fatal("annotation key with spaces must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_HonoursTTLEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_EXTERNAL_DNS_RECORD_TTL", "300")

	cfg, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSRecordTTL != 300 {
		t.Fatalf("TTL = %d, want 300 (from env)", cfg.ExternalDNSRecordTTL)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsInvalidTTLEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_EXTERNAL_DNS_RECORD_TTL", "not-a-number")

	_, err := config.ParseControllerFlags(nil)
	if err == nil {
		t.Fatal("invalid TTL env must fail fast, not be silently dropped")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsInvalidProxyIP(t *testing.T) {
	t.Parallel()

	// A typo'd proxy IP would let the controller start, but every Build
	// would then fail to parse the literal — leaving desired empty and
	// causing prune to delete every ouroboros-owned DNSEndpoint. Catch it
	// at config time instead.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.999",
	})
	if err == nil {
		t.Fatal("invalid proxy IP must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsReservedSourceAnnotation(t *testing.T) {
	t.Parallel()

	// Build() rejects this internally, but until that happens the
	// controller has already started and is happily reconciling. With
	// every Build returning an error, desired is empty and prune wipes
	// the namespace. Reject the annotation at parse time.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-annotation", "ouroboros.lexfrei.tech/source=user-override",
	})
	if err == nil {
		t.Fatal("reserved source annotation key must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsOverlongNamespace(t *testing.T) {
	t.Parallel()

	// 64-char namespaces would parse the regex but blow up at the API
	// server with a confusing label-length error; catch it locally.
	overlong := "a"
	for range 6 {
		overlong += overlong
	} // 64 chars

	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-namespace", overlong,
	})
	if err == nil {
		t.Fatalf("64-char namespace must fail RFC 1123 length validation (got %d chars)", len(overlong))
	}
}

func TestExternalDNSDefaultTTL_StaysInSyncWithEndpointPackage(t *testing.T) {
	t.Parallel()

	// The default record TTL is duplicated in config and externaldns
	// (cycle-free, but parallel). Pin them together so a future bump in
	// one trips this test instead of producing a silent skew between
	// the chart-rendered flag default and the Build() fallback.
	cfg := config.DefaultController()
	if cfg.ExternalDNSRecordTTL != externaldns.DefaultRecordTTL {
		t.Fatalf("config.DefaultController.ExternalDNSRecordTTL = %d, externaldns.DefaultRecordTTL = %d — keep in sync",
			cfg.ExternalDNSRecordTTL, externaldns.DefaultRecordTTL)
	}
}

const teamLabelValue = "platform"

func TestParseControllerFlags_ExternalDNSMode_AcceptsLabelPassthrough(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "external-dns-instance=internal-dns",
		"--external-dns-label", "team=" + teamLabelValue,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSLabels["external-dns-instance"] != "internal-dns" {
		t.Fatalf("got labels %v", cfg.ExternalDNSLabels)
	}

	if cfg.ExternalDNSLabels["team"] != teamLabelValue {
		t.Fatalf("got labels %v", cfg.ExternalDNSLabels)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsLabelNameOver63Chars(t *testing.T) {
	t.Parallel()

	// k8s label-key name part is capped at 63 chars; the apiserver
	// rejects longer keys with a confusing error. Catch it at parse time
	// so a bad chart value does not put the controller in the
	// 'every Build fails -> delete-all' state.
	long := strings.Repeat("a", 64)

	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", long + "=v",
	})
	if err == nil {
		t.Fatalf("64-char label name (%d chars) must fail validation", len(long))
	}
}

func TestParseControllerFlags_ExternalDNSMode_AcceptsPrefixedLabelKey(t *testing.T) {
	t.Parallel()

	// Spec-conformant label keys with a prefix/name split must pass
	// (e.g. external-dns-instance under company.io/).
	cfg, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "company.io/external-dns-instance=internal",
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSLabels["company.io/external-dns-instance"] != "internal" {
		t.Fatalf("got labels %v", cfg.ExternalDNSLabels)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsLabelKeyWithLeadingSlash(t *testing.T) {
	t.Parallel()

	// "/foo" has a non-empty key (so AnnotationFlag.Set accepts it) but
	// strings.Cut splits it into ("", "foo", true) — that lands in
	// validateLabelKey's "empty prefix with hasPrefix=true" branch. Pin
	// the rejection: apiserver would otherwise reject the label with a
	// confusing message at SSA time.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "/foo=v",
	})
	if err == nil {
		t.Fatal("label key with leading '/' (empty prefix) must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsLabelPrefixOver253Chars(t *testing.T) {
	t.Parallel()

	// k8s label-key prefix is capped at 253 chars (DNS-1123 subdomain).
	// 254-char prefix must fail at parse time, not at SSA time.
	overlong := strings.Repeat("a", 254)

	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", overlong + "/name=v",
	})
	if err == nil {
		t.Fatalf("254-char prefix must fail validation (got %d chars)", len(overlong))
	}
}

func TestParseControllerFlags_ExternalDNSMode_AcceptsEmptyLabelValue(t *testing.T) {
	t.Parallel()

	// Kubernetes labels and annotations both allow empty string values
	// (presence-marker idiom). Helm renders them verbatim:
	// `--external-dns-label=foo=`. The parser must not reject those.
	cfg, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "presence-marker=",
	})
	if err != nil {
		t.Fatalf("empty label value must be accepted: %v", err)
	}

	if value, ok := cfg.ExternalDNSLabels["presence-marker"]; !ok || value != "" {
		t.Fatalf("expected presence-marker -> '', got %q (present=%v)", value, ok)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsLabelPrefixWithUppercase(t *testing.T) {
	t.Parallel()

	// Apiserver rejects label-key prefixes that contain uppercase. The
	// previous validator (annotation-key regex) let it through, so a
	// chart misconfig would put the controller in delete-all loop.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "Company.io/foo=v",
	})
	if err == nil {
		t.Fatal("uppercase in label-key prefix must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsLabelPrefixWithUnderscore(t *testing.T) {
	t.Parallel()

	// DNS-1123 subdomain disallows underscores in label prefixes.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "under_score.io/foo=v",
	})
	if err == nil {
		t.Fatal("underscore in label-key prefix must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_AcceptsMultiSegmentDNSPrefix(t *testing.T) {
	t.Parallel()

	// Multi-segment subdomain prefix (a.b.c/name) is the DNS-1123
	// idiom — must pass.
	cfg, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "a.b.c/team=" + teamLabelValue,
	})
	if err != nil {
		t.Fatalf("multi-segment prefix must pass: %v", err)
	}

	if cfg.ExternalDNSLabels["a.b.c/team"] != teamLabelValue {
		t.Fatalf("got labels %v", cfg.ExternalDNSLabels)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsLabelNameWithUnderscoreLeadingChar(t *testing.T) {
	t.Parallel()

	// Label name parts must start with alphanumeric — not '_'. apiserver
	// would reject; catch locally.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "_bad=v",
	})
	if err == nil {
		t.Fatal("label key starting with '_' must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsReservedManagedByLabel(t *testing.T) {
	t.Parallel()

	// Operators must not be able to clobber ownership labels through the
	// passthrough — that would let their cleanup tooling mistake foreign
	// records as theirs (or vice-versa).
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "app.kubernetes.io/managed-by=imposter",
	})
	if err == nil {
		t.Fatal("reserved managed-by label key must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsReservedInstanceLabel(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-label", "ouroboros.lexfrei.tech/instance=wrong-release",
	})
	if err == nil {
		t.Fatal("reserved instance label key must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsReservedExternalDNSTargetAnnotation(t *testing.T) {
	t.Parallel()

	// external-dns reads its own alpha target annotation and would
	// override the proxy ClusterIP target — exactly what ouroboros
	// exists to prevent. Reject at parse time.
	_, err := config.ParseControllerFlags([]string{
		"--mode", "external-dns",
		"--external-dns-proxy-ip", "10.42.0.7",
		"--external-dns-annotation", "external-dns.alpha.kubernetes.io/target=evil.example.com",
	})
	if err == nil {
		t.Fatal("reserved external-dns target annotation key must fail validation")
	}
}
