package externaldns_test

import (
	"strings"
	"testing"

	"github.com/lexfrei/ouroboros/internal/externaldns"
)

const (
	testInstance  = "ouroboros"
	testNamespace = "ouroboros"
	testHost      = "foo.example.com"
	v4Target      = "10.42.0.7"
	v6Target      = "fd00::7"
)

func mustBuild(t *testing.T, opts externaldns.BuildOpts) []externaldns.Endpoint {
	t.Helper()

	out, err := externaldns.Build(&opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return out
}

func TestBuild_SingleStack_V4_AnRecord(t *testing.T) {
	t.Parallel()

	got := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	if len(got) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(got))
	}

	if got[0].RecordType != "A" {
		t.Fatalf("got RecordType %q, want A", got[0].RecordType)
	}

	if len(got[0].Targets) != 1 || got[0].Targets[0] != v4Target {
		t.Fatalf("got targets %v, want [%s]", got[0].Targets, v4Target)
	}
}

func TestBuild_SingleStack_V6_AAAARecord(t *testing.T) {
	t.Parallel()

	got := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v6Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	if len(got) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(got))
	}

	if got[0].RecordType != "AAAA" {
		t.Fatalf("got RecordType %q, want AAAA", got[0].RecordType)
	}
}

func TestBuild_DualStack_TwoObjects_DistinctNames(t *testing.T) {
	t.Parallel()

	got := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target, v6Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2 (one per family)", len(got))
	}

	types := map[string]bool{got[0].RecordType: true, got[1].RecordType: true}
	if !types["A"] || !types["AAAA"] {
		t.Fatalf("got record types %v, want A+AAAA", types)
	}

	if got[0].Name == got[1].Name {
		t.Fatalf("dual-stack endpoints share name %q — must be unique", got[0].Name)
	}
}

func TestBuild_TTLDefault_60WhenZero(t *testing.T) {
	t.Parallel()

	got := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target}, TTL: 0,
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	if got[0].RecordTTL != 60 {
		t.Fatalf("got TTL %d, want 60 (default)", got[0].RecordTTL)
	}
}

func TestBuild_TTLExplicit_PassThrough(t *testing.T) {
	t.Parallel()

	got := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target}, TTL: 300,
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	if got[0].RecordTTL != 300 {
		t.Fatalf("got TTL %d, want 300", got[0].RecordTTL)
	}
}

func TestBuild_LabelsAndOwnership(t *testing.T) {
	t.Parallel()

	got := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceGatewayListener, Instance: "myrelease", Namespace: testNamespace,
	})

	labels := got[0].Labels
	if labels[externaldns.LabelManagedBy] != "ouroboros" {
		t.Fatalf("missing managed-by label: %v", labels)
	}

	if labels[externaldns.LabelInstance] != "myrelease" {
		t.Fatalf("missing instance label: %v", labels)
	}
}

func TestBuild_SourceAnnotation_PerSourceType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  externaldns.Source
		want string
	}{
		{externaldns.SourceIngress, "ingress"},
		{externaldns.SourceGatewayListener, "gateway-listener"},
		{externaldns.SourceHTTPRoute, "httproute"},
	}
	for _, tt := range cases {
		got := mustBuild(t, externaldns.BuildOpts{
			Host: testHost, Targets: []string{v4Target},
			Source: tt.src, Instance: testInstance, Namespace: testNamespace,
		})
		if got[0].Annotations[externaldns.AnnotationSource] != tt.want {
			t.Errorf("source %s: got annotation %q, want %q",
				tt.src, got[0].Annotations[externaldns.AnnotationSource], tt.want)
		}
	}
}

func TestBuild_NameSanitization_LowercaseAndDotsReplaced(t *testing.T) {
	t.Parallel()

	got := mustBuild(t, externaldns.BuildOpts{
		Host: "Foo.Example.COM", Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	name := got[0].Name
	if strings.ContainsAny(name, ".ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Fatalf("name %q contains uppercase or dot — must be RFC 1123-safe", name)
	}

	if !strings.HasPrefix(name, "ouroboros-") {
		t.Fatalf("name %q must be prefixed with ouroboros-", name)
	}
}

func TestBuild_NameSanitization_LongHost_TruncatedWithHash(t *testing.T) {
	t.Parallel()

	// 280-char host — Kubernetes object names cap at 253. Truncation must
	// preserve uniqueness via a deterministic hash suffix to avoid collisions
	// between hosts that share the leading 244 chars.
	long := strings.Repeat("a", 100) + "." + strings.Repeat("b", 100) + "." + strings.Repeat("c", 80)
	got := mustBuild(t, externaldns.BuildOpts{
		Host: long, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	name := got[0].Name
	if len(name) > 253 {
		t.Fatalf("name length %d exceeds 253", len(name))
	}

	// Sibling host that shares the first 244 chars but differs after — names
	// must NOT collide.
	long2 := strings.Repeat("a", 100) + "." + strings.Repeat("b", 100) + "." + strings.Repeat("d", 80)
	got2 := mustBuild(t, externaldns.BuildOpts{
		Host: long2, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	if name == got2[0].Name {
		t.Fatalf("truncated names collided for hosts differing in the tail: %s", name)
	}
}

func TestBuild_NameSanitization_WildcardHostsRejected(t *testing.T) {
	t.Parallel()

	// ExtractHostnames upstream filters wildcards, but Build must defend
	// itself: an unsanitised "*" in a hostname produces an invalid k8s
	// resource name, which the API server then 400s with a confusing message.
	_, err := externaldns.Build(&externaldns.BuildOpts{
		Host: "*.example.com", Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})
	if err == nil {
		t.Fatal("Build: want error for wildcard host, got nil")
	}
}

func TestBuild_RejectsEmptyHost(t *testing.T) {
	t.Parallel()

	_, err := externaldns.Build(&externaldns.BuildOpts{
		Host: "", Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})
	if err == nil {
		t.Fatal("Build: want error for empty host, got nil")
	}
}

func TestBuild_RejectsNoTargets(t *testing.T) {
	t.Parallel()

	_, err := externaldns.Build(&externaldns.BuildOpts{
		Host: testHost, Targets: nil,
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})
	if err == nil {
		t.Fatal("Build: want error for empty targets, got nil")
	}
}

func TestBuild_RejectsInvalidTarget(t *testing.T) {
	t.Parallel()

	_, err := externaldns.Build(&externaldns.BuildOpts{
		Host: testHost, Targets: []string{"not-an-ip"},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})
	if err == nil {
		t.Fatal("Build: want error for non-IP target, got nil")
	}
}

func TestBuild_ProviderAnnotations_AppliedVerbatim(t *testing.T) {
	t.Parallel()

	got := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		Annotations: map[string]string{
			"external-dns.alpha.kubernetes.io/cloudflare-proxied": "false",
			"external-dns.alpha.kubernetes.io/aws-region":         "us-east-1",
		},
	})

	if got[0].Annotations["external-dns.alpha.kubernetes.io/cloudflare-proxied"] != "false" {
		t.Fatalf("cloudflare-proxied annotation missing: %v", got[0].Annotations)
	}

	if got[0].Annotations["external-dns.alpha.kubernetes.io/aws-region"] != "us-east-1" {
		t.Fatalf("aws-region annotation missing: %v", got[0].Annotations)
	}
}

func TestBuild_ProviderAnnotations_RejectsSourceCollision(t *testing.T) {
	t.Parallel()

	// The internal source annotation key is reserved; if an operator tries to
	// override it they likely misunderstood what the chart value does. Fail
	// loudly instead of silently overwriting.
	_, err := externaldns.Build(&externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		Annotations: map[string]string{
			externaldns.AnnotationSource: "user-trying-to-override",
		},
	})
	if err == nil {
		t.Fatal("Build: want collision error for reserved source annotation key, got nil")
	}
}

func TestBuild_DNSName_PreservesOriginalHost(t *testing.T) {
	t.Parallel()

	// .spec.endpoints[].dnsName must hold the original (lowercased) hostname,
	// not the sanitised k8s object name. external-dns reads dnsName when
	// publishing — sanitising it would break DNS resolution.
	got := mustBuild(t, externaldns.BuildOpts{
		Host: "Foo.Example.COM", Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	if got[0].DNSName != "foo.example.com" {
		t.Fatalf("got DNSName %q, want lowercased original 'foo.example.com'", got[0].DNSName)
	}
}

func TestBuild_Unstructured_RoundTripsCorrectly(t *testing.T) {
	t.Parallel()

	endpoints := mustBuild(t, externaldns.BuildOpts{
		Host: testHost, Targets: []string{v4Target}, TTL: 120,
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
	})

	uns, err := endpoints[0].ToUnstructured()
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}

	if uns.GetAPIVersion() != "externaldns.k8s.io/v1alpha1" {
		t.Fatalf("got apiVersion %q, want externaldns.k8s.io/v1alpha1", uns.GetAPIVersion())
	}

	if uns.GetKind() != "DNSEndpoint" {
		t.Fatalf("got kind %q, want DNSEndpoint", uns.GetKind())
	}

	if uns.GetNamespace() != testNamespace {
		t.Fatalf("got namespace %q, want %q", uns.GetNamespace(), testNamespace)
	}
}
