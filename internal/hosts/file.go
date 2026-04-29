// Package hosts mutates a hosts(5) file so the in-cluster proxy receives
// traffic for ingress hostnames on nodes that bypass CoreDNS (kubelet, the
// container runtime, etc.).
package hosts

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cockroachdb/errors"
)

const (
	// BlockBegin is the marker line opening the ouroboros-managed block.
	BlockBegin = "# === BEGIN ouroboros (do not edit by hand) ==="

	// BlockEnd is the marker line closing the ouroboros-managed block.
	BlockEnd = "# === END ouroboros ==="

	defaultMode = os.FileMode(0o644)
)

// Apply rewrites a hosts(5) file body so that the ouroboros-managed block
// contains exactly one "<ip> <host>" line per host, sorted and deduplicated.
// Hosts are lowercased; empty entries and wildcards are dropped. When the
// resulting host set is empty, an existing block is removed.
//
//nolint:gocritic // public API: (string, bool, error) is the natural shape
func Apply(content, addr string, names []string) (string, bool, error) {
	if addr == "" {
		return "", false, errors.New("empty IP")
	}

	if net.ParseIP(addr) == nil {
		return "", false, errors.Errorf("invalid IP %q", addr)
	}

	cleaned := normalizeHosts(names)
	desired := buildBlock(cleaned, addr)

	lines := strings.Split(content, "\n")
	markers := findExistingMarkers(lines)

	var newLines []string

	switch {
	case markers.begin >= 0 && markers.end >= 0:
		newLines = append(newLines, lines[:markers.begin]...)
		newLines = append(newLines, desired...)
		newLines = append(newLines, lines[markers.end+1:]...)
	case len(desired) == 0:
		return content, false, nil
	default:
		newLines = appendBlockBeforeTrailingBlank(lines, desired)
	}

	result := strings.Join(newLines, "\n")

	return result, result != content, nil
}

// WriteAtomic writes data to path via a temp file in the same directory and
// os.Rename, preserving the existing file's mode (defaulting to 0644 when
// the destination does not yet exist).
func WriteAtomic(path string, data []byte) error {
	mode, modeErr := destinationMode(path)
	if modeErr != nil {
		return modeErr
	}

	tmpPath, writeErr := writeTempFile(filepath.Dir(path), data, mode)
	if writeErr != nil {
		return writeErr
	}

	renameErr := os.Rename(tmpPath, path) //nolint:gosec // path is operator-supplied via flag
	if renameErr != nil {
		_ = os.Remove(tmpPath) //nolint:gosec // tmpPath is from os.CreateTemp in the same dir

		return errors.Wrapf(renameErr, "rename %s to %s", tmpPath, path)
	}

	return nil
}

func destinationMode(path string) (os.FileMode, error) {
	info, statErr := os.Stat(path)
	if statErr == nil {
		return info.Mode().Perm(), nil
	}

	if errors.Is(statErr, os.ErrNotExist) {
		return defaultMode, nil
	}

	return 0, errors.Wrapf(statErr, "stat %s", path)
}

func writeTempFile(dir string, data []byte, mode os.FileMode) (string, error) {
	tmp, createErr := os.CreateTemp(dir, ".ouroboros-hosts-*")
	if createErr != nil {
		return "", errors.Wrapf(createErr, "create temp file in %s", dir)
	}

	tmpPath := tmp.Name()

	_, writeErr := tmp.Write(data)
	if writeErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return "", errors.Wrap(writeErr, "write temp")
	}

	syncErr := tmp.Sync()
	if syncErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return "", errors.Wrap(syncErr, "sync temp")
	}

	closeErr := tmp.Close()
	if closeErr != nil {
		_ = os.Remove(tmpPath)

		return "", errors.Wrap(closeErr, "close temp")
	}

	chmodErr := os.Chmod(tmpPath, mode)
	if chmodErr != nil {
		_ = os.Remove(tmpPath)

		return "", errors.Wrap(chmodErr, "chmod temp")
	}

	return tmpPath, nil
}

func normalizeHosts(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))

	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}

		if strings.ContainsAny(name, "*?") {
			continue
		}

		_, dup := seen[name]
		if dup {
			continue
		}

		seen[name] = struct{}{}
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

type markerBounds struct {
	begin int
	end   int
}

func findExistingMarkers(lines []string) markerBounds {
	out := markerBounds{begin: -1, end: -1}

	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case BlockBegin:
			out.begin = i
		case BlockEnd:
			out.end = i
		}
	}

	return out
}

func buildBlock(names []string, addr string) []string {
	if len(names) == 0 {
		return nil
	}

	out := make([]string, 0, len(names)+2)
	out = append(out, BlockBegin)

	for _, name := range names {
		out = append(out, addr+" "+name)
	}

	out = append(out, BlockEnd)

	return out
}

func appendBlockBeforeTrailingBlank(lines, block []string) []string {
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		out := make([]string, 0, len(lines)+len(block))
		out = append(out, lines[:len(lines)-1]...)
		out = append(out, block...)
		out = append(out, "")

		return out
	}

	out := make([]string, 0, len(lines)+len(block))
	out = append(out, lines...)
	out = append(out, block...)

	return out
}
