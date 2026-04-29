package hosts_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lexfrei/ouroboros/internal/hosts"
)

func TestReconciler_RejectsNonHostsBasename(t *testing.T) {
	t.Parallel()

	cases := []string{
		"/etc/shadow",
		"/etc/kubernetes/admin.conf",
		"/host/etc/passwd",
		"/var/lib/whatever",
		"",
	}

	for _, path := range cases {
		rec := &hosts.Reconciler{Path: path, ProxyIP: proxyIP}

		err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
		if err == nil {
			t.Errorf("Reconcile must refuse Path=%q (basename guard); got nil error", path)
		}
	}
}

func TestReconciler_FailsOnMissingFile(t *testing.T) {
	t.Parallel()

	rec := &hosts.Reconciler{
		Path:    filepath.Join(t.TempDir(), "missing"),
		ProxyIP: proxyIP,
	}

	err := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if err == nil {
		t.Fatal("expected error when file is missing")
	}
}

func TestReconciler_RejectsCanceledContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")

	writeErr := os.WriteFile(path, []byte(baseHosts), 0o644)
	if writeErr != nil {
		t.Fatalf("seed: %v", writeErr)
	}

	rec := &hosts.Reconciler{Path: path, ProxyIP: proxyIP}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := rec.Reconcile(ctx, []string{"foo.example.com"})
	if err == nil {
		t.Fatal("expected error on canceled context")
	}
}

func TestReconciler_AppliesAndIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")

	writeErr := os.WriteFile(path, []byte(baseHosts), 0o644)
	if writeErr != nil {
		t.Fatalf("seed: %v", writeErr)
	}

	rec := &hosts.Reconciler{Path: path, ProxyIP: proxyIP}

	firstErr := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if firstErr != nil {
		t.Fatalf("first Reconcile: %v", firstErr)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}

	if !strings.Contains(string(got), proxyIP+" foo.example.com") {
		t.Errorf("content missing expected entry:\n%s", got)
	}

	statBefore, _ := os.Stat(path)

	secondErr := rec.Reconcile(t.Context(), []string{"foo.example.com"})
	if secondErr != nil {
		t.Fatalf("second Reconcile: %v", secondErr)
	}

	statAfter, _ := os.Stat(path)
	if statAfter.ModTime() != statBefore.ModTime() {
		t.Errorf("file rewritten on idempotent reconcile (mtime changed)")
	}
}
