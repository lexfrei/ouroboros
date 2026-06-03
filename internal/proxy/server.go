package proxy

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
)

const (
	defaultDialTimeout      = 5 * time.Second
	defaultReadyTimeout     = 2 * time.Second
	defaultShutdownGrace    = 30 * time.Second
	healthReadHeaderTimeout = 5 * time.Second
)

// Readiness-failure cause tokens. They surface both in the WARN log field and
// in the /readyz 503 body, so an operator sees the same classification from
// `kubectl logs` and from a manual `curl /readyz`.
const (
	causeDNSNotFound = "dns-nxdomain"
	causeDNSTimeout  = "dns-timeout"
	causeDNSOther    = "dns-error"
	causeRefused     = "connection-refused"
	causeTimeout     = "timeout"
	causeUnknown     = "unreachable"
)

// Config configures a proxy Server.
type Config struct {
	HTTPListen       string
	HTTPSListen      string
	HealthListen     string
	BackendHost      string
	BackendHTTPPort  int
	BackendHTTPSPort int
	DialTimeout      time.Duration
	ReadyTimeout     time.Duration
	ShutdownGrace    time.Duration
	Logger           *slog.Logger
}

// Server is a TCP relay that injects a PROXY-protocol v1 header at the head
// of each forwarded stream.
type Server struct {
	cfg     Config
	httpL   net.Listener
	httpsL  net.Listener
	healthL net.Listener
	log     *slog.Logger
	active  sync.WaitGroup

	// Readiness-transition tracking. The /readyz handler runs in concurrent
	// net/http goroutines, so these are guarded by readyMu. They let the proxy
	// log the backend failure once per state change instead of on every probe
	// (the probe fires every few seconds, so per-probe logging would flood).
	readyMu       sync.Mutex
	readyObserved bool // false until the first /readyz probe records a state
	readyHealthy  bool // last observed health; meaningful only when readyObserved
}

// New opens any configured listeners and returns a Server. At least one of
// HTTPListen or HTTPSListen must be non-empty. If a later listener fails to
// bind, all previously-opened listeners are closed before the error is
// returned, so callers do not leak file descriptors.
//
//nolint:gocritic // ergonomic struct-literal call site for a public constructor
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.HTTPListen == "" && cfg.HTTPSListen == "" {
		return nil, errors.New("proxy: at least one of HTTPListen or HTTPSListen must be set")
	}

	applyDefaults(&cfg)

	srv := &Server{cfg: cfg, log: cfg.Logger}
	if srv.log == nil {
		srv.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	openErr := srv.openListeners(ctx)
	if openErr != nil {
		srv.closeListeners()

		return nil, openErr
	}

	return srv, nil
}

// HTTPAddr returns the actual HTTP listener address, or an empty string when
// no HTTP listener is configured.
func (srv *Server) HTTPAddr() string { return addrOrEmpty(srv.httpL) }

// HTTPSAddr returns the actual HTTPS listener address, or an empty string
// when no HTTPS listener is configured.
func (srv *Server) HTTPSAddr() string { return addrOrEmpty(srv.httpsL) }

// HealthAddr returns the actual health listener address, or an empty string
// when no health listener is configured.
func (srv *Server) HealthAddr() string { return addrOrEmpty(srv.healthL) }

// Run blocks until ctx is canceled or a fatal listener error occurs. On
// shutdown it closes all listeners, then waits up to cfg.ShutdownGrace for
// in-flight connections to drain. A clean shutdown returns nil even when the
// underlying error is context.Canceled or net.ErrClosed.
func (srv *Server) Run(ctx context.Context) error {
	group, gctx := errgroup.WithContext(ctx)

	if srv.httpL != nil {
		group.Go(func() error {
			return srv.acceptLoop(gctx, srv.httpL, srv.cfg.BackendHost, srv.cfg.BackendHTTPPort)
		})
	}

	if srv.httpsL != nil {
		group.Go(func() error {
			return srv.acceptLoop(gctx, srv.httpsL, srv.cfg.BackendHost, srv.cfg.BackendHTTPSPort)
		})
	}

	if srv.healthL != nil {
		group.Go(func() error { return srv.runHealth(gctx) })
	}

	groupErr := group.Wait()

	srv.drainActive()

	if groupErr == nil || stderrors.Is(groupErr, context.Canceled) || stderrors.Is(groupErr, net.ErrClosed) {
		return nil
	}

	return errors.Wrap(groupErr, "proxy run")
}

func (srv *Server) openListeners(ctx context.Context) error {
	var listenCfg net.ListenConfig

	if srv.cfg.HTTPListen != "" {
		listener, err := listenCfg.Listen(ctx, "tcp", srv.cfg.HTTPListen)
		if err != nil {
			return errors.Wrapf(err, "listen http %s", srv.cfg.HTTPListen)
		}

		srv.httpL = listener
	}

	if srv.cfg.HTTPSListen != "" {
		listener, err := listenCfg.Listen(ctx, "tcp", srv.cfg.HTTPSListen)
		if err != nil {
			return errors.Wrapf(err, "listen https %s", srv.cfg.HTTPSListen)
		}

		srv.httpsL = listener
	}

	if srv.cfg.HealthListen != "" {
		listener, err := listenCfg.Listen(ctx, "tcp", srv.cfg.HealthListen)
		if err != nil {
			return errors.Wrapf(err, "listen health %s", srv.cfg.HealthListen)
		}

		srv.healthL = listener
	}

	return nil
}

func (srv *Server) closeListeners() {
	for _, listener := range []net.Listener{srv.httpL, srv.httpsL, srv.healthL} {
		if listener != nil {
			_ = listener.Close()
		}
	}
}

func (srv *Server) drainActive() {
	drained := make(chan struct{})

	go func() {
		srv.active.Wait()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(srv.cfg.ShutdownGrace):
		srv.log.Warn("shutdown grace expired with active connections")
	}
}

func (srv *Server) acceptLoop(ctx context.Context, listener net.Listener, backendHost string, backendPort int) error {
	go func() {
		<-ctx.Done()

		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if stderrors.Is(err, net.ErrClosed) {
				return nil
			}

			return errors.Wrap(err, "accept")
		}

		srv.active.Go(func() {
			srv.handleConn(ctx, conn, backendHost, backendPort)
		})
	}
}

func (srv *Server) handleConn(ctx context.Context, client net.Conn, backendHost string, backendPort int) {
	defer func() { _ = client.Close() }()

	backend, err := srv.dialBackend(ctx, backendHost, backendPort)
	if err != nil {
		srv.log.Warn("backend dial failed",
			"host", backendHost, "port", backendPort, "error", err)

		return
	}

	defer func() { _ = backend.Close() }()

	closer := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
			_ = backend.Close()
		case <-closer:
		}
	}()

	defer close(closer)

	header := V1Header(client.RemoteAddr(), client.LocalAddr())

	writeErr := writeHeader(backend, header, srv.cfg.DialTimeout)
	if writeErr != nil {
		srv.log.Warn("write proxy header", "error", writeErr)

		return
	}

	relay(client, backend)
}

// writeHeader sends the PROXY-protocol v1 prefix to backend with a write
// deadline. The deadline guards against a hostile or hung backend that
// completed the TCP handshake but never reads — without it, the goroutine
// would block on Write indefinitely. The deadline is cleared after the
// header lands so that the bidi relay can run open-ended.
func writeHeader(backend net.Conn, header string, timeout time.Duration) error {
	setErr := backend.SetWriteDeadline(time.Now().Add(timeout))
	if setErr != nil {
		return errors.Wrap(setErr, "set write deadline")
	}

	_, writeErr := io.WriteString(backend, header)
	if writeErr != nil {
		return errors.Wrap(writeErr, "write proxy header")
	}

	clearErr := backend.SetWriteDeadline(time.Time{})
	if clearErr != nil {
		return errors.Wrap(clearErr, "clear write deadline")
	}

	return nil
}

func (srv *Server) dialBackend(ctx context.Context, host string, port int) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	dialCtx, cancel := context.WithTimeout(ctx, srv.cfg.DialTimeout)
	defer cancel()

	var dialer net.Dialer

	conn, err := dialer.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, errors.Wrapf(err, "dial backend %s", addr)
	}

	return conn, nil
}

func (srv *Server) runHealth(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", srv.handleReady)

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: healthReadHeaderTimeout,
	}

	go func() {
		<-ctx.Done()

		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), srv.cfg.ShutdownGrace)
		defer cancel()

		_ = httpSrv.Shutdown(shutCtx)
	}()

	serveErr := httpSrv.Serve(srv.healthL)
	if !stderrors.Is(serveErr, http.ErrServerClosed) {
		return errors.Wrap(serveErr, "health serve")
	}

	return nil
}

func (srv *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	addr := net.JoinHostPort(srv.cfg.BackendHost, strconv.Itoa(srv.cfg.BackendHTTPPort))

	dialCtx, cancel := context.WithTimeout(r.Context(), srv.cfg.ReadyTimeout)
	defer cancel()

	var dialer net.Dialer

	conn, dialErr := dialer.DialContext(dialCtx, "tcp", addr)

	// Classify once so the WARN log field and the 503 body always agree on the
	// cause. cause is "" when the dial succeeded.
	cause := classifyDialError(dialErr)

	srv.recordReady(dialErr == nil, addr, cause, dialErr)

	if dialErr != nil {
		http.Error(w,
			fmt.Sprintf("backend unreachable (%s): %v", cause, dialErr),
			http.StatusServiceUnavailable)

		return
	}

	_ = conn.Close()

	w.WriteHeader(http.StatusOK)
}

// recordReady logs the readiness state only when it changes, so a backend that
// is down stays loud exactly once (one WARN) rather than on every probe. The
// first observation logs only if it is already a failure — a clean start stays
// silent. A recovery from a previously-failed state logs a single INFO.
//
// The tracked state is a single boolean, so a failure whose cause shifts (for
// example dns-nxdomain → connection-refused) without ever passing through a
// healthy probe is intentionally not re-logged; the live cause always remains
// in the /readyz 503 body for a manual probe. cause and dialErr are read only
// on the failure branch, where the caller passes the classified token and the
// non-nil dial error.
func (srv *Server) recordReady(healthy bool, addr, cause string, dialErr error) {
	srv.readyMu.Lock()
	defer srv.readyMu.Unlock()

	prevObserved, prevHealthy := srv.readyObserved, srv.readyHealthy
	srv.readyObserved, srv.readyHealthy = true, healthy

	switch {
	case healthy && prevObserved && !prevHealthy:
		srv.log.Info("backend readiness recovered", "backend", addr)
	case !healthy && (!prevObserved || prevHealthy):
		srv.log.Warn("backend readiness check failed",
			"backend", addr,
			"cause", cause,
			"error", dialErr.Error())
	}
}

// classifyDialError maps a backend dial failure to a short, stable cause token.
// The dial error from handleReady is a plain stdlib chain (not cockroachdb-
// wrapped), so stderrors As/Is traverse it to the concrete net types — a DNS
// failure arrives as a *net.DNSError nested inside the *net.OpError that
// DialContext returns. Order is most-specific first: a name that does not
// resolve (dns-nxdomain, typically a wrong Service/namespace) and a slow or
// unreachable cluster DNS (dns-timeout) are split out ahead of the generic
// connection failures, because they point an operator at different root causes.
// Any other *net.DNSError (SERVFAIL and the rest of the temporary-failure
// class) returns dns-error rather than falling through, so a DNS-layer problem
// never masquerades as a generic connection failure — preserving the DNS-vs-
// dial distinction this classification exists to surface.
func classifyDialError(err error) string {
	if err == nil {
		return ""
	}

	if dnsErr, ok := stderrors.AsType[*net.DNSError](err); ok {
		switch {
		case dnsErr.IsNotFound:
			return causeDNSNotFound
		case dnsErr.IsTimeout:
			return causeDNSTimeout
		default:
			return causeDNSOther
		}
	}

	switch {
	case stderrors.Is(err, syscall.ECONNREFUSED):
		return causeRefused
	case stderrors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return causeTimeout
	default:
		return causeUnknown
	}
}

func applyDefaults(cfg *Config) {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}

	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}

	if cfg.ShutdownGrace == 0 {
		cfg.ShutdownGrace = defaultShutdownGrace
	}
}

func addrOrEmpty(listener net.Listener) string {
	if listener == nil {
		return ""
	}

	return listener.Addr().String()
}

// WriteHeaderForTest is a test-only export of writeHeader that lets the
// proxy_test package exercise the deadline path without standing up a real
// backend. It is intentionally not part of the documented API.
func WriteHeaderForTest(backend net.Conn, header string, timeout time.Duration) error {
	return writeHeader(backend, header, timeout)
}

// ClassifyDialErrorForTest is a test-only export of classifyDialError so the
// proxy_test package can table-test every classification branch against
// synthetic error chains, without provoking a real failing backend (or real
// DNS) for each case. It is intentionally not part of the documented API.
func ClassifyDialErrorForTest(err error) string {
	return classifyDialError(err)
}

// relay copies data bidirectionally between client and backend, propagating
// half-close (CloseWrite) so each side observes EOF on its read after the
// peer is done sending. Half-close — instead of full Close on one direction
// finishing — preserves any in-flight echo bytes that the backend was still
// writing when the client was done sending. Goroutine leaks on aborted peers
// are handled at handleConn level by the ctx-watcher that force-closes both
// ends on context cancellation.
func relay(client, backend net.Conn) {
	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		_, _ = io.Copy(backend, client)

		if tcp, ok := backend.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()

		_, _ = io.Copy(client, backend)

		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()

	wg.Wait()
}
