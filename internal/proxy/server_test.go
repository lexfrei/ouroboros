package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
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
