package coredns

import (
	"context"

	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
)

// reconcileWithRetry wraps a reconcile-once closure in the standard
// retry-on-conflict loop and applies the canonical error decoration. The
// modeLabel is included in the wrap so a failure in either coredns or
// coredns-import surface tells the operator which path was running.
func reconcileWithRetry(ctx context.Context, modeLabel string, once func() (bool, error)) (bool, error) {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return false, errors.Wrap(ctxErr, "context canceled before reconcile")
	}

	var changed bool

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		didChange, err := once()
		if err != nil {
			return err
		}

		changed = didChange

		return nil
	})
	if retryErr != nil {
		return false, errors.Wrap(retryErr, "reconcile "+modeLabel)
	}

	return changed, nil
}

// wrapConfigMapUpdateErr decorates a ConfigMap.Update error with the
// resource coordinates and surfaces conflict separately so retry-loop
// callers can distinguish "we'll try again" from terminal failures.
func wrapConfigMapUpdateErr(err error, namespace, name string) error {
	if apierrors.IsConflict(err) {
		return errors.Wrapf(err, "update configmap %s/%s (conflict)", namespace, name)
	}

	return errors.Wrapf(err, "update configmap %s/%s", namespace, name)
}
