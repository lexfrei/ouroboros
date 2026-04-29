package controller_test

import (
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/ouroboros/internal/controller"
)

func ingressTLS(name string, hosts ...string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: hosts}},
		},
	}
}

func gatewayWithHostnames(name string, hostnames ...string) *gatewayv1.Gateway {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}

	for i, host := range hostnames {
		var listenerHostname *gatewayv1.Hostname

		if host != "" {
			h := gatewayv1.Hostname(host)
			listenerHostname = &h
		}

		gateway.Spec.Listeners = append(gateway.Spec.Listeners, gatewayv1.Listener{
			Name:     gatewayv1.SectionName("listener-" + string(rune('a'+i))),
			Port:     gatewayv1.PortNumber(443),
			Protocol: gatewayv1.HTTPSProtocolType,
			Hostname: listenerHostname,
		})
	}

	return gateway
}

func httpRouteWithHostnames(name string, hostnames ...string) *gatewayv1.HTTPRoute {
	hosts := make([]gatewayv1.Hostname, 0, len(hostnames))
	for _, host := range hostnames {
		hosts = append(hosts, gatewayv1.Hostname(host))
	}

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: hosts,
		},
	}
}

func TestExtractHostnames_EmptyInputsReturnEmpty(t *testing.T) {
	t.Parallel()

	got := controller.ExtractHostnames(nil, nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestExtractHostnames_PullsFromIngressTLS(t *testing.T) {
	t.Parallel()

	got := controller.ExtractHostnames(
		[]*networkingv1.Ingress{ingressTLS("a", "foo.example.com", "bar.example.com")},
		nil, nil,
	)

	want := []string{"bar.example.com", "foo.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractHostnames_IgnoresIngressWithoutTLS(t *testing.T) {
	t.Parallel()

	plain := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "default"},
	}

	got := controller.ExtractHostnames([]*networkingv1.Ingress{plain}, nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty result for non-TLS Ingress, got %v", got)
	}
}

func TestExtractHostnames_PullsFromGatewayListener(t *testing.T) {
	t.Parallel()

	got := controller.ExtractHostnames(nil,
		[]*gatewayv1.Gateway{gatewayWithHostnames("gw", "foo.example.com", "")},
		nil)

	want := []string{"foo.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (nil hostname listener should be skipped)", got, want)
	}
}

func TestExtractHostnames_PullsFromHTTPRoute(t *testing.T) {
	t.Parallel()

	got := controller.ExtractHostnames(nil, nil,
		[]*gatewayv1.HTTPRoute{httpRouteWithHostnames("rt", "api.example.com", "edge.example.com")})

	want := []string{"api.example.com", "edge.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractHostnames_DropsWildcardsAndBlanks(t *testing.T) {
	t.Parallel()

	got := controller.ExtractHostnames(
		[]*networkingv1.Ingress{ingressTLS("a", "foo.example.com", "*.wild.example.com", "")},
		nil, nil,
	)

	for _, host := range got {
		if host == "" || host == "*.wild.example.com" {
			t.Errorf("invalid hostname leaked: %q in %v", host, got)
		}
	}

	if len(got) != 1 || got[0] != "foo.example.com" {
		t.Errorf("got %v, want [foo.example.com]", got)
	}
}

func TestExtractHostnames_DeduplicatesAcrossSources(t *testing.T) {
	t.Parallel()

	got := controller.ExtractHostnames(
		[]*networkingv1.Ingress{ingressTLS("a", "Foo.example.com")},
		[]*gatewayv1.Gateway{gatewayWithHostnames("gw", "FOO.example.com")},
		[]*gatewayv1.HTTPRoute{httpRouteWithHostnames("rt", "foo.example.com")},
	)

	want := []string{"foo.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractHostnames_OutputIsSortedAndLowercase(t *testing.T) {
	t.Parallel()

	got := controller.ExtractHostnames(
		[]*networkingv1.Ingress{ingressTLS("a", "Zeta.example.com", "Alpha.example.com", "MIDDLE.example.com")},
		nil, nil,
	)

	want := []string{"alpha.example.com", "middle.example.com", "zeta.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractHostnames_GatewayHostnamesAreNotTLSFiltered(t *testing.T) {
	t.Parallel()

	httpOnly := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "http", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name:     "plain",
					Port:     gatewayv1.PortNumber(80),
					Protocol: gatewayv1.HTTPProtocolType,
					Hostname: gatewayHostnamePtr("plain.example.com"),
				},
			},
		},
	}

	got := controller.ExtractHostnames(nil, []*gatewayv1.Gateway{httpOnly}, nil)
	if len(got) != 1 || got[0] != "plain.example.com" {
		t.Errorf("HTTP-only Gateway hostname must be included (Gateway-API is not TLS-filtered): got %v", got)
	}
}

func TestExtractHostnames_SkipsNilEntries(t *testing.T) {
	t.Parallel()

	got := controller.ExtractHostnames(
		[]*networkingv1.Ingress{nil, ingressTLS("a", "foo.example.com")},
		[]*gatewayv1.Gateway{nil},
		[]*gatewayv1.HTTPRoute{nil},
	)

	want := []string{"foo.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nil entries must not panic; got %v, want %v", got, want)
	}
}
