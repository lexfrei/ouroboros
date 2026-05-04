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

// TestREADME_ControllerFlagTableContainsEveryFlag pins the
// documentation invariant the README itself stakes on line 266
// ("the table below is the source-of-truth alphabetical reference").
// Every controller flag registered via ParseControllerFlags must
// appear in the README's flag table; otherwise an operator scanning
// the table to find every supported knob silently misses one. The
// failure mode this catches: a new flag lands without a README row,
// the prose section describing the feature mentions the flag in
// passing, and the table goes stale until somebody hits the gap.
func TestREADME_ControllerFlagTableContainsEveryFlag(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(readmePath(t))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	// Scope the search to the `### \`ouroboros controller\`` flag
	// table only. A prose mention of `\`--cluster-domain\`` somewhere
	// else in the README does not fulfill the source-of-truth contract
	// the table itself stakes — operators read the table as a
	// reference, not the prose around it.
	text := string(body)
	tableStart := strings.Index(text, "### `ouroboros controller`")

	if tableStart < 0 {
		t.Fatal("README: cannot find `### \\`ouroboros controller\\`` heading")
	}

	tableEnd := strings.Index(text[tableStart+1:], "### `ouroboros proxy`")
	if tableEnd < 0 {
		t.Fatal("README: cannot find `### \\`ouroboros proxy\\`` heading after the controller table")
	}

	tableSection := text[tableStart : tableStart+1+tableEnd]

	for _, flag := range controllerFlagNames(t) {
		needle := "`--" + flag + "`"
		if !strings.Contains(tableSection, needle) {
			t.Errorf("README controller flag table missing %q "+
				"(the table itself is the documented source of "+
				"truth; every flag from ParseControllerFlags must "+
				"have a row).", needle)
		}
	}
}

// controllerFlagNames extracts every flag string ("name") registered
// via flagSet.<Type>Var(... "name", ...) calls in controller.go. The
// extraction is naive on purpose — a one-line grep equivalent — so a
// future refactor of the flag-registration shape (cobra, urfave/cli,
// etc.) will trip the test rather than silently bypass it.
func controllerFlagNames(t *testing.T) []string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	src := filepath.Join(wd, "..", "config", "controller.go")

	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read controller.go: %v", err)
	}

	// flagSet.StringVar(&cfg.X, "name", ...)  — name is the second
	// quoted argument. flagSet.<Bool|Duration|Int64|String>Var follow
	// the same shape. We collect the second quoted token of each
	// matching line. Skipping flags whose "name" is a help/usage
	// string is automatic: those are the third or later argument in
	// .Var calls, which the line-level scan never sees as the second
	// quoted token.
	var (
		flags []string
		known = map[string]bool{}
	)

	for _, line := range strings.Split(string(body), "\n") {
		if !strings.Contains(line, "flagSet.") || !strings.Contains(line, "Var") {
			continue
		}

		// Collect every `"` position on the line. The second pair
		// (i.e. quotePositions[2:4]) wraps the flag-name argument
		// by construction in this codebase: every flagSet.<T>Var
		// call passes (target, "name", default, "usage"). Skip
		// lines that do not have at least 4 quote positions —
		// those are multi-line registrations whose name lives on
		// the same first line as the call (target+"name"+default
		// fit, "usage" continues on the next line) so two or more
		// `"` tokens are still on this line. Lines with exactly 2
		// quotes (`flagSet.<T>Var(target, "name",`) are also valid.
		var quotePositions []int
		for i, char := range line {
			if char == '"' {
				quotePositions = append(quotePositions, i)
			}
		}

		if len(quotePositions) < 2 {
			continue
		}

		// flag-name lives between the FIRST pair of quotes when
		// the call is split across lines; between the SECOND pair
		// when usage and default are on the same line. Detect
		// which form by checking whether the first quoted token
		// looks like a flag name (lowercase, hyphen-separated,
		// no spaces).
		firstName := line[quotePositions[0]+1 : quotePositions[1]]
		if isFlagName(firstName) {
			if !known[firstName] {
				known[firstName] = true
				flags = append(flags, firstName)
			}

			continue
		}
	}

	if len(flags) == 0 {
		t.Fatal("no controller flags extracted from controller.go — flag-registration shape changed?")
	}

	return flags
}

// isFlagName reports whether s looks like a flag name (lowercase
// alphanumeric + hyphen, at least 2 chars, no spaces). The CLI
// flags in this codebase all match `^[a-z][a-z0-9-]+$`. Usage
// strings (the only other quoted argument the line scanner could
// confuse with a flag name) contain spaces and start with a
// capital letter.
func isFlagName(s string) bool {
	if len(s) < 2 {
		return false
	}

	if s[0] < 'a' || s[0] > 'z' {
		return false
	}

	for _, char := range s {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9':
		case char == '-':
		default:
			return false
		}
	}

	return true
}
