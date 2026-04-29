package hosts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lexfrei/ouroboros/internal/hosts"
)

const (
	proxyIP     = "10.96.1.2"
	beginMarker = "# === BEGIN ouroboros (do not edit by hand) ==="
	endMarker   = "# === END ouroboros ==="
	baseHosts   = `127.0.0.1 localhost
::1 localhost ip6-localhost ip6-loopback
10.0.0.1 host.example.com
`
)

func mustApply(t *testing.T, content, ip string, names []string) (string, bool) {
	t.Helper()

	out, changed, err := hosts.Apply(content, ip, names)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	return out, changed
}

func TestApply_RejectsEmptyIP(t *testing.T) {
	t.Parallel()

	_, _, err := hosts.Apply(baseHosts, "", []string{"foo.example.com"})
	if err == nil {
		t.Fatal("Apply with empty IP must return error")
	}
}

func TestApply_RejectsInvalidIP(t *testing.T) {
	t.Parallel()

	_, _, err := hosts.Apply(baseHosts, "not-an-ip", []string{"foo.example.com"})
	if err == nil {
		t.Fatal("Apply with invalid IP must return error")
	}
}

func TestApply_AddsBlockWhenAbsent(t *testing.T) {
	t.Parallel()

	out, changed := mustApply(t, baseHosts, proxyIP, []string{"foo.example.com", "bar.example.com"})
	if !changed {
		t.Error("changed = false, want true")
	}

	if !strings.Contains(out, beginMarker) || !strings.Contains(out, endMarker) {
		t.Errorf("markers missing:\n%s", out)
	}

	if !strings.Contains(out, proxyIP+" foo.example.com") {
		t.Errorf("missing entry for foo.example.com:\n%s", out)
	}

	if !strings.Contains(out, proxyIP+" bar.example.com") {
		t.Errorf("missing entry for bar.example.com:\n%s", out)
	}
}

func TestApply_PreservesExistingEntries(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, baseHosts, proxyIP, []string{"foo.example.com"})

	mustKeep := []string{
		"127.0.0.1 localhost",
		"::1 localhost ip6-localhost ip6-loopback",
		"10.0.0.1 host.example.com",
	}

	for _, line := range mustKeep {
		if !strings.Contains(out, line) {
			t.Errorf("existing line %q removed:\n%s", line, out)
		}
	}
}

func TestApply_IsIdempotent(t *testing.T) {
	t.Parallel()

	first, _ := mustApply(t, baseHosts, proxyIP, []string{"foo.example.com"})

	second, changed := mustApply(t, first, proxyIP, []string{"foo.example.com"})
	if changed {
		t.Error("changed = true on identical Apply")
	}

	if first != second {
		t.Errorf("output differs on second Apply:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestApply_DeduplicatesAndSorts(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, baseHosts, proxyIP,
		[]string{"foo.example.com", "FOO.example.com", "bar.example.com", "foo.example.com"})

	count := strings.Count(out, proxyIP+" foo.example.com")
	if count != 1 {
		t.Errorf("foo.example.com appears %d times, want 1", count)
	}

	fooIdx := strings.Index(out, "foo.example.com")
	barIdx := strings.Index(out, "bar.example.com")

	if fooIdx < 0 || barIdx < 0 {
		t.Fatalf("expected entries missing:\n%s", out)
	}

	if barIdx > fooIdx {
		t.Errorf("entries not sorted: bar should precede foo (bar=%d foo=%d)", barIdx, fooIdx)
	}
}

func TestApply_FiltersWildcardsAndBlanks(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, baseHosts, proxyIP,
		[]string{"foo.example.com", "*.wild.example.com", "", "  ", "bar.example.com"})

	if strings.Contains(out, "*.wild.example.com") {
		t.Error("wildcard hostname leaked")
	}

	for _, line := range strings.Split(out, "\n") {
		if line == proxyIP+" " || line == proxyIP {
			t.Errorf("blank entry produced: %q", line)
		}
	}
}

func TestApply_ReplacesExistingBlock(t *testing.T) {
	t.Parallel()

	first, _ := mustApply(t, baseHosts, proxyIP, []string{"old.example.com"})

	second, changed := mustApply(t, first, proxyIP, []string{"new.example.com"})
	if !changed {
		t.Error("changed = false when block content differs")
	}

	if strings.Contains(second, "old.example.com") {
		t.Errorf("old hostname not removed:\n%s", second)
	}

	if !strings.Contains(second, "new.example.com") {
		t.Errorf("new hostname missing:\n%s", second)
	}

	if strings.Count(second, beginMarker) != 1 {
		t.Errorf("BEGIN marker count = %d, want 1", strings.Count(second, beginMarker))
	}
}

func TestApply_RemovesBlockOnEmptyHosts(t *testing.T) {
	t.Parallel()

	withBlock, _ := mustApply(t, baseHosts, proxyIP, []string{"foo.example.com"})

	cleaned, changed := mustApply(t, withBlock, proxyIP, nil)
	if !changed {
		t.Error("changed = false when removing block")
	}

	if strings.Contains(cleaned, beginMarker) {
		t.Errorf("BEGIN marker still present:\n%s", cleaned)
	}

	if strings.Contains(cleaned, "foo.example.com") {
		t.Errorf("hostname still present:\n%s", cleaned)
	}
}

func TestApply_NoopWhenEmptyHostsAndNoBlock(t *testing.T) {
	t.Parallel()

	out, changed := mustApply(t, baseHosts, proxyIP, nil)
	if changed {
		t.Error("changed = true on no-op")
	}

	if out != baseHosts {
		t.Errorf("output mutated:\n%s", out)
	}
}

func TestApply_AcceptsIPv6(t *testing.T) {
	t.Parallel()

	out, _ := mustApply(t, baseHosts, "fd00::1", []string{"foo.example.com"})
	if !strings.Contains(out, "fd00::1 foo.example.com") {
		t.Errorf("IPv6 entry missing:\n%s", out)
	}
}

func TestWriteAtomic_PreservesMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")

	const wantMode os.FileMode = 0o644

	err := os.WriteFile(path, []byte(baseHosts), wantMode)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	chmodErr := os.Chmod(path, wantMode)
	if chmodErr != nil {
		t.Fatalf("chmod: %v", chmodErr)
	}

	writeErr := hosts.WriteAtomic(path, []byte("new content\n"))
	if writeErr != nil {
		t.Fatalf("WriteAtomic: %v", writeErr)
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat: %v", statErr)
	}

	if info.Mode().Perm() != wantMode {
		t.Errorf("mode = %v, want %v", info.Mode().Perm(), wantMode)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}

	if string(got) != "new content\n" {
		t.Errorf("content = %q, want %q", got, "new content\n")
	}
}

func TestWriteAtomic_FailsForMissingDirectory(t *testing.T) {
	t.Parallel()

	bogus := filepath.Join(t.TempDir(), "no-such-dir", "hosts")

	err := hosts.WriteAtomic(bogus, []byte("x"))
	if err == nil {
		t.Fatal("WriteAtomic into a missing directory must return error")
	}
}

func TestWriteAtomic_DoesNotLeaveTempFileOnSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")

	err := os.WriteFile(path, []byte("seed\n"), 0o644)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	writeErr := hosts.WriteAtomic(path, []byte("after\n"))
	if writeErr != nil {
		t.Fatalf("WriteAtomic: %v", writeErr)
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("readdir: %v", readErr)
	}

	for _, entry := range entries {
		if entry.Name() != filepath.Base(path) {
			t.Errorf("stray file left behind: %s", entry.Name())
		}
	}
}
