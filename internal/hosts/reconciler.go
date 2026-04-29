package hosts

import (
	"context"
	"os"
	"path/filepath"

	"github.com/cockroachdb/errors"
)

// expectedBasename is the only filename Reconciler will write to. We refuse
// any other path so that an operator who misconfigures --etc-hosts to
// /host/etc/shadow (or /host/etc/kubernetes/admin.conf, or anything under
// /etc that the DaemonSet's hostPath mount happens to expose) cannot turn
// the controller into an arbitrary-file overwriter.
const expectedBasename = "hosts"

// Reconciler synchronises an /etc/hosts-shaped file with a desired hostname
// set, pointing every name at the supplied proxy IP.
type Reconciler struct {
	Path    string
	ProxyIP string
}

// Validate refuses any Path whose basename is not literally "hosts".
func (r *Reconciler) Validate() error {
	if r.Path == "" {
		return errors.New("hosts.Reconciler: Path is empty")
	}

	if filepath.Base(r.Path) != expectedBasename {
		return errors.Errorf(
			"hosts.Reconciler: refuse to operate on %q (basename must be %q to prevent arbitrary-file overwrite)",
			r.Path, expectedBasename,
		)
	}

	return nil
}

// Reconcile reads the file, applies the ouroboros block, and atomically
// writes back when content changed. ctx is observed at the read/write
// boundaries; long-running mutations do not block on it.
func (r *Reconciler) Reconcile(ctx context.Context, names []string) error {
	validateErr := r.Validate()
	if validateErr != nil {
		return validateErr
	}

	ctxErr := ctx.Err()
	if ctxErr != nil {
		return errors.Wrap(ctxErr, "context canceled before reconcile")
	}

	current, readErr := os.ReadFile(r.Path)
	if readErr != nil {
		return errors.Wrapf(readErr, "read %s", r.Path)
	}

	updated, changed, applyErr := Apply(string(current), r.ProxyIP, names)
	if applyErr != nil {
		return errors.Wrap(applyErr, "apply hosts mutation")
	}

	if !changed {
		return nil
	}

	writeErr := WriteAtomic(r.Path, []byte(updated))
	if writeErr != nil {
		return errors.Wrapf(writeErr, "write %s", r.Path)
	}

	return nil
}
