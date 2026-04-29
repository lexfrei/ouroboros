// Package coredns mutates the CoreDNS Corefile so the in-cluster proxy
// receives DNS-rewritten traffic for ingress hostnames.
package coredns

import (
	"regexp"
	"sort"
	"strings"

	"github.com/cockroachdb/errors"
)

const (
	// BlockBegin is the marker line opening the ouroboros-managed block.
	BlockBegin = "# === BEGIN ouroboros (do not edit by hand) ==="

	// BlockEnd is the marker line closing the ouroboros-managed block.
	BlockEnd = "# === END ouroboros ==="

	defaultIndent = "    "
)

// serverBlockOpener matches the opening line of the catch-all CoreDNS server
// block bound to port 53 (e.g. ".:53 {", "  .:53 {"). Server blocks bound
// to other ports or to specific zones (e.g. "ext.example.com:53", ".foo:53")
// are not matched — the regex demands the bare "." root zone.
var serverBlockOpener = regexp.MustCompile(`^\s*\.:53\s*\{\s*$`)

// blockBounds is a half-open [open, close] index pair describing a CoreDNS
// server-block within a slice of lines.
type blockBounds struct {
	open  int
	close int
}

// markerBounds locates the BEGIN/END marker lines of an ouroboros block.
// A negative value signals the marker was not found.
type markerBounds struct {
	begin int
	end   int
}

// Apply rewrites corefile so the ouroboros-managed block contains exactly one
// "rewrite name <host> <target>" line per host, sorted and deduplicated.
// Hosts are lowercased; empty entries and wildcard patterns ("*.example.com")
// are silently dropped.
//
// When the resulting host set is empty, an existing block is removed and any
// non-existent block is left alone. The returned bool is true when corefile
// content actually changed.
//
// Errors:
//   - empty corefile
//   - empty target or target without a trailing dot
//   - corefile has no top-level ".:53" server block
//   - corefile contains an unclosed server block
//
//nolint:gocritic // public API: (string, bool, error) is the natural shape
func Apply(corefile string, hosts []string, target string) (string, bool, error) {
	if corefile == "" {
		return "", false, errors.New("empty Corefile")
	}

	if target == "" {
		return "", false, errors.New("empty target")
	}

	if !strings.HasSuffix(target, ".") {
		return "", false, errors.New("target must be FQDN with trailing dot")
	}

	cleaned := normalizeHosts(hosts)

	lines := strings.Split(corefile, "\n")

	bounds, locErr := locateServerBlock(lines)
	if locErr != nil {
		return "", false, locErr
	}

	markers := findExistingMarkers(lines, bounds)

	indent := detectIndent(lines, bounds)
	desired := buildBlock(cleaned, target, indent)

	var newLines []string

	switch {
	case markers.begin >= 0 && markers.end >= 0:
		newLines = append(newLines, lines[:markers.begin]...)
		newLines = append(newLines, desired...)
		newLines = append(newLines, lines[markers.end+1:]...)
	case len(desired) == 0:
		return corefile, false, nil
	default:
		newLines = append(newLines, lines[:bounds.close]...)
		newLines = append(newLines, desired...)
		newLines = append(newLines, lines[bounds.close:]...)
	}

	result := strings.Join(newLines, "\n")

	return result, result != corefile, nil
}

func normalizeHosts(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	out := make([]string, 0, len(hosts))

	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}

		if strings.ContainsAny(host, "*?") {
			continue
		}

		_, dup := seen[host]
		if dup {
			continue
		}

		seen[host] = struct{}{}
		out = append(out, host)
	}

	sort.Strings(out)

	return out
}

func locateServerBlock(lines []string) (blockBounds, error) {
	for i, line := range lines {
		if !serverBlockOpener.MatchString(line) {
			continue
		}

		depth := 1

		for idx := i + 1; idx < len(lines); idx++ {
			stripped := stripComment(lines[idx])
			depth += strings.Count(stripped, "{")
			depth -= strings.Count(stripped, "}")

			if depth == 0 {
				return blockBounds{open: i, close: idx}, nil
			}
		}

		return blockBounds{}, errors.New("unclosed CoreDNS server block at .:53")
	}

	return blockBounds{}, errors.New("no .:53 server block found")
}

// HasReloadPlugin reports whether corefile mentions the CoreDNS "reload"
// plugin. Without it, CoreDNS does not pick up ConfigMap changes and pods
// must be restarted manually after each ouroboros reconcile. The check is
// a coarse line scan: a line whose first non-whitespace, non-comment token
// is exactly "reload" counts. This catches the canonical "    reload" plugin
// directive and tolerates leading whitespace and trailing arguments.
func HasReloadPlugin(corefile string) bool {
	for line := range strings.SplitSeq(corefile, "\n") {
		stripped := strings.TrimSpace(stripComment(line))
		if stripped == "" {
			continue
		}

		fields := strings.Fields(stripped)
		if len(fields) > 0 && fields[0] == "reload" {
			return true
		}
	}

	return false
}

// stripComment removes everything from the first '#' onward so braces inside
// comments are not counted toward server-block depth.
func stripComment(line string) string {
	before, _, _ := strings.Cut(line, "#")

	return before
}

func findExistingMarkers(lines []string, bounds blockBounds) markerBounds {
	out := markerBounds{begin: -1, end: -1}

	for i := bounds.open + 1; i < bounds.close; i++ {
		switch strings.TrimSpace(lines[i]) {
		case BlockBegin:
			out.begin = i
		case BlockEnd:
			out.end = i
		}
	}

	return out
}

func detectIndent(lines []string, bounds blockBounds) string {
	for i := bounds.close - 1; i > bounds.open; i-- {
		line := lines[i]
		trimmed := strings.TrimLeft(line, " \t")

		if trimmed == "" {
			continue
		}

		return line[:len(line)-len(trimmed)]
	}

	return defaultIndent
}

func buildBlock(hosts []string, target, indent string) []string {
	if len(hosts) == 0 {
		return nil
	}

	out := make([]string, 0, len(hosts)+2)
	out = append(out, indent+BlockBegin)

	for _, host := range hosts {
		out = append(out, indent+"rewrite name "+host+" "+target)
	}

	out = append(out, indent+BlockEnd)

	return out
}
