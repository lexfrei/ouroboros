package externaldns

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// OwnershipSelector returns the label-selector string the reconciler passes
// to a list call to enumerate exactly the DNSEndpoints it owns. Two ouroboros
// releases in the same namespace are scoped by the instance label so they do
// not delete each other's objects during stale cleanup.
func OwnershipSelector(instance string) string {
	return fmt.Sprintf("%s=ouroboros,%s=%s", LabelManagedBy, LabelInstance, instance)
}

// OwnershipSelectorAsMap is the structured form of OwnershipSelector for
// callers that build a metav1.LabelSelector{MatchLabels: ...}.
func OwnershipSelectorAsMap(instance string) map[string]string {
	return map[string]string{
		LabelManagedBy: "ouroboros",
		LabelInstance:  instance,
	}
}

// IsOwnedByOuroboros reports whether obj carries both ouroboros ownership
// labels AND the instance matches. Used during stale cleanup to make sure
// the reconciler never deletes a DNSEndpoint that belongs to a different
// ouroboros release or a different controller entirely.
func IsOwnedByOuroboros(obj *unstructured.Unstructured, instance string) bool {
	if obj == nil {
		return false
	}

	labels := obj.GetLabels()
	if labels == nil {
		return false
	}

	return labels[LabelManagedBy] == "ouroboros" && labels[LabelInstance] == instance
}
