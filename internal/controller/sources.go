// Package controller orchestrates Ingress and Gateway-API informers,
// extracting hostnames into a sorted, deduplicated set that downstream
// reconcilers (CoreDNS, /etc/hosts) write into the cluster.
package controller

import (
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ExtractHostnames flattens hostnames from the given Ingresses, Gateways, and
// HTTPRoutes into a single lowercased, deduplicated, sorted slice. Wildcard
// patterns ("*.example.com") and empty strings are dropped because CoreDNS
// "rewrite name" (and /etc/hosts) cannot express them. Nil entries are
// tolerated.
func ExtractHostnames(
	ingresses []*networkingv1.Ingress,
	gateways []*gatewayv1.Gateway,
	routes []*gatewayv1.HTTPRoute,
) []string {
	seen := make(map[string]struct{})

	collectIngressHosts(seen, ingresses)
	collectGatewayHosts(seen, gateways)
	collectRouteHosts(seen, routes)

	out := make([]string, 0, len(seen))
	for host := range seen {
		out = append(out, host)
	}

	sort.Strings(out)

	return out
}

func collectIngressHosts(seen map[string]struct{}, ingresses []*networkingv1.Ingress) {
	// Ingress: by design we look only at spec.tls[].hosts and ignore
	// spec.rules[].host. Matches upstream compumike/hairpin-proxy behaviour.
	// Note: Gateway and HTTPRoute extractors are NOT TLS-filtered — see the
	// per-function comments below. README "Coverage caveat" explains the
	// asymmetry to operators.
	for _, ingress := range ingresses {
		if ingress == nil {
			continue
		}

		for _, tls := range ingress.Spec.TLS {
			for _, host := range tls.Hosts {
				addHost(seen, host)
			}
		}
	}
}

func collectGatewayHosts(seen map[string]struct{}, gateways []*gatewayv1.Gateway) {
	// Gateway: every listener.hostname is collected regardless of protocol.
	// Unlike Ingress, we do not filter by TLS — Gateway listeners commonly
	// pair plain HTTP with HTTPS (e.g. for redirect listeners) and operators
	// expect both to be hairpinned. README documents this as the
	// "Gateway-API hostnames are NOT TLS-filtered" caveat.
	for _, gateway := range gateways {
		if gateway == nil {
			continue
		}

		for _, listener := range gateway.Spec.Listeners {
			if listener.Hostname == nil {
				continue
			}

			addHost(seen, string(*listener.Hostname))
		}
	}
}

func collectRouteHosts(seen map[string]struct{}, routes []*gatewayv1.HTTPRoute) {
	// HTTPRoute: hostnames collected regardless of which Gateway listener
	// (TLS or HTTP) the route is attached to. Same rationale as Gateway.
	for _, route := range routes {
		if route == nil {
			continue
		}

		for _, host := range route.Spec.Hostnames {
			addHost(seen, string(host))
		}
	}
}

func addHost(seen map[string]struct{}, raw string) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		return
	}

	if strings.ContainsAny(host, "*?") {
		return
	}

	seen[host] = struct{}{}
}
