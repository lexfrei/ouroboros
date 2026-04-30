package config

import (
	"os"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
)

// envErrors collects per-variable parse errors so the caller can report all
// invalid OUROBOROS_* values at once instead of failing on the first one.
type envErrors struct {
	errs []error
}

func (e *envErrors) addf(format string, args ...any) {
	e.errs = append(e.errs, errors.Errorf(format, args...))
}

// err returns nil when no errors were collected, otherwise a joined error.
func (e *envErrors) err() error {
	if len(e.errs) == 0 {
		return nil
	}

	return errors.Join(e.errs...)
}

// envString assigns the env value to *dst when the variable is set and
// non-empty.
func envString(name string, dst *string) {
	val := os.Getenv(envPrefix + name)
	if val != "" {
		*dst = val
	}
}

// envInt parses the env value as an int and writes it to *dst on success.
// On invalid input the parse error is recorded; the destination is not
// touched.
func envInt(errs *envErrors, name string, dst *int) {
	val := os.Getenv(envPrefix + name)
	if val == "" {
		return
	}

	parsed, err := strconv.Atoi(val)
	if err != nil {
		errs.addf("%s%s=%q is not a valid integer: %v", envPrefix, name, val, err)

		return
	}

	*dst = parsed
}

// envInt64 parses the env value as an int64 and writes it to *dst on success.
func envInt64(errs *envErrors, name string, dst *int64) {
	val := os.Getenv(envPrefix + name)
	if val == "" {
		return
	}

	const decimalBase = 10

	const sixtyFourBits = 64

	parsed, err := strconv.ParseInt(val, decimalBase, sixtyFourBits)
	if err != nil {
		errs.addf("%s%s=%q is not a valid int64: %v", envPrefix, name, val, err)

		return
	}

	*dst = parsed
}

// envBool parses the env value as a bool and writes it to *dst on success.
func envBool(errs *envErrors, name string, dst *bool) {
	val := os.Getenv(envPrefix + name)
	if val == "" {
		return
	}

	parsed, err := strconv.ParseBool(val)
	if err != nil {
		errs.addf("%s%s=%q is not a valid boolean (use true/false): %v", envPrefix, name, val, err)

		return
	}

	*dst = parsed
}

// envDuration parses the env value as a time.Duration and writes it to *dst
// on success.
func envDuration(errs *envErrors, name string, dst *time.Duration) {
	val := os.Getenv(envPrefix + name)
	if val == "" {
		return
	}

	parsed, err := time.ParseDuration(val)
	if err != nil {
		errs.addf("%s%s=%q is not a valid duration (e.g. 5s, 10m): %v", envPrefix, name, val, err)

		return
	}

	*dst = parsed
}
