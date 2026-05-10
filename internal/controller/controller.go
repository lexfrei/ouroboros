package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
)

// DefaultResyncPeriod is the SharedInformerFactory resync period applied when
// Options.ResyncPeriod is zero. Periodic resync is the controller's only
// recovery path for an Ingress watch that silently dies — observed on
// kamaji-managed tenant kube-apiservers reached through konnectivity, where
// the initial watch returns 200 OK but no events ever flow afterwards. The
// default doubles as the worst-case event-to-reconcile latency for those
// broken-watch setups, so it is exported and pinned via TestDefaultResyncPeriod
// to keep the chart's resync default and any out-of-tree caller in lock-step.
const DefaultResyncPeriod = 30 * time.Second

const (
	queueKey  = "reconcile"
	queueName = "ouroboros"
)

// ReconcileFunc applies a desired hostname set somewhere (CoreDNS ConfigMap,
// /etc/hosts file, etc.). It is called from a single worker goroutine, so the
// implementation does not need to be reentrant.
type ReconcileFunc func(ctx context.Context, hosts []string) error

// Options bundles Controller dependencies.
type Options struct {
	Core         kubernetes.Interface
	Gateway      gatewayclient.Interface
	EnableGW     bool
	Reconciler   ReconcileFunc
	ResyncPeriod time.Duration
	Logger       *slog.Logger

	// IngressClass, when non-empty, restricts hostname extraction to
	// Ingresses whose spec.ingressClassName matches it. Ingresses without an
	// explicit class are dropped under the filter — see FilterIngresses for
	// the rationale (hairpin-proxy upstream issue #17: multiple
	// ingress-controllers with different PROXY-protocol settings).
	IngressClass string

	// GatewayClass, when non-empty, restricts hostname extraction to
	// Gateways whose spec.gatewayClassName matches it, and to HTTPRoutes
	// attached (via parentRefs) to one of the surviving Gateways.
	GatewayClass string
}

// Controller watches Ingress resources (and optionally Gateway+HTTPRoute) and
// fans every event into a single rate-limited workqueue key so reconcile
// calls coalesce naturally.
type Controller struct {
	opts Options
	log  *slog.Logger
}

// New builds a Controller. Logger == nil silences output. The supplied
// Options pointer is NOT mutated — defaulting happens on the local copy
// so callers can safely reuse the struct (e.g. in tests). A nil opts
// pointer is treated as an empty Options{} rather than panicking — keeps
// callers safe during early bootstrap where Options may be assembled
// piece-meal.
func New(opts *Options) *Controller {
	var local Options
	if opts != nil {
		local = *opts
	}

	if local.ResyncPeriod == 0 {
		local.ResyncPeriod = DefaultResyncPeriod
	}

	logger := local.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Controller{opts: local, log: logger}
}

// Run starts informers and the reconcile worker. It blocks until ctx is
// canceled or a fatal error occurs. Returns nil on clean shutdown.
func (c *Controller) Run(ctx context.Context) error {
	queue := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: queueName},
	)

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			c.log.Debug("informer event", "type", "add", "obj", describeObj(obj))
			queue.Add(queueKey)
		},
		UpdateFunc: func(_, obj any) {
			c.log.Debug("informer event", "type", "update", "obj", describeObj(obj))
			queue.Add(queueKey)
		},
		DeleteFunc: func(obj any) {
			c.log.Debug("informer event", "type", "delete", "obj", describeObj(obj))
			queue.Add(queueKey)
		},
	}

	coreFactory, state, regErr := c.registerCoreInformer(handler)
	if regErr != nil {
		return regErr
	}

	gatewayFactory, gwErr := c.registerGatewayInformers(handler, &state)
	if gwErr != nil {
		return gwErr
	}

	coreFactory.Start(ctx.Done())

	if gatewayFactory != nil {
		gatewayFactory.Start(ctx.Done())
	}

	syncStart := time.Now()

	syncErr := waitForSync(ctx, coreFactory, gatewayFactory)
	if syncErr != nil {
		return syncErr
	}

	c.log.Info("informer cache synced", "elapsed", time.Since(syncStart))

	queue.Add(queueKey)

	return c.runWorkers(ctx, queue, &state)
}

// describeObj renders a `<go-type> <namespace>/<name>` string for logging
// informer events. Tombstones (cache.DeletedFinalStateUnknown, delivered
// when a watch deletion event was missed during a disconnect) are unwrapped
// so the line still names the object whose deletion the controller reacted
// to instead of just "tombstone of unknown" — that's the exact case where
// keeping identity in the log matters for diagnosis. Falls back to the bare
// type when obj implements neither metav1.Object nor the tombstone shape.
func describeObj(obj any) string {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return "tombstone(" + tomb.Key + ") " + describeObj(tomb.Obj)
	}

	if accessor, ok := obj.(metav1.Object); ok {
		return fmt.Sprintf("%T %s/%s", obj, accessor.GetNamespace(), accessor.GetName())
	}

	return fmt.Sprintf("%T", obj)
}

func (c *Controller) registerCoreInformer(handler cache.ResourceEventHandler) (informers.SharedInformerFactory, listerState, error) {
	factory := informers.NewSharedInformerFactory(c.opts.Core, c.opts.ResyncPeriod)
	ingressInformer := factory.Networking().V1().Ingresses().Informer()

	state := listerState{ingress: factory.Networking().V1().Ingresses().Lister()}

	_, err := ingressInformer.AddEventHandler(handler)
	if err != nil {
		return nil, listerState{}, errors.Wrap(err, "register ingress handler")
	}

	errSet := ingressInformer.SetWatchErrorHandler(c.watchErrorHandler("Ingress"))
	if errSet != nil {
		return nil, listerState{}, errors.Wrap(errSet, "set ingress watch error handler")
	}

	return factory, state, nil
}

// watchErrorHandler returns a cache.WatchErrorHandler that surfaces every
// ListAndWatch failure as a Warn log line tagged with the watched kind.
// Critical for diagnosing setups where a watch silently dies after the
// initial healthy connection (kamaji-managed tenant kube-apiservers reached
// through konnectivity have shown this pattern in cozystack e2e) — the
// default client-go handler logs at varying levels and is easy to miss
// in pod logs scoped to the controller's own slog handler.
func (c *Controller) watchErrorHandler(kind string) cache.WatchErrorHandler {
	return func(_ *cache.Reflector, err error) {
		c.log.Warn("watch error", "kind", kind, "error", err)
	}
}

func (c *Controller) registerGatewayInformers(
	handler cache.ResourceEventHandler,
	state *listerState,
) (gatewayinformers.SharedInformerFactory, error) {
	if !c.opts.EnableGW || c.opts.Gateway == nil {
		return nil, nil //nolint:nilnil // gateway-api disabled is a valid configuration
	}

	factory := gatewayinformers.NewSharedInformerFactory(c.opts.Gateway, c.opts.ResyncPeriod)

	gwInformer := factory.Gateway().V1().Gateways().Informer()
	state.gateway = factory.Gateway().V1().Gateways().Lister()

	_, err := gwInformer.AddEventHandler(handler)
	if err != nil {
		return nil, errors.Wrap(err, "register gateway handler")
	}

	errSetGW := gwInformer.SetWatchErrorHandler(c.watchErrorHandler("Gateway"))
	if errSetGW != nil {
		return nil, errors.Wrap(errSetGW, "set gateway watch error handler")
	}

	rtInformer := factory.Gateway().V1().HTTPRoutes().Informer()
	state.route = factory.Gateway().V1().HTTPRoutes().Lister()

	_, err = rtInformer.AddEventHandler(handler)
	if err != nil {
		return nil, errors.Wrap(err, "register httproute handler")
	}

	errSetRT := rtInformer.SetWatchErrorHandler(c.watchErrorHandler("HTTPRoute"))
	if errSetRT != nil {
		return nil, errors.Wrap(errSetRT, "set httproute watch error handler")
	}

	return factory, nil
}

func (c *Controller) runWorkers(
	ctx context.Context,
	queue workqueue.TypedRateLimitingInterface[string],
	state *listerState,
) error {
	group, gctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		<-gctx.Done()

		queue.ShutDown()

		return nil
	})

	group.Go(func() error {
		c.worker(gctx, queue, state)

		return nil
	})

	groupErr := group.Wait()
	if groupErr != nil && !errors.Is(groupErr, context.Canceled) {
		return errors.Wrap(groupErr, "controller run")
	}

	return nil
}

func (c *Controller) worker(
	ctx context.Context,
	queue workqueue.TypedRateLimitingInterface[string],
	state *listerState,
) {
	for {
		key, shutdown := queue.Get()
		if shutdown {
			return
		}

		err := c.reconcileOnce(ctx, state)
		if err != nil {
			c.log.Error("reconcile failed", "error", err)
			queue.AddRateLimited(key)
		} else {
			queue.Forget(key)
		}

		queue.Done(key)
	}
}

func (c *Controller) reconcileOnce(ctx context.Context, state *listerState) error {
	ingresses, gateways, routes, listErr := listAll(state)
	if listErr != nil {
		return listErr
	}

	c.log.Debug("reconcile starting",
		"ingressesSeen", len(ingresses),
		"gatewaysSeen", len(gateways),
		"routesSeen", len(routes))

	ingresses = FilterIngresses(ingresses, c.opts.IngressClass)
	gateways = FilterGateways(gateways, c.opts.GatewayClass)

	// Always go through FilterHTTPRoutes so the public API has one
	// reachable contract. When no GatewayClass filter is set, pass nil
	// survivors and the function pass-throughs every non-nil route —
	// preserves v0.1/v0.2 behaviour for clusters with a single class
	// without leaving an untested branch in the package.
	var survivors []*gatewayv1.Gateway
	if c.opts.GatewayClass != "" {
		survivors = gateways
	}

	routes = FilterHTTPRoutes(routes, survivors)

	hosts := ExtractHostnames(ingresses, gateways, routes)

	c.log.Debug("reconcile applying", "hosts", len(hosts))

	reconcileErr := c.opts.Reconciler(ctx, hosts)
	if reconcileErr != nil {
		return errors.Wrap(reconcileErr, "apply reconcile")
	}

	return nil
}

// listAll fans out List() across the three lister kinds the controller
// watches and returns the raw, unfiltered slices. Hoisted out of
// reconcileOnce so the latter stays inside funlen's ceiling and the
// per-resource error handling lives in one place rather than three.
func listAll(state *listerState) (
	[]*networkingv1.Ingress,
	[]*gatewayv1.Gateway,
	[]*gatewayv1.HTTPRoute,
	error,
) {
	ingresses, listErr := state.ingress.List(labels.Everything())
	if listErr != nil {
		return nil, nil, nil, errors.Wrap(listErr, "list ingresses")
	}

	var (
		gateways []*gatewayv1.Gateway
		routes   []*gatewayv1.HTTPRoute
	)

	if state.gateway != nil {
		gws, err := state.gateway.List(labels.Everything())
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "list gateways")
		}

		gateways = gws
	}

	if state.route != nil {
		rts, err := state.route.List(labels.Everything())
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "list httproutes")
		}

		routes = rts
	}

	return ingresses, gateways, routes, nil
}

func waitForSync(
	ctx context.Context,
	core informers.SharedInformerFactory,
	gateway gatewayinformers.SharedInformerFactory,
) error {
	coreSynced := core.WaitForCacheSync(ctx.Done())
	for resource, ok := range coreSynced {
		if !ok {
			return errors.Errorf("core informer %s did not sync", resource)
		}
	}

	if gateway == nil {
		return nil
	}

	gwSynced := gateway.WaitForCacheSync(ctx.Done())
	for resource, ok := range gwSynced {
		if !ok {
			return errors.Errorf("gateway informer %s did not sync", resource)
		}
	}

	return nil
}
