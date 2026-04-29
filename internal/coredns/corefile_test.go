package coredns_test

import (
	"strings"
	"testing"

	"github.com/lexfrei/ouroboros/internal/coredns"
)

const (
	defaultTarget   = "ouroboros-proxy.ouroboros.svc.cluster.local."
	beginMarker     = "# === BEGIN ouroboros (do not edit by hand) ==="
	endMarker       = "# === END ouroboros ==="
	corefileMinimal = `.:53 {
    kubernetes cluster.local
    forward . /etc/resolv.conf
}
`
	corefileFull = `.:53 {
    errors
    health {
       lameduck 5s
    }
    ready
    kubernetes cluster.local in-addr.arpa ip6.arpa {
       pods insecure
       fallthrough in-addr.arpa ip6.arpa
       ttl 30
    }
    prometheus :9153
    forward . /etc/resolv.conf {
       max_concurrent 1000
    }
    cache 30
    loop
    reload
    loadbalance
}
`
	corefileNo53 = `.:8053 {
    forward . /etc/resolv.conf
}
`
	corefileMultipleServers = `.:53 {
    kubernetes cluster.local
    forward . 8.8.8.8
}

ext.example.com:53 {
    forward . 1.1.1.1
}
`
)

func mustApply(t *testing.T, corefile string, hosts []string, target string) (string, bool) {
	t.Helper()

	out, changed, err := coredns.Apply(corefile, hosts, target)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	return out, changed
}

func countLines(s, sub string) int {
	count := 0

	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			count++
		}
	}

	return count
}

func TestApply_RejectsEmptyCorefile(t *testing.T) {
	t.Parallel()

	_, _, err := coredns.Apply("", []string{"foo.com"}, defaultTarget)
	if err == nil {
		t.Fatal("Apply with empty Corefile must return error")
	}
}

func TestApply_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()

	_, _, err := coredns.Apply(corefileMinimal, []string{"foo.com"}, "")
	if err == nil {
		t.Fatal("Apply with empty target must return error")
	}
}

func TestApply_RejectsTargetWithoutTrailingDot(t *testing.T) {
	t.Parallel()

	_, _, err := coredns.Apply(corefileMinimal, []string{"foo.com"}, "ouroboros-proxy.ouroboros.svc.cluster.local")
	if err == nil {
		t.Fatal("Apply with non-FQDN target must return error")
	}
}

func TestApply_NoServerBlock_ReturnsError(t *testing.T) {
	t.Parallel()

	_, _, err := coredns.Apply(corefileNo53, []string{"foo.com"}, defaultTarget)
	if err == nil {
		t.Fatal("Apply with no .:53 server block must return error")
	}
}

func TestApply_AddsBlockWhenAbsent(t *testing.T) {
	t.Parallel()

	out, changed := mustApply(t, corefileMinimal, []string{"foo.example.com", "bar.example.com"}, defaultTarget)

	if !changed {
		t.Error("changed = false, want true after adding new block")
	}

	if !strings.Contains(out, beginMarker) || !strings.Contains(out, endMarker) {
		t.Errorf("output missing markers:\n%s", out)
	}

	if !strings.Contains(out, "rewrite name foo.example.com "+defaultTarget) {
		t.Errorf("missing rewrite for foo.example.com:\n%s", out)
	}

	if !strings.Contains(out, "rewrite name bar.example.com "+defaultTarget) {
		t.Errorf("missing rewrite for bar.example.com:\n%s", out)
	}
}

func TestApply_PlacesBlockInsideServerBraces(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, corefileFull, []string{"foo.example.com"}, defaultTarget)

	beginIdx := strings.Index(out, beginMarker)
	if beginIdx < 0 {
		t.Fatalf("BEGIN marker missing:\n%s", out)
	}

	closingIdx := strings.LastIndex(out, "}")
	if closingIdx < beginIdx {
		t.Errorf("BEGIN marker is outside the server-block braces (begin=%d closing=%d)", beginIdx, closingIdx)
	}
}

func TestApply_IsIdempotent(t *testing.T) {
	t.Parallel()

	once, _ := mustApply(t, corefileFull, []string{"foo.example.com", "bar.example.com"}, defaultTarget)

	twice, changed := mustApply(t, once, []string{"foo.example.com", "bar.example.com"}, defaultTarget)
	if changed {
		t.Error("changed = true on second Apply with identical inputs, want false")
	}

	if once != twice {
		t.Errorf("output differs on second Apply:\n--- first ---\n%s\n--- second ---\n%s", once, twice)
	}
}

func TestApply_DeduplicatesAndSorts(t *testing.T) {
	t.Parallel()

	first, _ := mustApply(t, corefileMinimal,
		[]string{"foo.example.com", "FOO.example.com", "bar.example.com", "foo.example.com"},
		defaultTarget)

	if got := countLines(first, "rewrite name foo.example.com"); got != 1 {
		t.Errorf("foo.example.com rewrite count = %d, want 1", got)
	}

	fooIdx := strings.Index(first, "rewrite name foo.example.com")
	barIdx := strings.Index(first, "rewrite name bar.example.com")

	if barIdx < 0 || fooIdx < 0 {
		t.Fatalf("missing expected rewrites:\n%s", first)
	}

	if barIdx > fooIdx {
		t.Errorf("rewrites not sorted: bar should come before foo, got bar=%d foo=%d", barIdx, fooIdx)
	}
}

func TestApply_FiltersWildcardsAndBlanks(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, corefileMinimal,
		[]string{"foo.example.com", "*.wild.example.com", "", "  ", "bar.example.com"},
		defaultTarget)

	if strings.Contains(out, "*.wild.example.com") {
		t.Error("wildcard hostname leaked into Corefile")
	}

	if strings.Contains(out, "rewrite name  ") {
		t.Errorf("blank hostname produced an empty rewrite line:\n%s", out)
	}
}

func TestApply_ReplacesExistingBlock(t *testing.T) {
	t.Parallel()

	first, _ := mustApply(t, corefileFull, []string{"old.example.com"}, defaultTarget)

	second, changed := mustApply(t, first, []string{"new.example.com"}, defaultTarget)
	if !changed {
		t.Error("changed = false when block content differs")
	}

	if strings.Contains(second, "rewrite name old.example.com") {
		t.Errorf("old hostname not removed:\n%s", second)
	}

	if !strings.Contains(second, "rewrite name new.example.com") {
		t.Errorf("new hostname not present:\n%s", second)
	}

	if strings.Count(second, beginMarker) != 1 {
		t.Errorf("BEGIN marker count = %d, want 1 (no duplicate block)", strings.Count(second, beginMarker))
	}
}

func TestApply_RemovesBlockOnEmptyHosts(t *testing.T) {
	t.Parallel()

	withBlock, _ := mustApply(t, corefileFull, []string{"foo.example.com", "bar.example.com"}, defaultTarget)

	cleaned, changed := mustApply(t, withBlock, nil, defaultTarget)
	if !changed {
		t.Error("changed = false when removing block, want true")
	}

	if strings.Contains(cleaned, beginMarker) || strings.Contains(cleaned, endMarker) {
		t.Errorf("markers remained after removal:\n%s", cleaned)
	}

	if strings.Contains(cleaned, "rewrite name") {
		t.Errorf("rewrite line remained after removal:\n%s", cleaned)
	}
}

func TestApply_NoChangeWhenEmptyAndNothingToRemove(t *testing.T) {
	t.Parallel()

	out, changed := mustApply(t, corefileMinimal, nil, defaultTarget)
	if changed {
		t.Error("changed = true when there was nothing to remove and nothing to add")
	}

	if out != corefileMinimal {
		t.Errorf("output mutated despite no-op:\n%s", out)
	}
}

func TestApply_PreservesNonHairpinPlugins(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, corefileFull, []string{"foo.example.com"}, defaultTarget)

	mustKeep := []string{
		"errors",
		"lameduck 5s",
		"kubernetes cluster.local in-addr.arpa ip6.arpa",
		"pods insecure",
		"prometheus :9153",
		"forward . /etc/resolv.conf",
		"max_concurrent 1000",
		"cache 30",
		"loop",
		"reload",
		"loadbalance",
	}

	for _, want := range mustKeep {
		if !strings.Contains(out, want) {
			t.Errorf("plugin %q removed from Corefile:\n%s", want, out)
		}
	}
}

func TestApply_TargetsFirstServerBlock(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, corefileMultipleServers, []string{"foo.example.com"}, defaultTarget)

	beginIdx := strings.Index(out, beginMarker)
	if beginIdx < 0 {
		t.Fatalf("BEGIN marker missing:\n%s", out)
	}

	extServerIdx := strings.Index(out, "ext.example.com:53")
	if extServerIdx < 0 {
		t.Fatalf("ext.example.com:53 server-block missing — corefile mangled:\n%s", out)
	}

	if beginIdx > extServerIdx {
		t.Errorf("ouroboros block was placed inside the wrong server (begin=%d ext=%d)", beginIdx, extServerIdx)
	}
}

func TestHasReloadPlugin_DetectsReload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		corefile string
		want     bool
	}{
		{name: "canonical_indented_reload", corefile: corefileFull, want: true},
		{name: "missing_reload", corefile: corefileMinimal, want: false},
		{name: "reload_with_args", corefile: ".:53 {\n    reload 5s\n}\n", want: true},
		{name: "reload_in_comment_does_not_count", corefile: ".:53 {\n    # reload\n}\n", want: false},
		{name: "reload_substring_does_not_count", corefile: ".:53 {\n    reloaded\n}\n", want: false},
		{name: "empty_corefile", corefile: "", want: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := coredns.HasReloadPlugin(tt.corefile)
			if got != tt.want {
				t.Errorf("HasReloadPlugin(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestApply_RejectsNamedZoneAsCatchAll(t *testing.T) {
	t.Parallel()

	// .foo:53 is a *zone-scoped* server block (matches the .foo TLD), not the
	// cluster catch-all. The mutator must not treat it as ".:53".
	corefile := ".foo:53 {\n    forward . /etc/resolv.conf\n}\n"

	_, _, err := coredns.Apply(corefile, []string{"x.example.com"}, defaultTarget)
	if err == nil {
		t.Fatal("Apply on a corefile with only .foo:53 must report no .:53 server block")
	}
}

func TestApply_RejectsUnclosedServerBlock(t *testing.T) {
	t.Parallel()

	broken := ".:53 {\n    kubernetes cluster.local\n"

	_, _, err := coredns.Apply(broken, []string{"foo.example.com"}, defaultTarget)
	if err == nil {
		t.Fatal("Apply with unclosed server block must return error")
	}
}

func TestApply_PreservesTrailingNewline(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, corefileMinimal, []string{"foo.example.com"}, defaultTarget)
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("trailing newline lost:\n%q", out)
	}

	noTrailing := strings.TrimRight(corefileMinimal, "\n")

	out2, _ := mustApply(t, noTrailing, []string{"foo.example.com"}, defaultTarget)
	if strings.HasSuffix(out2, "\n") {
		t.Errorf("trailing newline added unexpectedly:\n%q", out2)
	}
}
