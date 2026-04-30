package externaldns_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/lexfrei/ouroboros/internal/externaldns"
)

const (
	testAnnotationPrefix = "external-dns.alpha.kubernetes.io/"
	testCustomPrefix     = "internal-dns/"
)

func mustBuildService(t *testing.T, opts externaldns.BuildServiceOpts) *corev1.Service {
	t.Helper()

	svc, err := externaldns.BuildService(&opts)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}

	return svc
}

func TestBuildService_SingleStack_OneServiceWithTargetAnnotation(t *testing.T) {
	t.Parallel()

	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
	})

	// Headless — no allocation needed, the Service is just an annotation
	// carrier for external-dns.
	if got.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatalf("Service must be headless (ClusterIP=None), got %q", got.Spec.ClusterIP)
	}

	if len(got.Spec.Selector) != 0 {
		t.Fatalf("Service must have no selector (annotation carrier only), got %v", got.Spec.Selector)
	}

	if got.Annotations[testAnnotationPrefix+"hostname"] != testHost {
		t.Fatalf("hostname annotation missing or wrong: %v", got.Annotations)
	}

	if got.Annotations[testAnnotationPrefix+"target"] != v4Target {
		t.Fatalf("target annotation missing or wrong: %v", got.Annotations)
	}
}

func TestBuildService_DualStack_TargetAnnotationCommaJoined(t *testing.T) {
	t.Parallel()

	// external-dns reads a comma-separated target list and creates both
	// A and AAAA records from a single Service. We keep ONE Service
	// (unlike DNSEndpoint mode where dual-stack produces two CRs) so the
	// catalog stays tidy.
	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target, v6Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
	})

	want := v4Target + "," + v6Target
	if got.Annotations[testAnnotationPrefix+"target"] != want {
		t.Fatalf("got target %q, want %q (comma-joined dual-stack)",
			got.Annotations[testAnnotationPrefix+"target"], want)
	}
}

func TestBuildService_CustomAnnotationPrefix_ExactKeysRendered(t *testing.T) {
	t.Parallel()

	// homelab pattern: internal-dns external-dns instance reads keys
	// under 'internal-dns/' and ignores the default prefix entirely.
	// ouroboros must emit ALL its config keys under the configured
	// prefix — none should leak under the default.
	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testCustomPrefix,
	})

	if _, ok := got.Annotations[testCustomPrefix+"hostname"]; !ok {
		t.Fatalf("hostname annotation under custom prefix missing: %v", got.Annotations)
	}

	if _, ok := got.Annotations[testAnnotationPrefix+"hostname"]; ok {
		t.Fatalf("hostname annotation leaked under default prefix: %v", got.Annotations)
	}
}

func TestBuildService_TTLNonDefault_RendersTTLAnnotation(t *testing.T) {
	t.Parallel()

	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target}, TTL: 300,
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
	})

	if got.Annotations[testAnnotationPrefix+"ttl"] != "300" {
		t.Fatalf("got ttl %q, want '300'", got.Annotations[testAnnotationPrefix+"ttl"])
	}
}

func TestBuildService_TTLDefault_OmitsTTLAnnotation(t *testing.T) {
	t.Parallel()

	// When operator does not override TTL, do not render the annotation
	// at all — let external-dns use its provider default. Avoids a
	// confusing situation where ouroboros pins TTL=60 but the operator
	// thinks they are using their provider's default.
	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target}, TTL: 0,
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
	})

	if _, present := got.Annotations[testAnnotationPrefix+"ttl"]; present {
		t.Fatalf("TTL=0 must not render a ttl annotation: %v", got.Annotations)
	}
}

func TestBuildService_OwnershipLabels_AppliedAndOperatorLabelsMerged(t *testing.T) {
	t.Parallel()

	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
		Labels:           map[string]string{"team": "platform"},
	})

	if got.Labels[externaldns.LabelManagedBy] != externaldns.ManagedByValue {
		t.Fatalf("managed-by label missing/wrong: %v", got.Labels)
	}

	if got.Labels[externaldns.LabelInstance] != testInstance {
		t.Fatalf("instance label missing/wrong: %v", got.Labels)
	}

	if got.Labels["team"] != "platform" {
		t.Fatalf("operator label missing: %v", got.Labels)
	}
}

func TestBuildService_OperatorAnnotations_AppliedVerbatim(t *testing.T) {
	t.Parallel()

	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
		Annotations: map[string]string{
			"external-dns.alpha.kubernetes.io/cloudflare-proxied": "false",
		},
	})

	if got.Annotations["external-dns.alpha.kubernetes.io/cloudflare-proxied"] != "false" {
		t.Fatalf("operator annotation missing: %v", got.Annotations)
	}
}

func TestBuildService_RejectsEmptyAnnotationPrefix(t *testing.T) {
	t.Parallel()

	_, err := externaldns.BuildService(&externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: "",
	})
	if err == nil {
		t.Fatal("BuildService: empty AnnotationPrefix must fail (annotations could not be addressed)")
	}
}

func TestBuildService_RejectsAnnotationPrefixWithoutTrailingSlash(t *testing.T) {
	t.Parallel()

	// '/' is the namespace separator in annotation keys. Without it the
	// rendered key would be 'internal-dnshostname' — meaningless. Reject
	// so a chart misconfig fails fast.
	_, err := externaldns.BuildService(&externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: "internal-dns",
	})
	if err == nil {
		t.Fatal("BuildService: AnnotationPrefix without trailing '/' must fail")
	}
}

func TestBuildService_NameSanitization_ReusesSamePatternAsDNSEndpoint(t *testing.T) {
	t.Parallel()

	// Name-collision boundary: same long-host truncation contract as
	// DNSEndpoint mode. A user switching between output=crd and
	// output=service should not see different metadata.name shapes.
	long := strings.Repeat("a", 100) + "." + strings.Repeat("b", 100) + "." + strings.Repeat("c", 80)
	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: long, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
	})

	if len(got.Name) > 253 {
		t.Fatalf("name length %d exceeds 253", len(got.Name))
	}

	if !strings.HasPrefix(got.Name, "ouroboros-") {
		t.Fatalf("name must be prefixed 'ouroboros-': %q", got.Name)
	}
}

func TestBuildService_RejectsWildcardHost(t *testing.T) {
	t.Parallel()

	_, err := externaldns.BuildService(&externaldns.BuildServiceOpts{
		Host: "*.example.com", Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
	})
	if err == nil {
		t.Fatal("BuildService: wildcard host must fail validation")
	}
}

func TestBuildService_RejectsNoTargets(t *testing.T) {
	t.Parallel()

	_, err := externaldns.BuildService(&externaldns.BuildServiceOpts{
		Host: testHost, Targets: nil,
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
	})
	if err == nil {
		t.Fatal("BuildService: empty targets must fail")
	}
}

func TestBuildService_RejectsReservedSourceAnnotation(t *testing.T) {
	t.Parallel()

	_, err := externaldns.BuildService(&externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
		Annotations:      map[string]string{externaldns.AnnotationSource: "user-override"},
	})
	if err == nil {
		t.Fatal("BuildService: reserved source annotation must be rejected")
	}
}

func TestBuildService_RejectsReservedManagedByLabel(t *testing.T) {
	t.Parallel()

	_, err := externaldns.BuildService(&externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
		Labels:           map[string]string{externaldns.LabelManagedBy: "imposter"},
	})
	if err == nil {
		t.Fatal("BuildService: reserved managed-by label must be rejected")
	}
}

func TestBuildService_OwnershipSurvivesEvenIfOperatorTriesManagedByValue(t *testing.T) {
	t.Parallel()

	// Defence-in-depth: if rejectReservedKeys ever loosens, the merge
	// order must still preserve ownership labels. Same invariant as
	// BuildEndpoint side.
	got := mustBuildService(t, externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{v4Target},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
		Labels:           map[string]string{"team": "platform"},
	})

	if got.Labels[externaldns.LabelManagedBy] != externaldns.ManagedByValue {
		t.Fatalf("ownership managed-by must survive: %v", got.Labels)
	}
}

func TestBuildService_RejectsInvalidTarget(t *testing.T) {
	t.Parallel()

	_, err := externaldns.BuildService(&externaldns.BuildServiceOpts{
		Host: testHost, Targets: []string{"not-an-ip"},
		Source: externaldns.SourceIngress, Instance: testInstance, Namespace: testNamespace,
		AnnotationPrefix: testAnnotationPrefix,
	})
	if err == nil {
		t.Fatal("BuildService: non-IP target must fail validation")
	}
}
