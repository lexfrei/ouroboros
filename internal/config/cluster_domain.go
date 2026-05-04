package config

import (
	"bufio"
	"os"
	"strings"
)

// DefaultClusterDomain is the conventional Kubernetes cluster DNS domain.
// kubelet's --cluster-domain flag defaults to this and so does almost every
// distribution; cozystack tenants and a handful of bespoke clusters override
// it (e.g. "cozy.local"). Used as the last-resort fallback when auto-detect
// from /etc/resolv.conf yields nothing.
const DefaultClusterDomain = "cluster.local"

// resolvConfPath is the canonical location of the resolver config inside a
// pod. Constant in production; tests pass an explicit path argument so this
// stays unswapped at runtime.
const resolvConfPath = "/etc/resolv.conf"

// DetectClusterDomain returns the cluster DNS domain by parsing the
// resolver search list inside /etc/resolv.conf (or the supplied fixture).
// kubelet injects `search <ns>.svc.<domain> svc.<domain> <domain>` into
// every pod's resolv.conf at admission time; the "<domain>" entry is the
// cluster domain, anchored by the "<x>.svc.<domain>" siblings on the same
// line. The picker selects the candidate that the most siblings end with
// — that is the cluster domain by construction. Returns DefaultClusterDomain
// when no `search` line yields a confident pick (file missing, file empty,
// nameserver-only resolv.conf, ambiguous single-entry search, or the
// BSD-era `domain` directive that kubelet never emits).
//
// path == "" reads the canonical /etc/resolv.conf; tests pass a fixture path.
func DetectClusterDomain(path string) string {
	if path == "" {
		path = resolvConfPath
	}

	file, err := os.Open(path)
	if err != nil {
		return DefaultClusterDomain
	}

	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !isSearchLine(line) {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		domain := pickClusterDomain(fields[1:])
		if domain == "" {
			continue
		}

		return domain
	}

	return DefaultClusterDomain
}

// isSearchLine reports whether the trimmed resolv.conf line is a `search`
// directive. Older `domain <name>` form is intentionally ignored — kubelet
// never emits it; honoring it would let a hand-edited resolv.conf with both
// directives mislead the parser.
func isSearchLine(line string) bool {
	return strings.HasPrefix(line, "search ") || strings.HasPrefix(line, "search\t")
}

// ClusterDomainMismatch reports whether the configured ProxyFQDN's suffix
// disagrees with the resolved cluster-domain. It is a non-fatal sanity
// check the controller logs at startup so an operator on a non-default
// cluster (cozystack tenants with cozy.local, federations with
// k8s.example.com) can spot a chart that still bakes
// `*.svc.cluster.local.` into --proxy-fqdn while the cluster's DNS
// serves a different suffix.
//
// Returns false when either side is empty (no signal to compare against)
// and when ProxyFQDN ends with `.svc.<clusterDomain>.` (the canonical
// in-cluster Service FQDN). The trailing dot on ProxyFQDN is enforced
// elsewhere by Validate(); this function tolerates either form on the
// cluster-domain input (callers may pass `cluster.local` or
// `cluster.local.` — both compare equal here).
func ClusterDomainMismatch(clusterDomain, proxyFQDN string) bool {
	if clusterDomain == "" || proxyFQDN == "" {
		return false
	}

	cd := strings.TrimSuffix(clusterDomain, ".")
	wantSuffix := ".svc." + cd + "."

	return !strings.HasSuffix(proxyFQDN, wantSuffix)
}

// pickClusterDomain selects the cluster-domain entry from a kubelet-style
// search list. The cluster-domain candidate is any non-svc entry; the
// chosen one is the candidate that the most other entries are
// subdomain-suffixed by. Returns "" when no candidate scores at least 1
// (single-entry search, no svc siblings) so the caller falls back to the
// default rather than guessing on ambiguous input.
func pickClusterDomain(searches []string) string {
	candidates := collectCandidates(searches)

	bestScore := 0
	best := ""

	for _, candidate := range candidates {
		score := scoreCandidate(candidate, searches)
		if score > bestScore || (score == bestScore && best != "" && len(candidate) < len(best)) {
			bestScore = score
			best = candidate
		}
	}

	if bestScore < 1 {
		return ""
	}

	return best
}

// collectCandidates returns the trimmed-of-trailing-dot non-svc entries
// from a search list. svc.X and *.svc.X entries are excluded — they
// anchor against the cluster-domain entry, they are not the entry itself.
func collectCandidates(searches []string) []string {
	var candidates []string

	for _, entry := range searches {
		entry = strings.TrimSuffix(entry, ".")
		if entry == "" {
			continue
		}

		if strings.HasPrefix(entry, "svc.") || strings.Contains(entry, ".svc.") {
			continue
		}

		candidates = append(candidates, entry)
	}

	return candidates
}

// scoreCandidate counts how many search entries are kubelet-style
// anchors of the candidate cluster-domain. The two anchor shapes
// kubelet emits are `svc.<candidate>` (exactly) and
// `<ns>.svc.<candidate>` (any namespace). A parent zone of the
// cluster domain that an operator may have added via
// dnsConfig.searches (corp DNS that already resolves the parent —
// the canonical case is `search ... example.org` alongside
// `k8s.example.org` siblings) is intentionally NOT counted, because
// it has neither `.svc.<candidate>` nor `svc.<candidate>` shape.
// Trailing dots on entries are normalised first so `cluster.local.`
// and `cluster.local` are treated the same.
func scoreCandidate(candidate string, searches []string) int {
	score := 0
	svcExact := "svc." + candidate
	svcSuffix := ".svc." + candidate

	for _, entry := range searches {
		entry = strings.TrimSuffix(entry, ".")
		if entry == candidate {
			continue
		}

		if entry == svcExact || strings.HasSuffix(entry, svcSuffix) {
			score++
		}
	}

	return score
}
