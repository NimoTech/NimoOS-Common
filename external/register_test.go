package external

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Common/model"
)

// fakeGateway answers POST /v1/gateway/routes with the status `status()` returns
// and records every route it accepted.
type fakeGateway struct {
	srv      *httptest.Server
	mu       sync.Mutex
	status   func(path string) int
	accepted []string
	calls    atomic.Int32
}

func newFakeGateway(t *testing.T, status func(path string) int) *fakeGateway {
	g := &fakeGateway{status: status}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != APIGatewayRoutes {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		g.calls.Add(1)
		var route model.Route
		_ = json.NewDecoder(r.Body).Decode(&route)
		g.mu.Lock()
		code := g.status(route.Path)
		if code == http.StatusCreated {
			g.accepted = append(g.accepted, route.Path)
		}
		g.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) setStatus(f func(path string) int) {
	g.mu.Lock()
	g.status = f
	g.mu.Unlock()
}

func (g *fakeGateway) acceptedPaths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.accepted...)
}

func writeAddress(t *testing.T, dir, address string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ManagementURLFilename), []byte(address+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func routes(paths ...string) []*model.Route {
	out := make([]*model.Route, 0, len(paths))
	for _, p := range paths {
		out = append(out, &model.Route{Path: p, Target: "http://127.0.0.1:1"})
	}
	return out
}

// fast keeps the tests quick: millisecond backoff, short sync window.
func fast(wait time.Duration) RegisterOptions {
	return RegisterOptions{
		Wait:           wait,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		Log:            func(string, error) {},
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestRegisterRoutesSucceedsAfterTransientErrors(t *testing.T) {
	var n atomic.Int32
	g := newFakeGateway(t, func(string) int {
		if n.Add(1) <= 2 {
			return http.StatusServiceUnavailable
		}
		return http.StatusCreated
	})
	dir := t.TempDir()
	writeAddress(t, dir, g.srv.URL)

	var registered atomic.Bool
	opts := fast(2 * time.Second)
	opts.OnRegistered = func() { registered.Store(true) }
	if err := RegisterRoutes(context.Background(), dir, routes("/v1/a", "/v1/b"), opts); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	if got := g.acceptedPaths(); len(got) != 2 {
		t.Fatalf("accepted %v, want both routes", got)
	}
	if !registered.Load() {
		t.Fatal("OnRegistered not called")
	}
}

func TestRegisterRoutesDoesNotResubmitAcceptedRoutes(t *testing.T) {
	var bTries atomic.Int32
	g := newFakeGateway(t, func(path string) int {
		if path == "/v1/b" && bTries.Add(1) <= 2 {
			return http.StatusBadGateway
		}
		return http.StatusCreated
	})
	dir := t.TempDir()
	writeAddress(t, dir, g.srv.URL)

	if err := RegisterRoutes(context.Background(), dir, routes("/v1/a", "/v1/b"), fast(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := g.acceptedPaths(); len(got) != 2 || got[0] != "/v1/a" || got[1] != "/v1/b" {
		t.Fatalf("accepted %v, want [/v1/a /v1/b] with /v1/a submitted once", got)
	}
}

func TestRegisterRoutesFallsBackToBackgroundThenRecovers(t *testing.T) {
	// A Gateway that rejects us (e.g. a version mismatch, 403) until it is
	// upgraded: the service must keep running and register once it can.
	g := newFakeGateway(t, func(string) int { return http.StatusForbidden })
	dir := t.TempDir()
	writeAddress(t, dir, g.srv.URL)

	var registered atomic.Bool
	opts := fast(50 * time.Millisecond)
	opts.OnRegistered = func() { registered.Store(true) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := RegisterRoutes(ctx, dir, routes("/v1/a"), opts)
	if !errors.Is(err, ErrRegistrationPending) {
		t.Fatalf("err = %v, want ErrRegistrationPending", err)
	}
	if registered.Load() {
		t.Fatal("OnRegistered called before registration succeeded")
	}

	g.setStatus(func(string) int { return http.StatusCreated })
	waitFor(t, registered.Load)
	if got := g.acceptedPaths(); len(got) != 1 {
		t.Fatalf("accepted %v", got)
	}
}

func TestRegisterRoutesWaitsForAddressFile(t *testing.T) {
	g := newFakeGateway(t, func(string) int { return http.StatusCreated })
	dir := t.TempDir()
	go func() {
		time.Sleep(30 * time.Millisecond)
		writeAddress(t, dir, g.srv.URL)
	}()
	if err := RegisterRoutes(context.Background(), dir, routes("/v1/a"), fast(2*time.Second)); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
}

func TestRegisterRoutesFollowsGatewayToNewAddress(t *testing.T) {
	// The Gateway restarts on a new port: the next attempt must read the
	// address file again instead of retrying the old address forever.
	old := newFakeGateway(t, func(string) int { return http.StatusServiceUnavailable })
	moved := newFakeGateway(t, func(string) int { return http.StatusCreated })
	dir := t.TempDir()
	writeAddress(t, dir, old.srv.URL)

	var registered atomic.Bool
	opts := fast(30 * time.Millisecond)
	opts.OnRegistered = func() { registered.Store(true) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = RegisterRoutes(ctx, dir, routes("/v1/a"), opts)

	writeAddress(t, dir, moved.srv.URL)
	waitFor(t, registered.Load)
	if len(moved.acceptedPaths()) != 1 {
		t.Fatal("route not registered at the new address")
	}
}

func TestRegisterRoutesStopsWhenContextIsCancelled(t *testing.T) {
	g := newFakeGateway(t, func(string) int { return http.StatusServiceUnavailable })
	dir := t.TempDir()
	writeAddress(t, dir, g.srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	opts := fast(20 * time.Millisecond)
	stopped := make(chan struct{})
	opts.onBackgroundExit = func() { close(stopped) }
	if err := RegisterRoutes(ctx, dir, routes("/v1/a"), opts); !errors.Is(err, ErrRegistrationPending) {
		t.Fatalf("err = %v", err)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("background retry did not stop after ctx was cancelled")
	}
	before := g.calls.Load()
	time.Sleep(60 * time.Millisecond)
	if after := g.calls.Load(); after != before {
		t.Fatalf("still calling the gateway after cancel: %d -> %d", before, after)
	}
}

func TestRegisterRoutesCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RegisterRoutes(ctx, t.TempDir(), routes("/v1/a"), fast(time.Second))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRegisterRoutesNoRoutes(t *testing.T) {
	called := false
	opts := fast(time.Second)
	opts.OnRegistered = func() { called = true }
	if err := RegisterRoutes(context.Background(), t.TempDir(), nil, opts); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("OnRegistered not called for an empty route list")
	}
}

func TestRegisterRoutesReplaysEverythingAtANewGateway(t *testing.T) {
	// The old Gateway accepted /v1/a, then went away; the new instance at a
	// new address must get /v1/a too, not only what was still pending.
	old := newFakeGateway(t, func(path string) int {
		if path == "/v1/a" {
			return http.StatusCreated
		}
		return http.StatusServiceUnavailable
	})
	moved := newFakeGateway(t, func(string) int { return http.StatusCreated })
	dir := t.TempDir()
	writeAddress(t, dir, old.srv.URL)

	var registered atomic.Bool
	opts := fast(30 * time.Millisecond)
	opts.OnRegistered = func() { registered.Store(true) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = RegisterRoutes(ctx, dir, routes("/v1/a", "/v1/b"), opts)

	writeAddress(t, dir, moved.srv.URL)
	waitFor(t, registered.Load)
	got := moved.acceptedPaths()
	if len(got) != 2 {
		t.Fatalf("new gateway accepted %v, want both routes", got)
	}
}

func TestRegisterRoutesSyncPhaseRespectsWaitWithAHangingGateway(t *testing.T) {
	// A Gateway that accepts the connection but never answers: the synchronous
	// phase must end near opts.Wait, not after AttemptTimeout per route.
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		hang.CloseClientConnections()
		hang.Close()
	}()
	dir := t.TempDir()
	writeAddress(t, dir, hang.URL)

	opts := fast(100 * time.Millisecond)
	opts.AttemptTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	err := RegisterRoutes(ctx, dir, routes("/v1/a", "/v1/b", "/v1/c"), opts)
	if !errors.Is(err, ErrRegistrationPending) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("sync phase took %v, want about opts.Wait (100ms)", d)
	}
}

func TestRegisterRoutesCopiesRoutes(t *testing.T) {
	// The caller changes its Route after RegisterRoutes returned; the background
	// retry must still register what was passed in.
	g := newFakeGateway(t, func(string) int { return http.StatusServiceUnavailable })
	dir := t.TempDir()
	writeAddress(t, dir, g.srv.URL)

	var registered atomic.Bool
	opts := fast(20 * time.Millisecond)
	opts.OnRegistered = func() { registered.Store(true) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs := routes("/v1/original")
	_ = RegisterRoutes(ctx, dir, rs, opts)
	rs[0].Path = "/v1/changed"

	g.setStatus(func(string) int { return http.StatusCreated })
	waitFor(t, registered.Load)
	if got := g.acceptedPaths(); len(got) != 1 || got[0] != "/v1/original" {
		t.Fatalf("accepted %v, want [/v1/original]", got)
	}
}

func TestRegisterRoutesJitteredWaitStaysWithinWait(t *testing.T) {
	// Fast failures with a backoff close to Wait: a positive jitter must not
	// carry the synchronous phase past Wait.
	g := newFakeGateway(t, func(string) int { return http.StatusServiceUnavailable })
	dir := t.TempDir()
	writeAddress(t, dir, g.srv.URL)

	opts := fast(120 * time.Millisecond)
	opts.InitialBackoff = 100 * time.Millisecond
	opts.MaxBackoff = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 20; i++ { // jitter is random: repeat
		start := time.Now()
		_ = RegisterRoutes(ctx, dir, routes("/v1/a"), opts)
		if d := time.Since(start); d > 160*time.Millisecond {
			t.Fatalf("sync phase took %v with Wait=120ms", d)
		}
	}
}

func TestLazyManagementServiceNeverFailsAtConstruction(t *testing.T) {
	dir := t.TempDir()
	m := NewLazyManagementService(dir) // no management.url yet: must not block or fail
	if err := m.CreateRoute(&model.Route{Path: "/v1/a", Target: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("CreateRoute succeeded without a Gateway address")
	}

	g := newFakeGateway(t, func(string) int { return http.StatusCreated })
	writeAddress(t, dir, g.srv.URL)
	if err := m.CreateRoute(&model.Route{Path: "/v1/a", Target: "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("CreateRoute after the address appeared: %v", err)
	}
	if got := g.acceptedPaths(); len(got) != 1 {
		t.Fatalf("accepted %v", got)
	}
}
