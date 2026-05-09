package controller_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corefake "k8s.io/client-go/kubernetes/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"

	"github.com/lexfrei/ouroboros/internal/controller"
)

const (
	syncTimeout   = 3 * time.Second
	exampleHost   = "foo.example.com"
	exampleGWHost = "gw.example.com"

	// Shared test fixtures across all *_test.go files in this package.
	// Hoisted so goconst stays quiet without rewriting each call site.
	testNamespaceDefault    = "default"
	testGatewayName         = "gw"
	testListenerHTTPS       = "https"
	testHostMatchingExample = "matching.example.com"
	testGatewayProxy        = "gw-proxy"
	testPlainListener       = "plain"
	testObjectName          = "test"
)

var errSyntheticReconcile = errors.New("synthetic reconcile failure")

type recordingReconciler struct {
	calls     atomic.Int32
	lastHosts atomic.Value
	failNext  atomic.Int32
}

func (r *recordingReconciler) Reconcile(_ context.Context, hosts []string) error {
	r.calls.Add(1)

	cloned := append([]string(nil), hosts...)
	r.lastHosts.Store(cloned)

	if r.failNext.Add(-1) >= 0 {
		return errSyntheticReconcile
	}

	return nil
}

func (r *recordingReconciler) Hosts() []string {
	v := r.lastHosts.Load()
	if v == nil {
		return nil
	}

	return v.([]string)
}

func waitFor(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()

	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("condition not met within %s", deadline)
}

func TestController_ReconcilesAfterIngressAdded(t *testing.T) {
	t.Parallel()

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: testObjectName, Namespace: testNamespaceDefault},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{exampleHost}}},
		},
	}

	coreClient := corefake.NewSimpleClientset(ingress)
	gwClient := gatewayfake.NewSimpleClientset() //nolint:staticcheck // NewClientset has REST mapping issue for Gateway in v1.5.1

	rec := &recordingReconciler{}

	ctrl := controller.New(&controller.Options{
		Core:         coreClient,
		Gateway:      gwClient,
		EnableGW:     false,
		Reconciler:   rec.Reconcile,
		ResyncPeriod: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- ctrl.Run(ctx) }()

	waitFor(t, syncTimeout, func() bool {
		hosts := rec.Hosts()

		return len(hosts) == 1 && hosts[0] == exampleHost
	})
}

func TestController_RetriesAfterReconcileFailure(t *testing.T) {
	t.Parallel()

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: testObjectName, Namespace: testNamespaceDefault},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{exampleHost}}},
		},
	}

	coreClient := corefake.NewSimpleClientset(ingress)
	gwClient := gatewayfake.NewSimpleClientset() //nolint:staticcheck // NewClientset has REST mapping issue for Gateway in v1.5.1

	rec := &recordingReconciler{}
	rec.failNext.Store(1)

	ctrl := controller.New(&controller.Options{
		Core:         coreClient,
		Gateway:      gwClient,
		EnableGW:     false,
		Reconciler:   rec.Reconcile,
		ResyncPeriod: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() { _ = ctrl.Run(ctx) }()

	waitFor(t, syncTimeout, func() bool { return rec.calls.Load() >= 2 })
}

func TestController_PicksUpGatewayAPIWhenEnabled(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: testGatewayName, Namespace: testNamespaceDefault},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(testObjectName),
			Listeners: []gatewayv1.Listener{
				{
					Name:     testListenerHTTPS,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					Hostname: gatewayHostnamePtr(exampleGWHost),
				},
			},
		},
	}

	coreClient := corefake.NewSimpleClientset()
	gwClient := gatewayfake.NewSimpleClientset() //nolint:staticcheck // NewClientset has REST mapping issue for Gateway in v1.5.1

	rec := &recordingReconciler{}

	ctrl := controller.New(&controller.Options{
		Core:         coreClient,
		Gateway:      gwClient,
		EnableGW:     true,
		Reconciler:   rec.Reconcile,
		ResyncPeriod: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() { _ = ctrl.Run(ctx) }()

	// gateway-api fake.NewSimpleClientset(obj) does not seed its tracker with
	// pre-existing objects, so we Create after the controller has started.
	_, createErr := gwClient.GatewayV1().Gateways(testNamespaceDefault).Create(ctx, gateway, metav1.CreateOptions{})
	if createErr != nil {
		t.Fatalf("create gateway: %v", createErr)
	}

	waitFor(t, syncTimeout, func() bool {
		hosts := rec.Hosts()

		return len(hosts) == 1 && hosts[0] == exampleGWHost
	})
}

func TestController_IgnoresGatewayAPIWhenDisabled(t *testing.T) {
	t.Parallel()

	coreClient := corefake.NewSimpleClientset()
	gwClient := gatewayfake.NewSimpleClientset() //nolint:staticcheck // NewClientset has REST mapping issue for Gateway in v1.5.1

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: testGatewayName, Namespace: testNamespaceDefault},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(testObjectName),
			Listeners: []gatewayv1.Listener{
				{
					Name:     testListenerHTTPS,
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					Hostname: gatewayHostnamePtr(exampleGWHost),
				},
			},
		},
	}

	_, createErr := gwClient.GatewayV1().Gateways(testNamespaceDefault).Create(t.Context(), gateway, metav1.CreateOptions{})
	if createErr != nil {
		t.Fatalf("create gateway: %v", createErr)
	}

	rec := &recordingReconciler{}

	ctrl := controller.New(&controller.Options{
		Core:         coreClient,
		Gateway:      gwClient,
		EnableGW:     false,
		Reconciler:   rec.Reconcile,
		ResyncPeriod: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() { _ = ctrl.Run(ctx) }()

	// initial reconcile fires once at startup with no Ingress and no Gateway
	waitFor(t, syncTimeout, func() bool { return rec.calls.Load() >= 1 })

	if hosts := rec.Hosts(); len(hosts) != 0 {
		t.Errorf("expected no hosts when Gateway API is disabled, got %v", hosts)
	}
}

func TestController_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	coreClient := corefake.NewSimpleClientset()
	gwClient := gatewayfake.NewSimpleClientset() //nolint:staticcheck // NewClientset has REST mapping issue for Gateway in v1.5.1

	ctrl := controller.New(&controller.Options{
		Core:         coreClient,
		Gateway:      gwClient,
		EnableGW:     false,
		Reconciler:   func(_ context.Context, _ []string) error { return nil },
		ResyncPeriod: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() { done <- ctrl.Run(ctx) }()

	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(syncTimeout):
		t.Fatal("Run did not return after context cancel")
	}
}

func gatewayHostnamePtr(host string) *gatewayv1.Hostname {
	h := gatewayv1.Hostname(host)

	return &h
}

// TestController_AppliesIngressClassFilter pins the wiring of the
// IngressClass Option through the reconcile pipeline. A swap bug like
// FilterGateways(gateways, c.opts.IngressClass) would compile and pass
// the unit-level FilterIngresses tests, but this end-to-end check would
// catch it: the matching Ingress's hostname surfaces, the non-matching
// one does not.
func TestController_AppliesIngressClassFilter(t *testing.T) {
	t.Parallel()

	matchingClass := classNginxProxy
	skippedClass := "nginx-plain"

	matching := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: testNamespaceDefault},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &matchingClass,
			TLS:              []networkingv1.IngressTLS{{Hosts: []string{testHostMatchingExample}}},
		},
	}
	skipped := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "skipped", Namespace: testNamespaceDefault},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &skippedClass,
			TLS:              []networkingv1.IngressTLS{{Hosts: []string{"skipped.example.com"}}},
		},
	}

	coreClient := corefake.NewSimpleClientset(matching, skipped)
	gwClient := gatewayfake.NewSimpleClientset() //nolint:staticcheck // NewClientset has REST mapping issue for Gateway in v1.5.1

	rec := &recordingReconciler{}

	ctrl := controller.New(&controller.Options{
		Core:         coreClient,
		Gateway:      gwClient,
		EnableGW:     false,
		Reconciler:   rec.Reconcile,
		ResyncPeriod: time.Hour,
		IngressClass: matchingClass,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() { _ = ctrl.Run(ctx) }()

	waitFor(t, syncTimeout, func() bool {
		hosts := rec.Hosts()

		return len(hosts) == 1 && hosts[0] == testHostMatchingExample
	})
}

func TestDefaultResyncPeriod(t *testing.T) {
	t.Parallel()

	// Periodic resync is the controller's only recovery path when an Ingress
	// watch silently dies (kamaji-managed apiserver via konnectivity is one
	// reproducer where the initial watch establishes 200 OK but no events
	// flow afterwards). The default sets the worst-case event-to-reconcile
	// latency for those broken-watch setups; the chart and any out-of-tree
	// caller relying on the constant pin its expected upper bound here.
	const want = 30 * time.Second

	if controller.DefaultResyncPeriod != want {
		t.Fatalf("DefaultResyncPeriod: got %v, want %v", controller.DefaultResyncPeriod, want)
	}
}

func TestNew_NilOptionsDoesNotPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("New(nil) must not panic, got: %v", r)
		}
	}()

	ctrl := controller.New(nil)
	if ctrl == nil {
		t.Fatal("New(nil) returned nil controller")
	}
}

func TestNew_DoesNotMutateInputOptions(t *testing.T) {
	t.Parallel()

	// Callers may build a single Options struct in shared init code and
	// pass its pointer to multiple New() calls. If New defaults
	// ResyncPeriod by writing back through the pointer, the second call
	// inherits the first call's defaulted value and the contract becomes
	// stateful. Pin the immutability invariant.
	opts := controller.Options{}

	_ = controller.New(&opts)

	if opts.ResyncPeriod != 0 {
		t.Fatalf("New mutated opts.ResyncPeriod: got %v, want 0", opts.ResyncPeriod)
	}

	if opts.Logger != nil {
		t.Fatalf("New populated opts.Logger: got %v, want nil", opts.Logger)
	}
}
