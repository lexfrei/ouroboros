package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lexfrei/ouroboros/internal/config"
	"github.com/lexfrei/ouroboros/internal/externaldns"
)

// Test-data constants. Hoisted to keep goconst quiet without rewriting
// individual literals to be less explicit at the call site — table-driven
// tests below repeat these flag names and fixture strings dozens of times.
const (
	flagMode                        = "--mode"
	flagTargetHost                  = "--target-host"
	flagExternalDNSProxyIP          = "--external-dns-proxy-ip"
	flagExternalDNSOutput           = "--external-dns-output"
	flagExternalDNSAnnotationPrefix = "--external-dns-annotation-prefix"
	flagExternalDNSNamespace        = "--external-dns-namespace"
	flagExternalDNSAnnotation       = "--external-dns-annotation"
	flagExternalDNSLabel            = "--external-dns-label"

	testListenAddr        = ":9090"
	testProxyIPAddr       = "10.96.1.2"
	testExternalDNSProxIP = "10.42.0.7"
	testFlaghostFQDN      = "flaghost"
	testBogusValue        = "bogus"
	testInternalDNSName   = "internal-dns"
	testEnvoyProxyName    = "envoy-proxy"
	testCustomSvcHost     = "custom.svc"

	modeExternalDNSStr = "external-dns"
	modeCorednsImport  = "coredns-import"
	modeEtcHostsStr    = "etc-hosts"
	outputServiceStr   = "service"

	testCorednsImportCM     = "coredns-custom"
	testCorednsImportKeyVal = "ouroboros.override"

	flagClusterDomain        = "--cluster-domain"
	flagLogLevel             = "--log-level"
	testLogLevelDebug        = "debug"
	testCozyLocal            = "cozy.local"
	testK8sExampleCom        = "k8s.example.com"
	testFromFlagDomain       = "from-flag.local"
	testProxyFQDNCozyTenant  = "ouroboros-proxy.tenant.svc.cozy.local."
	testProxyFQDNClusterRoot = "ouroboros-proxy.tenant.svc.cluster.local."
	testK8sExampleOrg        = "k8s.example.org"
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
		"--listen-http", testListenAddr,
		flagTargetHost, testCustomSvcHost,
		"--target-http-port", "8081",
	})
	if err != nil {
		t.Fatalf("ParseProxyFlags: %v", err)
	}

	if cfg.HTTPListen != testListenAddr {
		t.Errorf("HTTPListen = %q, want :9090", cfg.HTTPListen)
	}

	if cfg.BackendHost != testCustomSvcHost {
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

	cfg, err := config.ParseProxyFlags([]string{flagTargetHost, testFlaghostFQDN})
	if err != nil {
		t.Fatalf("ParseProxyFlags: %v", err)
	}

	if cfg.BackendHost != testFlaghostFQDN {
		t.Errorf("BackendHost = %q, want flaghost (flag must override env)", cfg.BackendHost)
	}
}

func TestParseControllerFlags_DefaultsToCoreDNSMode(t *testing.T) {
	t.Parallel()

	// Pass --proxy-fqdn explicitly so default (coredns) mode's resolver
	// does not require runtime-composition shape — this test asserts
	// mode + namespace defaults, not FQDN resolution.
	cfg, err := config.ParseControllerFlags([]string{flagProxyFQDN, testProxyFQDNClusterRoot})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.Mode != config.ModeCoreDNS {
		t.Errorf("Mode = %q, want %q", cfg.Mode, config.ModeCoreDNS)
	}

	if cfg.CorednsNamespace != kubeSystemNS {
		t.Errorf("CorednsNamespace = %q, want %s", cfg.CorednsNamespace, kubeSystemNS)
	}
}

func TestParseControllerFlags_EtcHostsRequiresProxyIP(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{flagMode, modeEtcHostsStr})
	if err == nil {
		t.Fatal("etc-hosts mode without proxy-ip must fail validation")
	}
}

func TestParseControllerFlags_EtcHostsModeAcceptsValidFlags(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeEtcHostsStr,
		"--etc-hosts", "/tmp/hosts",
		"--proxy-ip", testProxyIPAddr,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.Mode != config.ModeEtcHosts {
		t.Errorf("Mode = %q, want %q", cfg.Mode, config.ModeEtcHosts)
	}

	if cfg.ProxyIP != testProxyIPAddr {
		t.Errorf("ProxyIP = %q", cfg.ProxyIP)
	}
}

func TestParseControllerFlags_DefaultsLogLevelToInfo(t *testing.T) {
	t.Parallel()

	// Without --log-level, the controller stays at info verbosity. main.go
	// reads cfg.LogLevel verbatim and passes it to slog.Level.UnmarshalText,
	// so the zero default has to be a string slog can parse.
	cfg, err := config.ParseControllerFlags([]string{flagProxyFQDN, testProxyFQDNClusterRoot})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestParseControllerFlags_LogLevelDebug(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagProxyFQDN, testProxyFQDNClusterRoot,
		flagLogLevel, testLogLevelDebug,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.LogLevel != testLogLevelDebug {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, testLogLevelDebug)
	}
}

func TestParseControllerFlags_LogLevelCachedAfterValidate(t *testing.T) {
	t.Parallel()

	// Validate runs SlogLevel once; runController will call it again to
	// build the slog.Logger. The cache makes the second call infallible
	// by construction — so even if a future refactor moves the SlogLevel
	// call out of validateCommon, runController will still see the same
	// level Validate accepted. Pin both halves of the contract.
	cfg, err := config.ParseControllerFlags([]string{
		flagProxyFQDN, testProxyFQDNClusterRoot,
		flagLogLevel, testLogLevelDebug,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	level, levelErr := cfg.SlogLevel()
	if levelErr != nil {
		t.Fatalf("SlogLevel after Validate: %v", levelErr)
	}

	if level != slog.LevelDebug {
		t.Fatalf("SlogLevel = %v, want %v", level, slog.LevelDebug)
	}

	// Mutate LogLevel to a value that would fail to parse fresh — the
	// cached resolution from Validate must still surface. This is the
	// exact failure mode the cache exists to prevent: a partially-mutated
	// config object should not throw at startup after Validate accepted.
	cfg.LogLevel = "this-would-not-parse"

	level2, level2Err := cfg.SlogLevel()
	if level2Err != nil {
		t.Fatalf("SlogLevel second call after cache: %v", level2Err)
	}

	if level2 != slog.LevelDebug {
		t.Fatalf("cached SlogLevel = %v, want %v", level2, slog.LevelDebug)
	}
}

func TestParseControllerFlags_LogLevelRejectsInvalid(t *testing.T) {
	t.Parallel()

	// Validation runs slog.Level.UnmarshalText, so an unknown level (like
	// "verbose" or a typo) must fail at parse time rather than silently
	// keep the previous default — operators relying on debug to diagnose
	// CI flakes deserve a fast failure rather than a quiet downgrade.
	_, err := config.ParseControllerFlags([]string{
		flagProxyFQDN, testProxyFQDNClusterRoot,
		flagLogLevel, "verbose",
	})
	if err == nil {
		t.Fatal("invalid --log-level must fail validation")
	}
}

func TestParseControllerFlags_RejectsUnknownMode(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{flagMode, testBogusValue})
	if err == nil {
		t.Fatal("unknown mode must fail validation")
	}
}

func TestMode_NeedsCorednsRewriteCheck(t *testing.T) {
	t.Parallel()

	// Modes that mutate (directly or via import) what CoreDNS resolves
	// must run the node-local-dns startup probe — node-local-dns
	// forwards non-cluster.local queries upstream, bypassing whatever
	// rewrites we install. Modes that don't touch CoreDNS resolution
	// (etc-hosts, external-dns) do not need the probe.
	cases := []struct {
		mode config.Mode
		want bool
	}{
		{config.ModeCoreDNS, true},
		{config.ModeCorednsImport, true},
		{config.ModeEtcHosts, false},
		{config.ModeExternalDNS, false},
	}

	for _, tc := range cases {
		got := tc.mode.NeedsCorednsRewriteCheck()
		if got != tc.want {
			t.Errorf("Mode(%q).NeedsCorednsRewriteCheck() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestParseControllerFlags_ModeFlagHelpListsEveryMode(t *testing.T) {
	t.Parallel()

	// The --mode flag's Usage string is the only place an operator
	// running `ouroboros controller --help` learns about supported
	// modes. A new Mode constant added without updating this string
	// silently disappears from --help. Pin it here.
	helpText := config.ModeFlagUsage()

	wantedModes := []config.Mode{
		config.ModeCoreDNS,
		config.ModeCorednsImport,
		config.ModeEtcHosts,
		config.ModeExternalDNS,
	}

	for _, mode := range wantedModes {
		if !strings.Contains(helpText, string(mode)) {
			t.Errorf("--mode help text missing %q (full text: %q)", mode, helpText)
		}
	}
}

func TestParseControllerFlags_ClusterDomain_DefaultsToNonEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{flagProxyFQDN, testProxyFQDNClusterRoot})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ClusterDomain == "" {
		t.Errorf("ClusterDomain = %q, want non-empty (auto-detected from /etc/resolv.conf or DefaultClusterDomain fallback)", cfg.ClusterDomain)
	}
}

func TestParseControllerFlags_ClusterDomain_FlagOverride(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagClusterDomain, testCozyLocal,
		flagProxyFQDN, testProxyFQDNCozyTenant,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ClusterDomain != testCozyLocal {
		t.Errorf("ClusterDomain = %q, want cozy.local", cfg.ClusterDomain)
	}
}

func TestParseControllerFlags_ClusterDomain_EnvOverride(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_CLUSTER_DOMAIN", testK8sExampleCom)

	cfg, err := config.ParseControllerFlags([]string{flagProxyFQDN, testProxyFQDNClusterRoot})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ClusterDomain != testK8sExampleCom {
		t.Errorf("ClusterDomain = %q, want k8s.example.com", cfg.ClusterDomain)
	}
}

func TestParseControllerFlags_ClusterDomain_FlagOverridesEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_CLUSTER_DOMAIN", "from-env.local")

	cfg, err := config.ParseControllerFlags([]string{
		flagClusterDomain, testFromFlagDomain,
		flagProxyFQDN, testProxyFQDNClusterRoot,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ClusterDomain != testFromFlagDomain {
		t.Errorf("ClusterDomain = %q, want from-flag.local (flag must win over env)", cfg.ClusterDomain)
	}
}

// ResolveProxyFQDN — runtime composition of the rewrite-target FQDN
// from --proxy-service-name + --proxy-service-namespace + cluster-domain
// when an explicit --proxy-fqdn was not passed by the chart. Lets the
// chart stay platform-agnostic: it can omit cluster-domain entirely and
// the controller composes the right FQDN at startup using whatever
// cluster-domain auto-detect (or env override) resolved.

const (
	flagProxyFQDN             = "--proxy-fqdn"
	flagProxyServiceName      = "--proxy-service-name"
	flagProxyServiceNamespace = "--proxy-service-namespace"
	testProxySvcName          = "ouroboros-proxy"
	testProxySvcNamespace     = "cozy-ouroboros"
	// testCozyLocalFQDN is the FQDN-style ("trailing dot") form of
	// cozy.local — used by tests that exercise trailing-dot stripping
	// in the runtime composers.
	testCozyLocalFQDN = "cozy.local."
	// testComposedFQDNCozy pins the canonical ResolveProxyFQDN composition
	// for the {testProxySvcName, testProxySvcNamespace, testCozyLocal}
	// triple — used by every "compose-from-service" test below.
	testComposedFQDNCozy = "ouroboros-proxy.cozy-ouroboros.svc.cozy.local."
)

func TestResolveProxyFQDN_ReturnsExplicitProxyFQDNAsIs(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = testProxyFQDNCozyTenant
	cfg.ClusterDomain = testCozyLocal
	// Service-name/namespace are also set, but ProxyFQDN must win as the
	// explicit-override path. Keeps backward compat for charts/operators
	// that already pass a fully-baked --proxy-fqdn.
	cfg.ProxyServiceName = testProxySvcName
	cfg.ProxyServiceNamespace = testProxySvcNamespace

	got, err := cfg.ResolveProxyFQDN()
	if err != nil {
		t.Fatalf("ResolveProxyFQDN: %v", err)
	}

	if got != testProxyFQDNCozyTenant {
		t.Errorf("ResolveProxyFQDN = %q, want explicit ProxyFQDN passed through unchanged", got)
	}
}

func TestResolveProxyFQDN_ComposesFromServiceWhenProxyFQDNEmpty(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = ""
	cfg.ProxyServiceName = testProxySvcName
	cfg.ProxyServiceNamespace = testProxySvcNamespace
	cfg.ClusterDomain = testCozyLocal

	got, err := cfg.ResolveProxyFQDN()
	if err != nil {
		t.Fatalf("ResolveProxyFQDN: %v", err)
	}

	want := testComposedFQDNCozy
	if got != want {
		t.Errorf("ResolveProxyFQDN = %q, want %q (composed from service + namespace + cluster-domain)", got, want)
	}
}

func TestResolveProxyFQDN_StripsTrailingDotFromClusterDomain(t *testing.T) {
	t.Parallel()

	// Operator may type cluster-domain as "cozy.local." (FQDN style). The
	// composed result must not end with a double dot — that's invalid for
	// CoreDNS rewrite name targets.
	cfg := config.DefaultController()
	cfg.ProxyFQDN = ""
	cfg.ProxyServiceName = testProxySvcName
	cfg.ProxyServiceNamespace = testProxySvcNamespace
	cfg.ClusterDomain = testCozyLocalFQDN

	got, err := cfg.ResolveProxyFQDN()
	if err != nil {
		t.Fatalf("ResolveProxyFQDN: %v", err)
	}

	want := testComposedFQDNCozy
	if got != want {
		t.Errorf("ResolveProxyFQDN = %q, want %q (single trailing dot)", got, want)
	}
}

func TestResolveProxyFQDN_ErrorsWhenServiceNameMissing(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = ""
	cfg.ProxyServiceName = ""
	cfg.ProxyServiceNamespace = testProxySvcNamespace
	cfg.ClusterDomain = testCozyLocal

	_, err := cfg.ResolveProxyFQDN()
	if err == nil {
		t.Fatal("ResolveProxyFQDN: want error when proxy-service-name is empty AND proxy-fqdn is empty")
	}
}

func TestResolveProxyFQDN_ErrorsWhenServiceNamespaceMissing(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = ""
	cfg.ProxyServiceName = testProxySvcName
	cfg.ProxyServiceNamespace = ""
	cfg.ClusterDomain = testCozyLocal

	_, err := cfg.ResolveProxyFQDN()
	if err == nil {
		t.Fatal("ResolveProxyFQDN: want error when proxy-service-namespace is empty AND proxy-fqdn is empty")
	}
}

func TestResolveProxyFQDN_ErrorsWhenClusterDomainMissing(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = ""
	cfg.ProxyServiceName = testProxySvcName
	cfg.ProxyServiceNamespace = testProxySvcNamespace
	cfg.ClusterDomain = ""

	_, err := cfg.ResolveProxyFQDN()
	if err == nil {
		t.Fatal("ResolveProxyFQDN: want error when cluster-domain is empty AND proxy-fqdn is empty " +
			"(auto-detect must have populated it before this call)")
	}
}

func TestParseControllerFlags_AcceptsProxyServiceNameFlag(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagProxyServiceName, testProxySvcName,
		flagProxyServiceNamespace, testProxySvcNamespace,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ProxyServiceName != testProxySvcName {
		t.Errorf("ProxyServiceName = %q, want %q", cfg.ProxyServiceName, testProxySvcName)
	}

	if cfg.ProxyServiceNamespace != testProxySvcNamespace {
		t.Errorf("ProxyServiceNamespace = %q, want %q", cfg.ProxyServiceNamespace, testProxySvcNamespace)
	}
}

func TestParseControllerFlags_AcceptsProxyServiceNameEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_PROXY_SERVICE_NAME", testProxySvcName)
	t.Setenv("OUROBOROS_CONTROLLER_PROXY_SERVICE_NAMESPACE", testProxySvcNamespace)

	cfg, err := config.ParseControllerFlags(nil)
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ProxyServiceName != testProxySvcName {
		t.Errorf("ProxyServiceName = %q, want %q (from env)", cfg.ProxyServiceName, testProxySvcName)
	}

	if cfg.ProxyServiceNamespace != testProxySvcNamespace {
		t.Errorf("ProxyServiceNamespace = %q, want %q (from env)", cfg.ProxyServiceNamespace, testProxySvcNamespace)
	}
}

// ResolveBackendHost — runtime composition of the proxy's backend
// (the in-cluster ingress controller) host from --target-service-name +
// --target-service-namespace + cluster-domain. Same shape as
// ResolveProxyFQDN: explicit --target-host overrides; otherwise compose.
// Lets the chart stay platform-agnostic on the proxy side too — it can
// pass {ingress-nginx-controller, ingress-nginx} without baking a cluster
// suffix at template time.

const (
	flagTargetServiceName      = "--target-service-name"
	flagTargetServiceNamespace = "--target-service-namespace"
	testTargetSvcName          = "ingress-nginx-controller"
	testTargetSvcNamespace     = "cozy-ingress-nginx"
	testComposedBackendCozy    = "ingress-nginx-controller.cozy-ingress-nginx.svc.cozy.local"
)

func TestResolveBackendHost_ReturnsExplicitBackendHostAsIs(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultProxy()
	cfg.BackendHost = testCustomSvcHost
	cfg.BackendServiceName = testTargetSvcName
	cfg.BackendServiceNamespace = testTargetSvcNamespace
	cfg.ClusterDomain = testCozyLocal

	got, err := cfg.ResolveBackendHost()
	if err != nil {
		t.Fatalf("ResolveBackendHost: %v", err)
	}

	if got != testCustomSvcHost {
		t.Errorf("ResolveBackendHost = %q, want explicit BackendHost passed through unchanged", got)
	}
}

func TestResolveBackendHost_ComposesFromServiceWhenBackendHostEmpty(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultProxy()
	cfg.BackendHost = ""
	cfg.BackendServiceName = testTargetSvcName
	cfg.BackendServiceNamespace = testTargetSvcNamespace
	cfg.ClusterDomain = testCozyLocal

	got, err := cfg.ResolveBackendHost()
	if err != nil {
		t.Fatalf("ResolveBackendHost: %v", err)
	}

	if got != testComposedBackendCozy {
		t.Errorf("ResolveBackendHost = %q, want %q", got, testComposedBackendCozy)
	}
}

func TestResolveBackendHost_StripsTrailingDotFromClusterDomain(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultProxy()
	cfg.BackendHost = ""
	cfg.BackendServiceName = testTargetSvcName
	cfg.BackendServiceNamespace = testTargetSvcNamespace
	cfg.ClusterDomain = testCozyLocalFQDN

	got, err := cfg.ResolveBackendHost()
	if err != nil {
		t.Fatalf("ResolveBackendHost: %v", err)
	}

	// Backend host is consumed by net.Dial — must NOT end with a dot
	// (Dial does not accept FQDN-style trailing dot in resolver lookups
	// the same way CoreDNS rewrites do).
	if got != testComposedBackendCozy {
		t.Errorf("ResolveBackendHost = %q, want %q (no trailing dot for Dial)", got, testComposedBackendCozy)
	}
}

func TestResolveBackendHost_ErrorsWhenServiceNameMissing(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultProxy()
	cfg.BackendHost = ""
	cfg.BackendServiceName = ""
	cfg.BackendServiceNamespace = testTargetSvcNamespace
	cfg.ClusterDomain = testCozyLocal

	_, err := cfg.ResolveBackendHost()
	if err == nil {
		t.Fatal("ResolveBackendHost: want error when target-service-name empty AND target-host empty")
	}
}

func TestResolveBackendHost_ErrorsWhenClusterDomainMissing(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultProxy()
	cfg.BackendHost = ""
	cfg.BackendServiceName = testTargetSvcName
	cfg.BackendServiceNamespace = testTargetSvcNamespace
	cfg.ClusterDomain = ""

	_, err := cfg.ResolveBackendHost()
	if err == nil {
		t.Fatal("ResolveBackendHost: want error when cluster-domain empty AND target-host empty")
	}
}

func TestParseProxyFlags_AcceptsTargetServiceNameFlag(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseProxyFlags([]string{
		flagTargetServiceName, testTargetSvcName,
		flagTargetServiceNamespace, testTargetSvcNamespace,
	})
	if err != nil {
		t.Fatalf("ParseProxyFlags: %v", err)
	}

	if cfg.BackendServiceName != testTargetSvcName {
		t.Errorf("BackendServiceName = %q, want %q", cfg.BackendServiceName, testTargetSvcName)
	}

	if cfg.BackendServiceNamespace != testTargetSvcNamespace {
		t.Errorf("BackendServiceNamespace = %q, want %q", cfg.BackendServiceNamespace, testTargetSvcNamespace)
	}
}

// Pin the regression the reviewer flagged: with chart-style argv that
// passes --proxy-service-name + --proxy-service-namespace + --cluster-domain
// AND OMITS --proxy-fqdn, the resolved FQDN must reflect the supplied
// cluster-domain (NOT a stale "cluster.local" default seed). On master
// before this change a non-empty DefaultController.ProxyFQDN seed
// short-circuited ResolveProxyFQDN and silently shadowed the chart flags,
// breaking every non-default-domain cluster (cozystack tenants, federations,
// custom domains). The test must fail without the empty-seed fix.
func TestParseControllerFlags_RuntimeCompositionWinsWhenProxyFQDNUnset(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, "coredns",
		flagProxyServiceName, testProxySvcName,
		flagProxyServiceNamespace, testProxySvcNamespace,
		flagClusterDomain, testCozyLocal,
		// no --proxy-fqdn — chart-default platform-agnostic shape
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ProxyFQDN != testComposedFQDNCozy {
		t.Errorf("ProxyFQDN = %q, want %q (composition path must win when --proxy-fqdn unset; "+
			"a non-empty default seed would silently shadow the chart-supplied service flags)",
			cfg.ProxyFQDN, testComposedFQDNCozy)
	}
}

func TestParseProxyFlags_RuntimeCompositionWinsWhenTargetHostUnset(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseProxyFlags([]string{
		flagTargetServiceName, testTargetSvcName,
		flagTargetServiceNamespace, testTargetSvcNamespace,
		flagClusterDomain, testCozyLocal,
		// no --target-host — chart-default platform-agnostic shape
	})
	if err != nil {
		t.Fatalf("ParseProxyFlags: %v", err)
	}

	if cfg.BackendHost != testComposedBackendCozy {
		t.Errorf("BackendHost = %q, want %q (composition path must win when --target-host unset; "+
			"a non-empty default seed would silently shadow the chart-supplied service flags)",
			cfg.BackendHost, testComposedBackendCozy)
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

func TestParseControllerFlags_DefaultsExternalDNSOutputToCRD(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSOutput != config.OutputCRD {
		t.Fatalf("default output = %q, want %q (backwards compatibility for v0.2/v0.3 users)",
			cfg.ExternalDNSOutput, config.OutputCRD)
	}
}

func TestParseControllerFlags_ExternalDNSOutput_AcceptsService(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSOutput, outputServiceStr,
		flagExternalDNSAnnotationPrefix, internalDNSAnnotPrefix,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSOutput != config.OutputService {
		t.Fatalf("output = %q, want service", cfg.ExternalDNSOutput)
	}

	if cfg.ExternalDNSAnnotationPrefix != internalDNSAnnotPrefix {
		t.Fatalf("prefix = %q, want %s", cfg.ExternalDNSAnnotationPrefix, internalDNSAnnotPrefix)
	}
}

func TestParseControllerFlags_ExternalDNSOutput_ServiceRequiresAnnotationPrefix(t *testing.T) {
	t.Parallel()

	// Service-mode with empty prefix would emit annotations under no
	// namespace at all — external-dns would never see them. Reject at
	// parse time so an operator with --external-dns-output=service but
	// without --external-dns-annotation-prefix gets a clear error.
	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSOutput, outputServiceStr,
		flagExternalDNSAnnotationPrefix, "",
	})
	if err == nil {
		t.Fatal("output=service without annotation-prefix must fail")
	}
}

func TestParseControllerFlags_ExternalDNSMode_CRD_RejectsBadAnnotationPrefix(t *testing.T) {
	t.Parallel()

	// AnnotationPrefix is unused in crd mode today, but a typo'd
	// value (no trailing '/') would silently survive validation only
	// to fail later when the operator flips externalDns.output to
	// service. Catch it at parse time regardless of active mode.
	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSOutput, "crd",
		flagExternalDNSAnnotationPrefix, testInternalDNSName,
	})
	if err == nil {
		t.Fatal("annotation-prefix without trailing '/' must fail validation in crd mode too")
	}
}

func TestParseControllerFlags_ExternalDNSOutput_RejectsAnnotationPrefixWithoutTrailingSlash(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSOutput, outputServiceStr,
		flagExternalDNSAnnotationPrefix, testInternalDNSName,
	})
	if err == nil {
		t.Fatal("annotation-prefix without trailing '/' must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSOutput_RejectsUnknownValue(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSOutput, testBogusValue,
	})
	if err == nil {
		t.Fatal("unknown output must fail validation")
	}
}

func TestParseControllerFlags_GatewayClassRequiresGatewayAPI(t *testing.T) {
	t.Parallel()

	// Setting --gateway-class without --gateway-api is a silent no-op
	// because the Gateway informer never starts. Catch the misconfig at
	// parse time so the operator gets a clear error instead of staring
	// at an unfiltered controller and wondering why nothing is filtered.
	_, err := config.ParseControllerFlags([]string{
		"--gateway-class", testEnvoyProxyName,
	})
	if err == nil {
		t.Fatal("--gateway-class without --gateway-api must fail validation")
	}
}

func TestParseControllerFlags_HonoursIngressClassEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_INGRESS_CLASS", "nginx-proxy")

	cfg, err := config.ParseControllerFlags([]string{flagProxyFQDN, testProxyFQDNClusterRoot})
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
	t.Setenv("OUROBOROS_CONTROLLER_GATEWAY_CLASS", testEnvoyProxyName)

	cfg, err := config.ParseControllerFlags([]string{flagProxyFQDN, testProxyFQDNClusterRoot})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.GatewayClass != testEnvoyProxyName {
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
	cfg.ProxyFQDN = proxyFQDNNoDot

	err := cfg.Validate()
	if err == nil {
		t.Fatal("coredns mode with non-FQDN proxy-fqdn must fail validation at config time")
	}
}

func TestValidate_CorednsModeAcceptsFQDNWithTrailingDot(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.ProxyFQDN = proxyFQDNNoDot + "."

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("trailing-dot FQDN must validate: %v", err)
	}
}

func TestParseControllerFlags_DefaultsCorednsImportFields(t *testing.T) {
	t.Parallel()

	// Defaults must be populated even when mode != coredns-import so an
	// operator flipping to coredns-import without retyping every flag
	// gets a sensible target ConfigMap, not empty strings that would
	// fail validation. Pass --proxy-fqdn explicitly so the default
	// (coredns) mode's ResolveProxyFQDN doesn't trip on the empty
	// runtime-composition shape — this test asserts CorednsImport
	// defaults, not the FQDN resolver.
	cfg, err := config.ParseControllerFlags([]string{
		flagProxyFQDN, testProxyFQDNClusterRoot,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.CorednsImportNamespace != kubeSystemNS {
		t.Errorf("CorednsImportNamespace = %q, want %s", cfg.CorednsImportNamespace, kubeSystemNS)
	}

	if cfg.CorednsImportConfigMap != testCorednsImportCM {
		t.Errorf("CorednsImportConfigMap = %q, want coredns-custom", cfg.CorednsImportConfigMap)
	}

	if cfg.CorednsImportKey != testCorednsImportKeyVal {
		t.Errorf("CorednsImportKey = %q, want ouroboros.override", cfg.CorednsImportKey)
	}
}

func TestParseControllerFlags_CorednsImportModeAcceptsValidFlags(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeCorednsImport,
		"--coredns-import-namespace", kubeSystemNS,
		"--coredns-import-configmap", testCorednsImportCM,
		"--coredns-import-key", testCorednsImportKeyVal,
		flagProxyFQDN, testProxyFQDNClusterRoot,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.Mode != config.ModeCorednsImport {
		t.Errorf("Mode = %q, want %q", cfg.Mode, config.ModeCorednsImport)
	}
}

func TestValidate_CorednsImportModeRequiresProxyFQDN(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.Mode = config.ModeCorednsImport
	cfg.ProxyFQDN = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("coredns-import mode without proxy-fqdn must fail validation")
	}
}

func TestValidate_CorednsImportModeRequiresTrailingDotInProxyFQDN(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.Mode = config.ModeCorednsImport
	cfg.ProxyFQDN = proxyFQDNNoDot

	err := cfg.Validate()
	if err == nil {
		t.Fatal("coredns-import mode with non-FQDN proxy-fqdn must fail validation at config time")
	}
}

func TestValidate_CorednsImportModeRejectsEmptyConfigMap(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultController()
	cfg.Mode = config.ModeCorednsImport
	cfg.CorednsImportConfigMap = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("coredns-import mode with empty configmap must fail validation")
	}
}

func TestParseControllerFlags_CorednsImportHonoursEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_COREDNS_IMPORT_NAMESPACE", "custom-ns")
	t.Setenv("OUROBOROS_CONTROLLER_COREDNS_IMPORT_CONFIGMAP", "my-coredns-custom")
	t.Setenv("OUROBOROS_CONTROLLER_COREDNS_IMPORT_KEY", "my.override")

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeCorednsImport,
		flagProxyFQDN, testProxyFQDNClusterRoot,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.CorednsImportNamespace != "custom-ns" {
		t.Errorf("CorednsImportNamespace = %q, want custom-ns", cfg.CorednsImportNamespace)
	}

	if cfg.CorednsImportConfigMap != "my-coredns-custom" {
		t.Errorf("CorednsImportConfigMap = %q, want my-coredns-custom", cfg.CorednsImportConfigMap)
	}

	if cfg.CorednsImportKey != "my.override" {
		t.Errorf("CorednsImportKey = %q, want my.override", cfg.CorednsImportKey)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RequiresProxyIPOrService(t *testing.T) {
	t.Parallel()

	// Default has ExternalDNSProxyService=ouroboros-proxy, so blank both to
	// reproduce the "operator forgot to set anything" failure mode.
	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		"--external-dns-proxy-service", "",
	})
	if err == nil {
		t.Fatal("external-dns mode with neither proxy-ip nor service must fail")
	}
}

func TestParseControllerFlags_ExternalDNSMode_ProxyServiceDefaultIsValid(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{flagMode, modeExternalDNSStr})
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSProxyIP != testExternalDNSProxIP {
		t.Fatalf("ExternalDNSProxyIP = %q, want 10.42.0.7", cfg.ExternalDNSProxyIP)
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsInvalidNamespace(t *testing.T) {
	t.Parallel()

	// Uppercase namespace would crash kube-apiserver with a confusing error;
	// validation catches it locally.
	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSNamespace, "BadNamespace",
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
			flagMode, modeExternalDNSStr,
			flagExternalDNSProxyIP, testExternalDNSProxIP,
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSAnnotation, "external-dns.alpha.kubernetes.io/cloudflare-proxied=false",
		flagExternalDNSAnnotation, "external-dns.alpha.kubernetes.io/aws-region=us-east-1",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSAnnotation, "no-equals-sign",
	})
	if err == nil {
		t.Fatal("annotation flag without '=' must fail")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsBadAnnotationKey(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSAnnotation, "bad key with spaces=value",
	})
	if err == nil {
		t.Fatal("annotation key with spaces must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_HonoursTTLEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_EXTERNAL_DNS_RECORD_TTL", "300")

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
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

func TestParseControllerFlags_ExternalDNSMode_HonoursOutputEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_EXTERNAL_DNS_OUTPUT", outputServiceStr)

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSOutput != config.OutputService {
		t.Fatalf("ExternalDNSOutput = %q, want %q (from env)",
			cfg.ExternalDNSOutput, config.OutputService)
	}
}

func TestParseControllerFlags_ExternalDNSMode_HonoursAnnotationPrefixEnv(t *testing.T) {
	t.Setenv("OUROBOROS_CONTROLLER_EXTERNAL_DNS_ANNOTATION_PREFIX", "internal-dns/")

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSAnnotationPrefix != "internal-dns/" {
		t.Fatalf("ExternalDNSAnnotationPrefix = %q, want %q (from env)",
			cfg.ExternalDNSAnnotationPrefix, "internal-dns/")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsInvalidProxyIP(t *testing.T) {
	t.Parallel()

	// A typo'd proxy IP would let the controller start, but every Build
	// would then fail to parse the literal — leaving desired empty and
	// causing prune to delete every ouroboros-owned DNSEndpoint. Catch it
	// at config time instead.
	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, "10.42.0.999",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSAnnotation, "ouroboros.lexfrei.tech/source=user-override",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSNamespace, overlong,
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

const (
	teamLabelValue         = "platform"
	internalDNSAnnotPrefix = "internal-dns/"
	kubeSystemNS           = "kube-system"
	proxyFQDNNoDot         = "ouroboros-proxy.ouroboros.svc.cluster.local"
)

func TestParseControllerFlags_ExternalDNSMode_AcceptsLabelPassthrough(t *testing.T) {
	t.Parallel()

	cfg, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "external-dns-instance=internal-dns",
		flagExternalDNSLabel, "team=" + teamLabelValue,
	})
	if err != nil {
		t.Fatalf("ParseControllerFlags: %v", err)
	}

	if cfg.ExternalDNSLabels["external-dns-instance"] != testInternalDNSName {
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, long + "=v",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "company.io/external-dns-instance=internal",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "/foo=v",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, overlong + "/name=v",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "presence-marker=",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "Company.io/foo=v",
	})
	if err == nil {
		t.Fatal("uppercase in label-key prefix must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsLabelPrefixWithUnderscore(t *testing.T) {
	t.Parallel()

	// DNS-1123 subdomain disallows underscores in label prefixes.
	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "under_score.io/foo=v",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "a.b.c/team=" + teamLabelValue,
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "_bad=v",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "app.kubernetes.io/managed-by=imposter",
	})
	if err == nil {
		t.Fatal("reserved managed-by label key must fail validation")
	}
}

func TestParseControllerFlags_ExternalDNSMode_RejectsReservedInstanceLabel(t *testing.T) {
	t.Parallel()

	_, err := config.ParseControllerFlags([]string{
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSLabel, "ouroboros.lexfrei.tech/instance=wrong-release",
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
		flagMode, modeExternalDNSStr,
		flagExternalDNSProxyIP, testExternalDNSProxIP,
		flagExternalDNSAnnotation, "external-dns.alpha.kubernetes.io/target=evil.example.com",
	})
	if err == nil {
		t.Fatal("reserved external-dns target annotation key must fail validation")
	}
}
