package controller

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	describeObjTestNS   = "default"
	describeObjTestName = "hairpin-probe"
	describeObjTestKey  = describeObjTestNS + "/" + describeObjTestName
)

// describeObj is package-private. White-box tests pin its branches
// directly so the tombstone-unwrap path (cache.DeletedFinalStateUnknown,
// delivered when a watch deletion event was missed during a disconnect)
// does not regress to a bare `%T` line that loses the deleted object's
// identity — the whole point of the Debug log is to know which object
// the controller reacted to.

func TestDescribeObj_TypedIngress(t *testing.T) {
	t.Parallel()

	obj := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: describeObjTestName, Namespace: describeObjTestNS},
	}

	got := describeObj(obj)

	if !strings.Contains(got, "*v1.Ingress") {
		t.Errorf("describeObj missing concrete type: %q", got)
	}

	if !strings.Contains(got, describeObjTestKey) {
		t.Errorf("describeObj missing namespace/name: %q", got)
	}
}

func TestDescribeObj_TombstoneUnwrapsInner(t *testing.T) {
	t.Parallel()

	// DeletedFinalStateUnknown carries a Key plus the last-known Obj.
	// Operators want both: the Key tells them what the cache knew the
	// object as, and the unwrapped Obj identifies it by namespace/name
	// regardless of whether the apiserver still has the resource.
	inner := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: describeObjTestName, Namespace: describeObjTestNS},
	}

	tomb := cache.DeletedFinalStateUnknown{Key: describeObjTestKey, Obj: inner}

	got := describeObj(tomb)

	if !strings.HasPrefix(got, "tombstone("+describeObjTestKey+")") {
		t.Errorf("describeObj tombstone prefix wrong: %q", got)
	}

	if !strings.Contains(got, "*v1.Ingress") || !strings.Contains(got, describeObjTestKey) {
		t.Errorf("describeObj tombstone did not unwrap inner object: %q", got)
	}
}

func TestDescribeObj_FallbackForOpaqueValue(t *testing.T) {
	t.Parallel()

	// Anything that implements neither metav1.Object nor the tombstone
	// shape (e.g. a malformed event payload) should still produce a
	// readable type-only string instead of panicking.
	got := describeObj(struct{ Foo int }{Foo: 7})

	if !strings.Contains(got, "struct {") {
		t.Errorf("describeObj fallback did not emit type: %q", got)
	}
}
