package controller_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/ouroboros/internal/controller"
)

func ingressWithClass(name, className string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{name + ".example.com"}}},
		},
	}

	if className != "" {
		ing.Spec.IngressClassName = &className
	}

	return ing
}

func gatewayWithClass(name, gatewayClass string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClass),
		},
	}
}

func httpRouteAttachedTo(name, parentNamespace, parentName string) *gatewayv1.HTTPRoute {
	parentNS := gatewayv1.Namespace(parentNamespace)
	parentObjName := gatewayv1.ObjectName(parentName)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Namespace: &parentNS, Name: parentObjName},
				},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(name + ".example.com")},
		},
	}
}

func TestFilterIngresses_EmptyClassName_PassesAllThrough(t *testing.T) {
	t.Parallel()

	in := []*networkingv1.Ingress{
		ingressWithClass("a", "nginx"),
		ingressWithClass("b", "traefik"),
		ingressWithClass("c", ""),
	}

	got := controller.FilterIngresses(in, "")
	if len(got) != 3 {
		t.Fatalf("empty filter must keep all, got %d/3", len(got))
	}
}

func TestFilterIngresses_FilterMatchesByClass(t *testing.T) {
	t.Parallel()

	in := []*networkingv1.Ingress{
		ingressWithClass("a", "nginx-proxy"),
		ingressWithClass("b", "nginx-plain"),
		ingressWithClass("c", "nginx-proxy"),
	}

	got := controller.FilterIngresses(in, "nginx-proxy")
	if len(got) != 2 {
		t.Fatalf("got %d/2 matching ingresses", len(got))
	}

	for _, ing := range got {
		if *ing.Spec.IngressClassName != "nginx-proxy" {
			t.Fatalf("got non-matching ingress %s with class %s", ing.Name, *ing.Spec.IngressClassName)
		}
	}
}

func TestFilterIngresses_DropsIngressWithoutClassWhenFilterSet(t *testing.T) {
	t.Parallel()

	// Without a class, an Ingress is ambiguous — the same hostname routed
	// through two ingress-controllers (one PROXY-protocol, one not) would
	// race for the rewrite. Drop unambiguous-only.
	in := []*networkingv1.Ingress{
		ingressWithClass("classed", "nginx-proxy"),
		ingressWithClass("legacy", ""),
	}

	got := controller.FilterIngresses(in, "nginx-proxy")
	if len(got) != 1 || got[0].Name != "classed" {
		t.Fatalf("legacy ingress with no class must be dropped under explicit filter; got %v", got)
	}
}

func TestFilterIngresses_HandlesNilEntries(t *testing.T) {
	t.Parallel()

	in := []*networkingv1.Ingress{nil, ingressWithClass("a", "nginx-proxy"), nil}

	got := controller.FilterIngresses(in, "nginx-proxy")
	if len(got) != 1 {
		t.Fatalf("expected 1 valid ingress kept, got %d", len(got))
	}
}

func TestFilterGateways_EmptyClassName_PassesAllThrough(t *testing.T) {
	t.Parallel()

	in := []*gatewayv1.Gateway{
		gatewayWithClass("a", "istio"),
		gatewayWithClass("b", "envoy"),
	}

	got := controller.FilterGateways(in, "")
	if len(got) != 2 {
		t.Fatalf("empty filter must keep all, got %d/2", len(got))
	}
}

func TestFilterGateways_FilterMatchesByClass(t *testing.T) {
	t.Parallel()

	in := []*gatewayv1.Gateway{
		gatewayWithClass("a", "traefik-proxy"),
		gatewayWithClass("b", "traefik-plain"),
	}

	got := controller.FilterGateways(in, "traefik-proxy")
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("expected only Gateway 'a' to match; got %v", got)
	}
}

func TestFilterGateways_HandlesNilEntries(t *testing.T) {
	t.Parallel()

	in := []*gatewayv1.Gateway{nil, gatewayWithClass("a", "envoy"), nil}

	got := controller.FilterGateways(in, "envoy")
	if len(got) != 1 {
		t.Fatalf("expected 1 kept, got %d", len(got))
	}
}

func TestFilterHTTPRoutes_NoGatewayFilter_PassesAllThrough(t *testing.T) {
	t.Parallel()

	in := []*gatewayv1.HTTPRoute{
		httpRouteAttachedTo("a", "default", "gw-a"),
		httpRouteAttachedTo("b", "default", "gw-b"),
	}

	got := controller.FilterHTTPRoutes(in, nil)
	if len(got) != 2 {
		t.Fatalf("nil gateway filter must keep all routes, got %d", len(got))
	}
}

func TestFilterHTTPRoutes_OnlyKeepsRoutesAttachedToSurvivingGateways(t *testing.T) {
	t.Parallel()

	// Gateway 'gw-proxy' survives the class filter; 'gw-plain' does not.
	// Routes attached to gw-plain must be dropped to avoid hairpinning a
	// hostname whose serving Gateway never gets PROXY-protocol traffic.
	survived := []*gatewayv1.Gateway{gatewayWithClass("gw-proxy", "envoy-proxy")}

	in := []*gatewayv1.HTTPRoute{
		httpRouteAttachedTo("good", "default", "gw-proxy"),
		httpRouteAttachedTo("bad", "default", "gw-plain"),
	}

	got := controller.FilterHTTPRoutes(in, survived)
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("expected only 'good' route to survive; got %v", got)
	}
}

func TestFilterHTTPRoutes_KeepsRouteIfAnyParentRefMatches(t *testing.T) {
	t.Parallel()

	// A multi-attached route (parentRefs to both surviving and dropped
	// Gateway) must be kept — its hostname reaches the proxy via the
	// surviving Gateway, so hairpin is still required.
	survived := []*gatewayv1.Gateway{gatewayWithClass("gw-proxy", "envoy-proxy")}

	parentNS := gatewayv1.Namespace("default")
	multi := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Namespace: &parentNS, Name: "gw-plain"},
					{Namespace: &parentNS, Name: "gw-proxy"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"multi.example.com"},
		},
	}

	got := controller.FilterHTTPRoutes([]*gatewayv1.HTTPRoute{multi}, survived)
	if len(got) != 1 {
		t.Fatalf("multi-parent route attached to a surviving Gateway must be kept; got %d", len(got))
	}
}

func TestFilterHTTPRoutes_DefaultsParentRefNamespaceToRouteNamespace(t *testing.T) {
	t.Parallel()

	// ParentRef.Namespace is optional; per Gateway-API spec it defaults to
	// the route's own namespace. Filter must apply that default before
	// matching against surviving Gateways' namespaces.
	survived := []*gatewayv1.Gateway{gatewayWithClass("gw-proxy", "envoy-proxy")}

	implicit := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "implicit", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "gw-proxy"}, // no namespace → defaults to "default"
				},
			},
			Hostnames: []gatewayv1.Hostname{"implicit.example.com"},
		},
	}

	got := controller.FilterHTTPRoutes([]*gatewayv1.HTTPRoute{implicit}, survived)
	if len(got) != 1 {
		t.Fatalf("implicit-namespace parentRef must match same-namespace Gateway; got %d", len(got))
	}
}

func TestFilterHTTPRoutes_HandlesNilEntries(t *testing.T) {
	t.Parallel()

	survived := []*gatewayv1.Gateway{gatewayWithClass("gw-proxy", "envoy-proxy")}
	in := []*gatewayv1.HTTPRoute{nil, httpRouteAttachedTo("good", "default", "gw-proxy"), nil}

	got := controller.FilterHTTPRoutes(in, survived)
	if len(got) != 1 {
		t.Fatalf("expected 1 kept, got %d", len(got))
	}
}
