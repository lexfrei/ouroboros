package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
)

const (
	defaultResync = 10 * time.Minute
	queueKey      = "reconcile"
	queueName     = "ouroboros"
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
// so callers can safely reuse the struct (e.g. in tests).
func New(opts *Options) *Controller {
	local := *opts
	if local.ResyncPeriod == 0 {
		local.ResyncPeriod = defaultResync
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
		AddFunc:    func(_ any) { queue.Add(queueKey) },
		UpdateFunc: func(_, _ any) { queue.Add(queueKey) },
		DeleteFunc: func(_ any) { queue.Add(queueKey) },
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

	syncErr := waitForSync(ctx, coreFactory, gatewayFactory)
	if syncErr != nil {
		return syncErr
	}

	queue.Add(queueKey)

	return c.runWorkers(ctx, queue, &state)
}

func (c *Controller) registerCoreInformer(handler cache.ResourceEventHandler) (informers.SharedInformerFactory, listerState, error) {
	factory := informers.NewSharedInformerFactory(c.opts.Core, c.opts.ResyncPeriod)
	ingressInformer := factory.Networking().V1().Ingresses()

	state := listerState{ingress: ingressInformer.Lister()}

	_, err := ingressInformer.Informer().AddEventHandler(handler)
	if err != nil {
		return nil, listerState{}, errors.Wrap(err, "register ingress handler")
	}

	return factory, state, nil
}

func (c *Controller) registerGatewayInformers(
	handler cache.ResourceEventHandler,
	state *listerState,
) (gatewayinformers.SharedInformerFactory, error) {
	if !c.opts.EnableGW || c.opts.Gateway == nil {
		return nil, nil //nolint:nilnil // gateway-api disabled is a valid configuration
	}

	factory := gatewayinformers.NewSharedInformerFactory(c.opts.Gateway, c.opts.ResyncPeriod)

	gwInformer := factory.Gateway().V1().Gateways()
	state.gateway = gwInformer.Lister()

	_, err := gwInformer.Informer().AddEventHandler(handler)
	if err != nil {
		return nil, errors.Wrap(err, "register gateway handler")
	}

	rtInformer := factory.Gateway().V1().HTTPRoutes()
	state.route = rtInformer.Lister()

	_, err = rtInformer.Informer().AddEventHandler(handler)
	if err != nil {
		return nil, errors.Wrap(err, "register httproute handler")
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
	ingresses, listErr := state.ingress.List(labels.Everything())
	if listErr != nil {
		return errors.Wrap(listErr, "list ingresses")
	}

	var (
		gateways []*gatewayv1.Gateway
		routes   []*gatewayv1.HTTPRoute
	)

	if state.gateway != nil {
		gws, err := state.gateway.List(labels.Everything())
		if err != nil {
			return errors.Wrap(err, "list gateways")
		}

		gateways = gws
	}

	if state.route != nil {
		rts, err := state.route.List(labels.Everything())
		if err != nil {
			return errors.Wrap(err, "list httproutes")
		}

		routes = rts
	}

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

	reconcileErr := c.opts.Reconciler(ctx, hosts)
	if reconcileErr != nil {
		return errors.Wrap(reconcileErr, "apply reconcile")
	}

	return nil
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
