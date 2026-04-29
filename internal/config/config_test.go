package config_test

import (
	"testing"
	"time"

	"github.com/lexfrei/ouroboros/internal/config"
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
