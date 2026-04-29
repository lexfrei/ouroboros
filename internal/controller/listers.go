package controller

import (
	networkingv1lister "k8s.io/client-go/listers/networking/v1"
	gatewayv1lister "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

// listerState bundles the lister handles a Controller uses during reconcile.
// Lister fields may be nil when Gateway-API is disabled.
type listerState struct {
	ingress networkingv1lister.IngressLister
	gateway gatewayv1lister.GatewayLister
	route   gatewayv1lister.HTTPRouteLister
}
