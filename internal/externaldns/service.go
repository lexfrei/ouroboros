package externaldns

import (
	"maps"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildServiceOpts is the input to BuildService. Mirrors BuildOpts but
// adds an explicit AnnotationPrefix because Service-mode external-dns
// instances key off annotation prefix to differentiate which records
// belong to them (split-horizon DNS pattern).
type BuildServiceOpts struct {
	Host             string
	Targets          []string
	TTL              int64
	Source           Source
	Instance         string
	Namespace        string
	Annotations      map[string]string
	Labels           map[string]string
	AnnotationPrefix string
}

// BuildService renders a single headless Service per hostname. The
// Service is an annotation-only carrier — no selector, no ports,
// ClusterIP=None. external-dns with --source=service reads the keys
// under AnnotationPrefix and publishes corresponding DNS records.
//
// Dual-stack targets are joined comma-separated into the 'target'
// annotation; external-dns then creates A and AAAA records from the
// same Service, which keeps the catalog tidy (one Service per host
// regardless of address-family count). Ordering is deterministic —
// V4 addresses first, then V6 — because Update sends the rendered
// annotation value as-is, and a non-deterministic join order would
// churn the Service object on every reconcile (apiserver sees the
// annotation as changed, bumps generation, external-dns re-publishes).
//
// AnnotationPrefix MUST end in '/' — without the namespace separator
// the rendered key would be a meaningless concatenation.
func BuildService(opts *BuildServiceOpts) (*corev1.Service, error) {
	if opts.AnnotationPrefix == "" {
		return nil, errors.New("BuildService: AnnotationPrefix must not be empty")
	}

	if !strings.HasSuffix(opts.AnnotationPrefix, "/") {
		return nil, errors.Errorf(
			"BuildService: AnnotationPrefix %q must end with '/' so the "+
				"rendered key uses the annotation namespace separator",
			opts.AnnotationPrefix)
	}

	host, validateErr := validateHost(opts.Host)
	if validateErr != nil {
		return nil, validateErr
	}

	if len(opts.Targets) == 0 {
		return nil, errors.New("BuildService: at least one target is required")
	}

	reservedErr := rejectReservedKeys(opts.Annotations, opts.Labels)
	if reservedErr != nil {
		return nil, reservedErr
	}

	// Validate every target parses as an IP. We do not split by family
	// here (unlike DNSEndpoint mode) because external-dns infers record
	// type per-target from the IP literal in the comma-joined list.
	split, splitErr := splitByFamily(opts.Targets)
	if splitErr != nil {
		return nil, splitErr
	}

	allTargets := append([]string{}, split.V4...)
	allTargets = append(allTargets, split.V6...)

	annotations := buildServiceAnnotations(host, allTargets, opts)
	labels := buildServiceLabels(opts)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        buildName(host, ""),
			Namespace:   opts.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
		},
	}, nil
}

// internalServiceAnnotations is the count of annotations BuildService
// always writes itself on top of operator-supplied annotations:
// AnnotationSource, hostname, target, plus ttl when non-default.
const internalServiceAnnotations = 4

func buildServiceAnnotations(host string, targets []string, opts *BuildServiceOpts) map[string]string {
	annotations := make(map[string]string, len(opts.Annotations)+internalServiceAnnotations)
	maps.Copy(annotations, opts.Annotations)

	// ouroboros-internal annotations always win on collision via merge
	// order — defence in depth on top of rejectReservedKeys.
	annotations[AnnotationSource] = string(opts.Source)
	annotations[opts.AnnotationPrefix+"hostname"] = host
	annotations[opts.AnnotationPrefix+"target"] = strings.Join(targets, ",")

	if opts.TTL > 0 {
		annotations[opts.AnnotationPrefix+"ttl"] = strconv.FormatInt(opts.TTL, 10)
	}

	return annotations
}

func buildServiceLabels(opts *BuildServiceOpts) map[string]string {
	labels := make(map[string]string, len(opts.Labels)+2)
	maps.Copy(labels, opts.Labels)

	labels[LabelManagedBy] = ManagedByValue
	labels[LabelInstance] = opts.Instance

	return labels
}
