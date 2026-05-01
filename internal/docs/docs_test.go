// Package docs holds documentation invariants. The tests here pin
// claims in user-facing markdown that previously drifted from code
// (or from each other).
package docs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readmePath resolves the repo-root README.md from the test's CWD
// (internal/docs at runtime). Walking two parents up keeps the
// location stable even if the test gets moved or the test runner
// changes how it sets CWD.
func readmePath(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	return filepath.Join(wd, "..", "..", "README.md")
}

// TestREADME_NoPhantomEmptyHostsTriggers pins the documentation
// invariant flagged in branch review: the empty-hosts mass-prune
// guard is triggered ONLY by config-driven scenarios (deleted
// routes, all-hostnames-wildcarded). 'Stale informer cache on
// startup' is NOT a real trigger because controller.go calls
// WaitForCacheSync before runWorkers — the first reconcile sees a
// fully synced cache. README must not list it as a triggering
// scenario; if it does, doc-vs-code drift is back.
func TestREADME_NoPhantomEmptyHostsTriggers(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(readmePath(t))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	text := string(body)

	for _, banned := range []string{
		"stale informer cache",
		"briefly stale informer",
	} {
		if strings.Contains(text, banned) {
			t.Errorf("README must not list %q as a guard trigger — "+
				"WaitForCacheSync precedes the first reconcile, so this "+
				"scenario isn't reachable. Update README to use a real "+
				"config-driven trigger instead.", banned)
		}
	}
}
