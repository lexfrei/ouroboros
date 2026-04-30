// Package externaldns produces externaldns.k8s.io/v1alpha1.DNSEndpoint
// objects for ouroboros's external-dns mode. It does not import
// sigs.k8s.io/external-dns to avoid pulling the entire DNS-controller dep
// tree; the published CRD shape is reproduced as a minimal typed struct and
// converted to unstructured.Unstructured via runtime.DefaultUnstructuredConverter.
package externaldns

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net"
	"sort"
	"strings"

	"github.com/cockroachdb/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CRD constants — published, stable contract from kubernetes-sigs/external-dns.
const (
	APIVersion = "externaldns.k8s.io/v1alpha1"
	Kind       = "DNSEndpoint"

	// k8sNameMax is the upper bound on metadata.name length for any
	// Kubernetes resource (RFC 1123 subdomain).
	k8sNameMax = 253

	// nameHashLen is the number of hex chars of SHA-256 hash appended when
	// a host is too long to fit in metadata.name. 8 chars (32 bits) makes
	// accidental collisions about 1-in-4-billion across a single namespace.
	nameHashLen = 8

	defaultRecordTTL int64 = 60
)

// Label and annotation keys ouroboros sets on every DNSEndpoint it owns.
const (
	LabelManagedBy   = "app.kubernetes.io/managed-by"
	LabelInstance    = "ouroboros.lexfrei.tech/instance"
	AnnotationSource = "ouroboros.lexfrei.tech/source"
)

// GVR is the GroupVersionResource for DNSEndpoint, used by the dynamic client.
//
//nolint:gochecknoglobals // GVR is an immutable identifier, not mutable state.
var GVR = schema.GroupVersionResource{
	Group:    "externaldns.k8s.io",
	Version:  "v1alpha1",
	Resource: "dnsendpoints",
}

// Source identifies which Kubernetes object surfaced the hostname; recorded
// as an annotation on the DNSEndpoint for operator-side debugging.
type Source string

// Recognised sources. Hostnames flow from these into the reconciler. The
// per-source values are kept for future per-host source tracking; the
// current ReconcileFunc interface aggregates hosts and uses
// SourceController.
const (
	SourceIngress         Source = "ingress"
	SourceGatewayListener Source = "gateway-listener"
	SourceHTTPRoute       Source = "httproute"
	SourceController      Source = "controller"
)

// BuildOpts is the input to Build. All fields except TTL and Annotations are
// required; an empty TTL falls back to defaultRecordTTL.
type BuildOpts struct {
	Host        string
	Targets     []string
	TTL         int64
	Source      Source
	Instance    string
	Namespace   string
	Annotations map[string]string
}

// Endpoint is the typed projection of one DNSEndpoint object. The reconciler
// builds these and serialises them via ToUnstructured before calling SSA.
type Endpoint struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	DNSName     string
	RecordType  string
	Targets     []string
	RecordTTL   int64
}

// Build splits opts.Targets by IP family and returns one Endpoint per family.
// Mixed dual-stack inputs produce two Endpoints with distinct names; a single
// family produces one. Validation errors (empty host, wildcard, non-IP
// target, reserved annotation collision) are returned without partial output.
func Build(opts *BuildOpts) ([]Endpoint, error) {
	host, validateErr := validateHost(opts.Host)
	if validateErr != nil {
		return nil, validateErr
	}

	if len(opts.Targets) == 0 {
		return nil, errors.New("Build: at least one target is required")
	}

	if _, ok := opts.Annotations[AnnotationSource]; ok {
		return nil, errors.Errorf("Build: annotation key %q is reserved by ouroboros — pick a different key", AnnotationSource)
	}

	split, splitErr := splitByFamily(opts.Targets)
	if splitErr != nil {
		return nil, splitErr
	}

	v4Targets, v6Targets := split.V4, split.V6

	ttl := opts.TTL
	if ttl == 0 {
		ttl = defaultRecordTTL
	}

	switch {
	case len(v4Targets) > 0 && len(v6Targets) > 0:
		return []Endpoint{
			endpointFor(host, "A", v4Targets, "-v4", ttl, opts),
			endpointFor(host, "AAAA", v6Targets, "-v6", ttl, opts),
		}, nil
	case len(v6Targets) > 0:
		return []Endpoint{endpointFor(host, "AAAA", v6Targets, "", ttl, opts)}, nil
	default:
		return []Endpoint{endpointFor(host, "A", v4Targets, "", ttl, opts)}, nil
	}
}

func validateHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))

	if host == "" {
		return "", errors.New("Build: host must not be empty")
	}

	if strings.ContainsAny(host, "*?") {
		return "", errors.Errorf("Build: wildcard host %q is not supported as a DNSEndpoint name", raw)
	}

	return host, nil
}

// targetSplit is the per-family bucket result of splitByFamily. A struct
// dodges nonamedreturns vs gocritic.unnamedResult, which are at odds when
// two same-type slices flow out of one function.
type targetSplit struct {
	V4 []string
	V6 []string
}

func splitByFamily(targets []string) (targetSplit, error) {
	var out targetSplit

	for _, target := range targets {
		parsed := net.ParseIP(target)
		if parsed == nil {
			return targetSplit{}, errors.Errorf("Build: target %q is not a valid IP literal", target)
		}

		if parsed.To4() != nil {
			out.V4 = append(out.V4, target)
		} else {
			out.V6 = append(out.V6, target)
		}
	}

	return out, nil
}

func endpointFor(host, recordType string, targets []string, suffix string, ttl int64, opts *BuildOpts) Endpoint {
	annotations := map[string]string{AnnotationSource: string(opts.Source)}
	maps.Copy(annotations, opts.Annotations)

	labels := map[string]string{
		LabelManagedBy: "ouroboros",
		LabelInstance:  opts.Instance,
	}

	return Endpoint{
		Name:        buildName(host, suffix),
		Namespace:   opts.Namespace,
		Labels:      labels,
		Annotations: annotations,
		DNSName:     host,
		RecordType:  recordType,
		Targets:     targets,
		RecordTTL:   ttl,
	}
}

// buildName composes a DNS-1123-safe metadata.name from host. Dots and
// underscores become dashes; the result is prefixed with "ouroboros-". When
// the prefixed name would exceed 253 chars the tail is replaced with a
// SHA-256-derived hash so distinct hosts that share a 244-char prefix do not
// collide.
func buildName(host, suffix string) string {
	const prefix = "ouroboros-"

	sanitised := strings.NewReplacer(".", "-", "_", "-").Replace(host)
	candidate := prefix + sanitised + suffix

	if len(candidate) <= k8sNameMax {
		return candidate
	}

	digest := sha256.Sum256([]byte(host + suffix))
	hash := hex.EncodeToString(digest[:])[:nameHashLen]

	tail := "-" + hash + suffix
	keep := k8sNameMax - len(prefix) - len(tail)

	return prefix + sanitised[:keep] + tail
}

// ToUnstructured renders the Endpoint as the unstructured.Unstructured shape
// the dynamic client expects for SSA.
func (endpoint *Endpoint) ToUnstructured() (*unstructured.Unstructured, error) {
	targets := make([]any, len(endpoint.Targets))
	for index, target := range endpoint.Targets {
		targets[index] = target
	}

	endpointSpec := map[string]any{
		"dnsName":    endpoint.DNSName,
		"recordType": endpoint.RecordType,
		"recordTTL":  endpoint.RecordTTL,
		"targets":    targets,
	}

	obj := map[string]any{
		"apiVersion": APIVersion,
		"kind":       Kind,
		"metadata": map[string]any{
			"name":        endpoint.Name,
			"namespace":   endpoint.Namespace,
			"labels":      sortedAnyCopy(endpoint.Labels),
			"annotations": sortedAnyCopy(endpoint.Annotations),
		},
		"spec": map[string]any{
			"endpoints": []any{endpointSpec},
		},
	}

	uns := &unstructured.Unstructured{Object: obj}

	// Round-trip through DefaultUnstructuredConverter to validate the shape
	// is coercible to the typed CRD without losing fields. Protects against
	// accidental schema drift in this file.
	var typed map[string]any

	convErr := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, &typed)
	if convErr != nil {
		return nil, errors.Wrap(convErr, "DNSEndpoint round-trip")
	}

	uns.SetGroupVersionKind(schema.GroupVersionKind{Group: "externaldns.k8s.io", Version: "v1alpha1", Kind: Kind})

	return uns, nil
}

// sortedAnyCopy returns a stable map[string]any copy of m suitable for
// embedding in an unstructured.Unstructured.Object payload. The standard
// runtime.DeepCopyJSON path that backs *Unstructured.DeepCopy panics on
// map[string]string — it only accepts map[string]any — so callers that
// build Object maps manually must funnel string-keyed maps through this
// converter.
func sortedAnyCopy(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}

	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	out := make(map[string]any, len(m))
	for _, key := range keys {
		out[key] = m[key]
	}

	return out
}
