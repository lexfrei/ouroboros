package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lexfrei/ouroboros/internal/proxy"
)

const (
	loopback       = "127.0.0.1"
	dialTimeout    = 2 * time.Second
	readyTimeout   = 500 * time.Millisecond
	shutdownGrace  = time.Second
	headerChanBuf  = 16
	roleHTTP       = "http"
	roleHTTPS      = "https"
	concurrentConn = 10
	largePayload   = 4 * 1024 * 1024
	settleDelay    = 100 * time.Millisecond
	leakTolerance  = 3
	httpPort       = 80
	httpsPort      = 443
)

var v1HeaderTCP4 = regexp.MustCompile(`^PROXY TCP4 (\d+\.\d+\.\d+\.\d+) (\d+\.\d+\.\d+\.\d+) (\d+) (\d+)\r\n$`)

// echoBackend starts a TCP listener that consumes one PROXY-protocol v1 line
// per accepted connection, publishes it on the returned channel, and echoes
// the rest back until the client half-closes. The returned channel is never
// closed — buffered, drop-on-full — so tests can race-safely receive PROXY
// header lines without coordinating with the per-connection goroutines.
func echoBackend(t *testing.T) (net.Listener, <-chan string) {
	t.Helper()

	listener, err := net.Listen("tcp", loopback+":0") //nolint:noctx // tests use the simple form
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}

	headers := make(chan string, headerChanBuf)

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			go func(client net.Conn) {
				defer func() { _ = client.Close() }()

				reader := bufio.NewReader(client)

				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}

				select {
				case headers <- line:
				default:
				}

				_, _ = io.Copy(client, reader)
			}(conn)
		}
	}()

	t.Cleanup(func() { _ = listener.Close() })

	return listener, headers
}

// hangingBackend listens but never accepts; the kernel still completes the
// TCP handshake from the listen-backlog, so dial succeeds but no application
// data ever flows.
func hangingBackend(t *testing.T) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", loopback+":0") //nolint:noctx // tests use the simple form
	if err != nil {
		t.Fatalf("listen hanging backend: %v", err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	return listener
}

// closingBackend reads the PROXY header from each accepted connection and
// then closes — used to verify the proxy reports clean EOF to the client and
// does not leak goroutines when the backend tears down early.
func closingBackend(t *testing.T) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", loopback+":0") //nolint:noctx // tests use the simple form
	if err != nil {
		t.Fatalf("listen closing backend: %v", err)
	}

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			go func(c net.Conn) {
				defer func() { _ = c.Close() }()

				_, _ = bufio.NewReader(c).ReadString('\n')
			}(conn)
		}
	}()

	t.Cleanup(func() { _ = listener.Close() })

	return listener
}

// reservedAddr returns a 127.0.0.1:port that is not currently listening: it
// opens an ephemeral port, captures it, and immediately releases it — banking
// on the kernel not reusing the port within the test window.
func reservedAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", loopback+":0") //nolint:noctx // tests use the simple form
	if err != nil {
		t.Fatalf("listen for reservation: %v", err)
	}

	addr := listener.Addr().String()

	closeErr := listener.Close()
	if closeErr != nil {
		t.Fatalf("close reservation: %v", closeErr)
	}

	return addr
}

func mustSplitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}

	port, atoiErr := strconv.Atoi(portStr)
	if atoiErr != nil {
		t.Fatalf("atoi %q: %v", portStr, atoiErr)
	}

	return host, port
}

func newProxy(t *testing.T, ctx context.Context, cfg proxy.Config) *proxy.Server {
	t.Helper()

	server, err := proxy.New(ctx, cfg)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	return server
}

// runProxy starts server.Run in a goroutine. The returned channel delivers
// the Run error exactly once (then closes) so test code can both wait for it
// explicitly *and* have the registered cleanup observe the goroutine
// terminating without re-blocking on an empty channel.
func runProxy(t *testing.T, ctx context.Context, server *proxy.Server) <-chan error {
	t.Helper()

	done := make(chan error, 1)

	go func() {
		done <- server.Run(ctx)
		close(done)
	}()

	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(dialTimeout):
			t.Errorf("proxy.Run did not return after context cancel")
		}
	})

	return done
}

func proxyForHTTPBackend(t *testing.T, ctx context.Context, backendAddr string) *proxy.Server {
	t.Helper()

	host, port := mustSplitHostPort(t, backendAddr)

	return newProxy(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		DialTimeout:     dialTimeout,
		ReadyTimeout:    readyTimeout,
		ShutdownGrace:   shutdownGrace,
	})
}

func dialContext(t *testing.T, ctx context.Context, addr string) net.Conn {
	t.Helper()

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}

	return conn
}

func dialAndEcho(t *testing.T, ctx context.Context, addr string, payload []byte) []byte {
	t.Helper()

	conn := dialContext(t, ctx, addr)

	defer func() { _ = conn.Close() }()

	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("expected *net.TCPConn, got %T", conn)
	}

	// Drive write and read concurrently. For payloads larger than the kernel
	// TCP buffer chain (client→proxy→backend→proxy→client) a sequential
	// write-then-read deadlocks: the response side fills before the request
	// side drains.
	writeErr := make(chan error, 1)

	go func() {
		_, err := tcp.Write(payload)
		if err != nil {
			writeErr <- err

			return
		}

		writeErr <- tcp.CloseWrite()
	}()

	got, readErr := io.ReadAll(conn)
	if readErr != nil {
		t.Fatalf("read echo: %v", readErr)
	}

	wErr := <-writeErr
	if wErr != nil {
		t.Fatalf("write payload: %v", wErr)
	}

	return got
}

func httpGet(t *testing.T, ctx context.Context, target string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		t.Fatalf("build request %s: %v", target, err)
	}

	resp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		t.Fatalf("GET %s: %v", target, doErr)
	}

	return resp
}

// TestNew_RejectsZeroListeners locks the contract: a Server that listens on
// nothing is a misconfiguration, not a no-op.
func TestNew_RejectsZeroListeners(t *testing.T) {
	t.Parallel()

	_, err := proxy.New(t.Context(), proxy.Config{
		BackendHost:     loopback,
		BackendHTTPPort: httpPort,
		DialTimeout:     dialTimeout,
	})
	if err == nil {
		t.Fatal("New with no listeners returned nil error")
	}
}

func TestNew_FailsOnInvalidBindAddress(t *testing.T) {
	t.Parallel()

	_, err := proxy.New(t.Context(), proxy.Config{
		HTTPListen:      "not-a-host:not-a-port",
		BackendHost:     loopback,
		BackendHTTPPort: httpPort,
		DialTimeout:     dialTimeout,
	})
	if err == nil {
		t.Fatal("New with malformed listen addr returned nil error")
	}
}

func TestNew_FailsOnPortAlreadyBound(t *testing.T) {
	t.Parallel()

	occupied, listenErr := net.Listen("tcp", loopback+":0") //nolint:noctx // tests use the simple form
	if listenErr != nil {
		t.Fatalf("listen: %v", listenErr)
	}

	t.Cleanup(func() { _ = occupied.Close() })

	_, err := proxy.New(t.Context(), proxy.Config{
		HTTPListen:      occupied.Addr().String(),
		BackendHost:     loopback,
		BackendHTTPPort: httpPort,
		DialTimeout:     dialTimeout,
	})
	if err == nil {
		t.Fatal("New on bound port returned nil error")
	}
}

func TestNew_ClosesAlreadyOpenedListenerOnSecondaryFailure(t *testing.T) {
	t.Parallel()

	occupied, listenErr := net.Listen("tcp", loopback+":0") //nolint:noctx // tests use the simple form
	if listenErr != nil {
		t.Fatalf("listen occupied: %v", listenErr)
	}

	t.Cleanup(func() { _ = occupied.Close() })

	stage, stageErr := net.Listen("tcp", loopback+":0") //nolint:noctx // tests use the simple form
	if stageErr != nil {
		t.Fatalf("listen stage: %v", stageErr)
	}

	stageAddr := stage.Addr().String()

	_ = stage.Close()

	_, newErr := proxy.New(t.Context(), proxy.Config{
		HTTPListen:       stageAddr,
		HTTPSListen:      occupied.Addr().String(),
		BackendHost:      loopback,
		BackendHTTPPort:  httpPort,
		BackendHTTPSPort: httpsPort,
		DialTimeout:      dialTimeout,
	})
	if newErr == nil {
		t.Fatal("expected New to fail when the second listener cannot bind")
	}

	again, againErr := net.Listen("tcp", stageAddr) //nolint:noctx // tests use the simple form
	if againErr != nil {
		t.Errorf("first listener was not released after secondary failure: %v", againErr)

		return
	}

	_ = again.Close()
}

func TestProxy_ForwardsAndInjectsValidPROXYHeader(t *testing.T) {
	t.Parallel()

	backend, headers := echoBackend(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := proxyForHTTPBackend(t, ctx, backend.Addr().String())

	runProxy(t, ctx, server)

	payload := []byte("hairpin payload")

	got := dialAndEcho(t, ctx, server.HTTPAddr(), payload)
	if !bytes.Equal(got, payload) {
		t.Errorf("echo: got %q, want %q", got, payload)
	}

	select {
	case header := <-headers:
		match := v1HeaderTCP4.FindStringSubmatch(header)
		if match == nil {
			t.Fatalf("PROXY header does not match v1 TCP4 spec: %q", header)
		}

		const (
			groupSrcIP = 1
			groupDstIP = 2
		)

		if match[groupSrcIP] != loopback {
			t.Errorf("src IP = %q, want %q", match[groupSrcIP], loopback)
		}

		if match[groupDstIP] != loopback {
			t.Errorf("dst IP = %q, want %q", match[groupDstIP], loopback)
		}
	case <-time.After(dialTimeout):
		t.Fatal("PROXY header not received within deadline")
	}
}

func TestProxy_BackendDialFails_FastClose(t *testing.T) {
	t.Parallel()

	deadAddr := reservedAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := proxyForHTTPBackend(t, ctx, deadAddr)

	runProxy(t, ctx, server)

	conn := dialContext(t, ctx, server.HTTPAddr())

	defer func() { _ = conn.Close() }()

	deadlineErr := conn.SetReadDeadline(time.Now().Add(dialTimeout))
	if deadlineErr != nil {
		t.Fatalf("set deadline: %v", deadlineErr)
	}

	buf := make([]byte, 1)

	n, readErr := conn.Read(buf)
	if n != 0 {
		t.Errorf("expected zero bytes when backend is unreachable, got %d", n)
	}

	if readErr == nil {
		t.Error("expected error when backend dial fails, got nil")
	}

	// net.Conn deadline errors wrap os.ErrDeadlineExceeded (not
	// context.DeadlineExceeded). If we hit that, the proxy did NOT close the
	// client connection promptly and we fell through to our own SetReadDeadline.
	if errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Error("proxy did not close client conn quickly when backend dial failed")
	}
}

func TestProxy_BackendClosesAfterHeader_ReportsCleanEOF(t *testing.T) {
	t.Parallel()

	backend := closingBackend(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := proxyForHTTPBackend(t, ctx, backend.Addr().String())

	runProxy(t, ctx, server)

	conn := dialContext(t, ctx, server.HTTPAddr())

	defer func() { _ = conn.Close() }()

	_, writeErr := conn.Write([]byte("ping"))
	if writeErr != nil {
		t.Fatalf("write: %v", writeErr)
	}

	deadlineErr := conn.SetReadDeadline(time.Now().Add(dialTimeout))
	if deadlineErr != nil {
		t.Fatalf("deadline: %v", deadlineErr)
	}

	got, readErr := io.ReadAll(conn)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !isNetworkClose(readErr) {
		t.Errorf("read returned unexpected error: %v", readErr)
	}

	if len(got) != 0 {
		t.Errorf("expected zero echo bytes when backend closes, got %d", len(got))
	}
}

func TestProxy_ClientAbort_DoesNotPanic(t *testing.T) {
	t.Parallel()

	backend, _ := echoBackend(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := proxyForHTTPBackend(t, ctx, backend.Addr().String())

	runProxy(t, ctx, server)

	for range concurrentConn {
		conn := dialContext(t, ctx, server.HTTPAddr())

		_, _ = conn.Write([]byte("partial"))

		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}

		_ = conn.Close()
	}

	time.Sleep(settleDelay)
}

func TestProxy_ContextCancel_StopsAcceptingNewConns(t *testing.T) {
	t.Parallel()

	backend, _ := echoBackend(t)

	ctx, cancel := context.WithCancel(t.Context())

	server := proxyForHTTPBackend(t, ctx, backend.Addr().String())
	done := runProxy(t, ctx, server)

	addr := server.HTTPAddr()

	time.Sleep(settleDelay)

	cancel()

	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Errorf("Run returned %v, want nil or context.Canceled", runErr)
		}
	case <-time.After(dialTimeout):
		t.Fatal("Run did not return after context cancel")
	}

	var dialer net.Dialer

	conn, dialErr := (&net.Dialer{Timeout: settleDelay}).DialContext(t.Context(), "tcp", addr)
	if dialErr == nil {
		_ = conn.Close()
		t.Errorf("listener still accepting after shutdown")
	}

	_ = dialer
}

func TestProxy_NoGoroutineLeak_AfterShutdown(t *testing.T) {
	backend, _ := echoBackend(t)

	runtime.GC()
	time.Sleep(settleDelay)

	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())

	server := proxyForHTTPBackend(t, ctx, backend.Addr().String())
	done := runProxy(t, ctx, server)

	for range concurrentConn {
		_ = dialAndEcho(t, ctx, server.HTTPAddr(), []byte("x"))
	}

	cancel()

	select {
	case <-done:
	case <-time.After(dialTimeout):
		t.Fatal("Run did not return after context cancel")
	}

	runtime.GC()
	time.Sleep(settleDelay)

	after := runtime.NumGoroutine()
	if after > baseline+leakTolerance {
		t.Errorf("goroutine leak: baseline=%d, after=%d (tolerance=%d)", baseline, after, leakTolerance)
	}
}

func TestProxy_LargePayload_Roundtrip(t *testing.T) {
	t.Parallel()

	backend, _ := echoBackend(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := proxyForHTTPBackend(t, ctx, backend.Addr().String())

	runProxy(t, ctx, server)

	payload := make([]byte, largePayload)

	_, randErr := rand.Read(payload)
	if randErr != nil {
		t.Fatalf("rand: %v", randErr)
	}

	got := dialAndEcho(t, ctx, server.HTTPAddr(), payload)
	if !bytes.Equal(got, payload) {
		t.Errorf("large payload round-trip mismatch (got %d bytes, want %d)", len(got), len(payload))
	}
}

func TestProxy_HealthzReturns200(t *testing.T) {
	t.Parallel()

	backend, _ := echoBackend(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	host, port := mustSplitHostPort(t, backend.Addr().String())

	server := newProxy(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    readyTimeout,
		ShutdownGrace:   shutdownGrace,
	})

	runProxy(t, ctx, server)

	resp := httpGet(t, ctx, "http://"+server.HealthAddr()+"/healthz")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestProxy_ReadyzReports503WhenBackendDown(t *testing.T) {
	t.Parallel()

	deadAddr := reservedAddr(t)

	host, port := mustSplitHostPort(t, deadAddr)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := newProxy(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    readyTimeout,
		ShutdownGrace:   shutdownGrace,
	})

	runProxy(t, ctx, server)

	resp := httpGet(t, ctx, "http://"+server.HealthAddr()+"/readyz")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestProxy_ReadyzDoesNotHangOnNonAcceptingBackend(t *testing.T) {
	t.Parallel()

	hanging := hangingBackend(t)

	host, port := mustSplitHostPort(t, hanging.Addr().String())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := newProxy(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    settleDelay,
		ShutdownGrace:   shutdownGrace,
	})

	runProxy(t, ctx, server)

	start := time.Now()

	resp := httpGet(t, ctx, "http://"+server.HealthAddr()+"/readyz")
	defer func() { _ = resp.Body.Close() }()

	elapsed := time.Since(start)
	if elapsed > dialTimeout {
		t.Errorf("/readyz took %v, ReadyTimeout should keep it under %v", elapsed, dialTimeout)
	}
	// A successful TCP handshake to a non-accepting listener still counts as
	// "reachable" at the kernel level — the kernel SYN-ACKs from the listen
	// backlog without the user-space process. We accept either 200 or 503;
	// the goal of this test is to assert /readyz does not block indefinitely.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want 200 or 503", resp.StatusCode)
	}
}

func TestProxy_BothListenersForward(t *testing.T) {
	t.Parallel()

	httpBackend, httpHeaders := echoBackend(t)
	httpsBackend, httpsHeaders := echoBackend(t)

	httpHost, httpPortNum := mustSplitHostPort(t, httpBackend.Addr().String())
	httpsHost, httpsPortNum := mustSplitHostPort(t, httpsBackend.Addr().String())

	if httpHost != httpsHost {
		t.Fatalf("loopback hosts differ: %s vs %s", httpHost, httpsHost)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := newProxy(t, ctx, proxy.Config{
		HTTPListen:       loopback + ":0",
		HTTPSListen:      loopback + ":0",
		BackendHost:      httpHost,
		BackendHTTPPort:  httpPortNum,
		BackendHTTPSPort: httpsPortNum,
		DialTimeout:      dialTimeout,
		ReadyTimeout:     readyTimeout,
		ShutdownGrace:    shutdownGrace,
	})

	runProxy(t, ctx, server)

	cases := []struct {
		role    string
		addr    string
		headers <-chan string
	}{
		{role: roleHTTP, addr: server.HTTPAddr(), headers: httpHeaders},
		{role: roleHTTPS, addr: server.HTTPSAddr(), headers: httpsHeaders},
	}

	for _, tt := range cases {
		got := dialAndEcho(t, ctx, tt.addr, []byte(tt.role))
		if string(got) != tt.role {
			t.Errorf("%s echo: got %q, want %q", tt.role, got, tt.role)
		}

		select {
		case header := <-tt.headers:
			if !v1HeaderTCP4.MatchString(header) {
				t.Errorf("%s PROXY header malformed: %q", tt.role, header)
			}
		case <-time.After(dialTimeout):
			t.Fatalf("%s PROXY header not received", tt.role)
		}
	}
}

func TestWriteHeader_TimesOutOnUnreadingBackend(t *testing.T) {
	t.Parallel()

	// net.Pipe returns synchronous, in-memory conns: every Write blocks
	// until a matching Read drains it. Without a deadline, writeHeader on
	// the unread side would block forever. With the deadline, it must
	// return an error within the timeout.
	pipeA, pipeB := net.Pipe()

	defer func() { _ = pipeA.Close() }()
	defer func() { _ = pipeB.Close() }()

	const headerWriteTimeout = 100 * time.Millisecond

	start := time.Now()

	err := proxy.WriteHeaderForTest(pipeA, "PROXY TCP4 1.2.3.4 5.6.7.8 1 2\r\n", headerWriteTimeout)

	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("writeHeader on unread net.Pipe must return an error")
	}

	if elapsed > headerWriteTimeout*5 {
		t.Errorf("writeHeader exceeded its timeout budget: elapsed=%v limit=%v", elapsed, headerWriteTimeout*5)
	}
}

func TestWriteHeader_SucceedsWhenPeerReads(t *testing.T) {
	t.Parallel()

	pipeA, pipeB := net.Pipe()

	defer func() { _ = pipeA.Close() }()
	defer func() { _ = pipeB.Close() }()

	go func() {
		buf := make([]byte, 256)
		_, _ = pipeB.Read(buf)
	}()

	err := proxy.WriteHeaderForTest(pipeA, "PROXY TCP4 1.2.3.4 5.6.7.8 1 2\r\n", time.Second)
	if err != nil {
		t.Fatalf("writeHeader must succeed when peer reads: %v", err)
	}
}

// lockedBuffer is a goroutine-safe bytes.Buffer. The /readyz handler logs from
// net/http handler goroutines, and the concurrent-probe test fans out many of
// them at once, so the capture sink must serialize writes and reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// bytes.Buffer.Write is documented to always consume all of p and return a
	// nil error (it panics only on OOM), so this adapter never surfaces one.
	b.buf.Write(p)

	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// newProxyWithLog mirrors newProxy but routes the server's slog output into buf
// so tests can assert on the readiness WARN/INFO transition lines. The default
// text handler level is INFO, so both WARN and INFO are captured and DEBUG is
// dropped.
func newProxyWithLog(t *testing.T, ctx context.Context, cfg proxy.Config, buf *lockedBuffer) *proxy.Server {
	t.Helper()

	cfg.Logger = slog.New(slog.NewTextHandler(buf, nil))

	return newProxy(t, ctx, cfg)
}

func readyGet(t *testing.T, ctx context.Context, server *proxy.Server) *http.Response {
	t.Helper()

	return httpGet(t, ctx, "http://"+server.HealthAddr()+"/readyz")
}

func warnCount(s string) int { return strings.Count(s, "level=WARN") }

func infoCount(s string) int { return strings.Count(s, "level=INFO") }

// Shared expectations for cause tokens that several table rows assert, so the
// same literal does not repeat across cases.
const (
	wantNXDOMAIN   = "dns-nxdomain"
	wantDNSTimeout = "dns-timeout"
	wantDNSOther   = "dns-error"
	wantTimeout    = "timeout"
	opDial         = "dial"
)

// errSyntheticDial is a static sentinel standing in for an unclassifiable dial
// error in the classification table.
var errSyntheticDial = errors.New("synthetic dial failure")

// TestClassifyDialError_Table is the offline, cross-platform source of truth
// for cause classification. It exercises the real helper against synthetic
// error chains so no failing backend (and no real DNS) is needed, which keeps
// every classification branch deterministic — including timeout, which the
// integration tests cannot drive reliably.
func TestClassifyDialError_Table(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "nxdomain", err: &net.DNSError{IsNotFound: true}, want: wantNXDOMAIN},
		// DialContext nests the *net.DNSError inside a *net.OpError; assert the
		// classifier traverses that real shape, not just a bare DNSError.
		{name: "wrapped nxdomain", err: &net.OpError{Op: opDial, Err: &net.DNSError{IsNotFound: true}}, want: wantNXDOMAIN},
		{name: "dns timeout", err: &net.DNSError{IsTimeout: true}, want: wantDNSTimeout},
		{name: "wrapped dns timeout", err: &net.OpError{Op: opDial, Err: &net.DNSError{IsTimeout: true}}, want: wantDNSTimeout},
		// A *net.DNSError that is neither NXDOMAIN nor a timeout (SERVFAIL and
		// the rest of the temporary-failure class, plus a flagless generic DNS
		// error) must stay a DNS-layer token, not collapse into "unreachable".
		{name: "dns servfail", err: &net.DNSError{IsTemporary: true}, want: wantDNSOther},
		{name: "wrapped dns servfail", err: &net.OpError{Op: opDial, Err: &net.DNSError{IsTemporary: true}}, want: wantDNSOther},
		{name: "dns generic", err: &net.DNSError{}, want: wantDNSOther},
		{name: "refused", err: &net.OpError{Op: opDial, Err: syscall.ECONNREFUSED}, want: "connection-refused"},
		// Every timeout shape DialContext realistically yields must map to
		// "timeout": the ReadyTimeout context deadline (bare and OpError-wrapped),
		// the netpoller's os.ErrDeadlineExceeded, and a kernel ETIMEDOUT — the
		// last two reach "timeout" via os.IsTimeout, not the context.Is branch,
		// so they pin that os.IsTimeout coverage and prove no ETIMEDOUT falls
		// through to "unreachable".
		{name: "context deadline", err: context.DeadlineExceeded, want: wantTimeout},
		{name: "wrapped context deadline", err: &net.OpError{Op: opDial, Err: context.DeadlineExceeded}, want: wantTimeout},
		{name: "os deadline", err: os.ErrDeadlineExceeded, want: wantTimeout},
		{name: "wrapped os deadline", err: &net.OpError{Op: opDial, Err: os.ErrDeadlineExceeded}, want: wantTimeout},
		{name: "etimedout", err: syscall.ETIMEDOUT, want: wantTimeout},
		{name: "wrapped etimedout", err: &net.OpError{Op: opDial, Err: syscall.ETIMEDOUT}, want: wantTimeout},
		{name: "unknown", err: errSyntheticDial, want: "unreachable"},
		{name: "nil", err: nil, want: ""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := proxy.ClassifyDialErrorForTest(tt.err)
			if got != tt.want {
				t.Errorf("classifyDialError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestReadyz_ClassifiesNXDOMAIN points the backend at an RFC 6761 .invalid name
// that resolves to NXDOMAIN offline, so the DNS-not-found branch is hit without
// any network dependency. ReadyTimeout is generous so a slow resolver cannot
// turn the instant NXDOMAIN into a misclassified timeout.
func TestReadyz_ClassifiesNXDOMAIN(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := &lockedBuffer{}

	server := newProxyWithLog(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     "ouroboros-nxdomain-test.invalid",
		BackendHTTPPort: httpPort,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    dialTimeout,
		ShutdownGrace:   shutdownGrace,
	}, buf)

	runProxy(t, ctx, server)

	resp := readyGet(t, ctx, server)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "(dns-nxdomain)") {
		t.Errorf("/readyz body = %q, want it to mention (dns-nxdomain)", body)
	}

	out := buf.String()
	if got := warnCount(out); got != 1 {
		t.Errorf("WARN count = %d, want 1; log:\n%s", got, out)
	}

	if !strings.Contains(out, "cause=dns-nxdomain") {
		t.Errorf("WARN missing cause=dns-nxdomain; log:\n%s", out)
	}

	if !strings.Contains(out, "ouroboros-nxdomain-test.invalid") {
		t.Errorf("WARN missing backend addr; log:\n%s", out)
	}
}

// TestReadyz_ClassifiesRefused dials a reserved-then-released loopback port,
// which the kernel refuses with ECONNREFUSED — the "name resolves but nothing
// is listening" case.
func TestReadyz_ClassifiesRefused(t *testing.T) {
	t.Parallel()

	host, port := mustSplitHostPort(t, reservedAddr(t))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := &lockedBuffer{}

	server := newProxyWithLog(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    readyTimeout,
		ShutdownGrace:   shutdownGrace,
	}, buf)

	runProxy(t, ctx, server)

	resp := readyGet(t, ctx, server)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	out := buf.String()
	if got := warnCount(out); got != 1 {
		t.Errorf("WARN count = %d, want 1; log:\n%s", got, out)
	}

	if !strings.Contains(out, "cause=connection-refused") {
		t.Errorf("WARN missing cause=connection-refused; log:\n%s", out)
	}
}

// TestReadyz_NoSpamOnRepeatedFailure is the core anti-spam guarantee: with a
// 5s probe period a per-failure log would emit ~720 lines/hour. Repeated
// failing probes must collapse to a single WARN until the state changes.
func TestReadyz_NoSpamOnRepeatedFailure(t *testing.T) {
	t.Parallel()

	host, port := mustSplitHostPort(t, reservedAddr(t))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := &lockedBuffer{}

	server := newProxyWithLog(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    readyTimeout,
		ShutdownGrace:   shutdownGrace,
	}, buf)

	runProxy(t, ctx, server)

	const probeRounds = 3

	for range probeRounds {
		resp := readyGet(t, ctx, server)

		_ = resp.Body.Close()
	}

	out := buf.String()
	if got := warnCount(out); got != 1 {
		t.Errorf("WARN count = %d after %d failing probes, want exactly 1; log:\n%s", got, probeRounds, out)
	}
}

// TestReadyz_RecoveryLogsOnceThenQuiet walks unhealthy → healthy → healthy:
// one WARN when the backend is down, one INFO when it comes back on the same
// address, and silence on the steady-healthy probe that follows.
func TestReadyz_RecoveryLogsOnceThenQuiet(t *testing.T) {
	t.Parallel()

	deadAddr := reservedAddr(t)
	host, port := mustSplitHostPort(t, deadAddr)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := &lockedBuffer{}

	server := newProxyWithLog(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    readyTimeout,
		ShutdownGrace:   shutdownGrace,
	}, buf)

	runProxy(t, ctx, server)

	resp1 := readyGet(t, ctx, server)
	_ = resp1.Body.Close()

	if got := warnCount(buf.String()); got != 1 {
		t.Fatalf("phase 1 WARN count = %d, want 1; log:\n%s", got, buf.String())
	}

	recovered, err := net.Listen("tcp", deadAddr) //nolint:noctx // re-binding the reserved addr; no context needed
	if err != nil {
		t.Skipf("could not re-bind %s for the recovery phase: %v", deadAddr, err)
	}

	defer func() { _ = recovered.Close() }()

	resp2 := readyGet(t, ctx, server)
	_ = resp2.Body.Close()

	if got := infoCount(buf.String()); got != 1 {
		t.Fatalf("phase 2 INFO count = %d, want 1; log:\n%s", got, buf.String())
	}

	if got := warnCount(buf.String()); got != 1 {
		t.Errorf("phase 2 added a WARN; want still 1; log:\n%s", buf.String())
	}

	before := buf.String()

	resp3 := readyGet(t, ctx, server)
	_ = resp3.Body.Close()

	if buf.String() != before {
		t.Errorf("steady-healthy probe emitted a new line:\n%s", strings.TrimPrefix(buf.String(), before))
	}
}

// TestReadyz_FirstObservationHealthy_NoLog locks the contract that a normal
// pod whose backend is up from the start logs nothing — no startup INFO noise.
func TestReadyz_FirstObservationHealthy_NoLog(t *testing.T) {
	t.Parallel()

	backend, _ := echoBackend(t)
	host, port := mustSplitHostPort(t, backend.Addr().String())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := &lockedBuffer{}

	server := newProxyWithLog(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    readyTimeout,
		ShutdownGrace:   shutdownGrace,
	}, buf)

	runProxy(t, ctx, server)

	resp := readyGet(t, ctx, server)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	if out := buf.String(); out != "" {
		t.Errorf("clean first-healthy probe logged unexpectedly:\n%s", out)
	}
}

// TestReadyz_ConcurrentProbes_NoRace fans out many simultaneous probes against
// a down backend. Under -race it guards the readiness-state mutex, and the
// transition logic must still collapse the burst to exactly one WARN.
func TestReadyz_ConcurrentProbes_NoRace(t *testing.T) {
	t.Parallel()

	host, port := mustSplitHostPort(t, reservedAddr(t))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := &lockedBuffer{}

	server := newProxyWithLog(t, ctx, proxy.Config{
		HTTPListen:      loopback + ":0",
		BackendHost:     host,
		BackendHTTPPort: port,
		HealthListen:    loopback + ":0",
		DialTimeout:     dialTimeout,
		ReadyTimeout:    readyTimeout,
		ShutdownGrace:   shutdownGrace,
	}, buf)

	runProxy(t, ctx, server)

	const concurrentProbes = 20

	target := "http://" + server.HealthAddr() + "/readyz"

	var wg sync.WaitGroup

	wg.Add(concurrentProbes)

	for range concurrentProbes {
		go func() {
			defer wg.Done()

			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
			if reqErr != nil {
				return
			}

			resp, doErr := http.DefaultClient.Do(req)
			if doErr != nil {
				return
			}

			_ = resp.Body.Close()
		}()
	}

	wg.Wait()

	if got := warnCount(buf.String()); got != 1 {
		t.Errorf("WARN count = %d under concurrent probes, want exactly 1; log:\n%s", got, buf.String())
	}
}

// isNetworkClose reports whether err looks like a peer reset / connection
// closed condition that's acceptable in tests where the backend deliberately
// tears down mid-stream.
func isNetworkClose(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}

	msg := err.Error()

	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe")
}
