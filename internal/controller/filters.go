package controller

import (
	networkingv1 "k8s.io/api/networking/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// FilterIngresses returns only Ingresses whose spec.ingressClassName matches
// className. An empty className disables the filter — every non-nil Ingress
// passes through, preserving the v0.1/v0.2 behaviour for clusters with a
// single ingress-controller.
//
// When a filter IS set, Ingresses without an explicit ingressClassName are
// dropped: they are ambiguous, and silently hairpinning them via the wrong
// ingress-controller is exactly the failure mode operators with multiple
// controllers want this filter to prevent (upstream issue #17).
func FilterIngresses(ingresses []*networkingv1.Ingress, className string) []*networkingv1.Ingress {
	if className == "" {
		return passThroughIngresses(ingresses)
	}

	out := make([]*networkingv1.Ingress, 0, len(ingresses))

	for _, ing := range ingresses {
		if ing == nil || ing.Spec.IngressClassName == nil {
			continue
		}

		if *ing.Spec.IngressClassName == className {
			out = append(out, ing)
		}
	}

	return out
}

func passThroughIngresses(ingresses []*networkingv1.Ingress) []*networkingv1.Ingress {
	out := make([]*networkingv1.Ingress, 0, len(ingresses))

	for _, ing := range ingresses {
		if ing == nil {
			continue
		}

		out = append(out, ing)
	}

	return out
}

// FilterGateways returns only Gateways whose spec.gatewayClassName matches
// className. Empty className disables the filter (all non-nil Gateways pass
// through). Unlike Ingress, Gateway requires gatewayClassName as a
// non-optional field — there is no "ambiguous Gateway" case to drop.
func FilterGateways(gateways []*gatewayv1.Gateway, className string) []*gatewayv1.Gateway {
	out := make([]*gatewayv1.Gateway, 0, len(gateways))

	for _, gateway := range gateways {
		if gateway == nil {
			continue
		}

		if className != "" && string(gateway.Spec.GatewayClassName) != className {
			continue
		}

		out = append(out, gateway)
	}

	return out
}

// FilterHTTPRoutes returns only HTTPRoutes attached (via parentRefs) to one
// of the supplied surviving Gateways. A nil survivors slice disables the
// filter (every non-nil route passes through). HTTPRoutes have no class of
// their own — they inherit it through the Gateway they target, so the
// filter follows the parentRefs.
//
// ParentRef.Namespace is optional; the Gateway-API spec defaults it to the
// route's own namespace, and the filter applies that default before matching.
// A multi-parent route is kept whenever AT LEAST ONE parentRef points at a
// surviving Gateway: the hostname still reaches the proxy that way, so
// hairpin is still required.
func FilterHTTPRoutes(routes []*gatewayv1.HTTPRoute, survivors []*gatewayv1.Gateway) []*gatewayv1.HTTPRoute {
	if survivors == nil {
		return passThroughRoutes(routes)
	}

	keys := make(map[gatewayKey]struct{}, len(survivors))
	for _, gateway := range survivors {
		if gateway == nil {
			continue
		}

		keys[gatewayKey{Namespace: gateway.Namespace, Name: gateway.Name}] = struct{}{}
	}

	out := make([]*gatewayv1.HTTPRoute, 0, len(routes))

	for _, route := range routes {
		if route == nil {
			continue
		}

		if routeAttachedToAny(route, keys) {
			out = append(out, route)
		}
	}

	return out
}

func passThroughRoutes(routes []*gatewayv1.HTTPRoute) []*gatewayv1.HTTPRoute {
	out := make([]*gatewayv1.HTTPRoute, 0, len(routes))

	for _, route := range routes {
		if route == nil {
			continue
		}

		out = append(out, route)
	}

	return out
}

type gatewayKey struct {
	Namespace string
	Name      string
}

func routeAttachedToAny(route *gatewayv1.HTTPRoute, survivors map[gatewayKey]struct{}) bool {
	for index := range route.Spec.ParentRefs {
		ref := &route.Spec.ParentRefs[index]

		ns := route.Namespace
		if ref.Namespace != nil {
			ns = string(*ref.Namespace)
		}

		if _, ok := survivors[gatewayKey{Namespace: ns, Name: string(ref.Name)}]; ok {
			return true
		}
	}

	return false
}
