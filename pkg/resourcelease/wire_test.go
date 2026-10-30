package resourcelease

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type goldenFile struct {
	Schema  int    `json:"schema"`
	Source  string `json:"source"`
	Acquire []struct {
		Name         string          `json:"name"`
		Client       string          `json:"client"`
		DefaultClass string          `json:"default_class"`
		Request      json.RawMessage `json:"request"`
		Wire         json.RawMessage `json:"wire"`
	} `json:"acquire"`
	Release []struct {
		Name string          `json:"name"`
		Info json.RawMessage `json:"info"`
		Body json.RawMessage `json:"body"`
	} `json:"release"`
}

// goldenRequest is the golden "request" object: the wire field names plus
// max_wait_ms and devices.
type goldenRequest struct {
	Task           string         `json:"task"`
	Class          string         `json:"class"`
	Priority       int            `json:"priority"`
	Tier           string         `json:"tier"`
	Kind           string         `json:"kind"`
	Workload       string         `json:"workload"`
	DevicePref     []string       `json:"device_pref"`
	Devices        []string       `json:"devices"`
	AllowFallback  bool           `json:"allow_fallback"`
	PID            int            `json:"pid"`
	EstVRAMMB      uint64         `json:"est_vram_mb"`
	EstRAMMB       uint64         `json:"est_ram_mb"`
	SizeHint       uint64         `json:"size_hint"`
	FloorRAMMB     uint64         `json:"floor_ram_mb"`
	FloorVRAMMB    uint64         `json:"floor_vram_mb"`
	TTLSec         int            `json:"ttl_s"`
	Budget         map[string]any `json:"budget"`
	AlreadyLoaded  bool           `json:"already_loaded"`
	AlreadyRunning bool           `json:"already_running"`
	MaxWaitMs      int            `json:"max_wait_ms"`
}

func (g goldenRequest) toRequest() Request {
	maxWait := time.Duration(g.MaxWaitMs) * time.Millisecond
	if g.MaxWaitMs < 0 {
		maxWait = -1
	}
	return Request{
		Task: g.Task, Class: g.Class, Priority: g.Priority, Tier: Tier(g.Tier), Kind: g.Kind,
		Workload: g.Workload, DevicePref: g.DevicePref, Devices: g.Devices, AllowFallback: g.AllowFallback,
		PID: g.PID, EstVRAMMB: g.EstVRAMMB, EstRAMMB: g.EstRAMMB, SizeHint: g.SizeHint,
		FloorRAMMB: g.FloorRAMMB, FloorVRAMMB: g.FloorVRAMMB, TTLSec: g.TTLSec, Budget: g.Budget,
		AlreadyLoaded: g.AlreadyLoaded, AlreadyRunning: g.AlreadyRunning, MaxWait: maxWait,
	}
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden_requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		t.Fatalf("golden file: %v", err)
	}
	if g.Schema != 1 || len(g.Acquire) == 0 || len(g.Release) == 0 {
		t.Fatalf("golden file: schema %d, %d acquire, %d release", g.Schema, len(g.Acquire), len(g.Release))
	}
	return g
}

// roundTrip marshals v and decodes it back into a generic value.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGoldenAcquirePayloads(t *testing.T) {
	g := loadGolden(t)
	for _, tc := range g.Acquire {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			var gr goldenRequest
			dec := json.NewDecoder(strings.NewReader(string(tc.Request)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&gr); err != nil {
				t.Fatalf("request: %v", err)
			}
			r := gr.toRequest()
			got := roundTrip(t, buildPayload(tc.Client, tc.DefaultClass, r, waitMsFor(r, 30*time.Second)))
			var want any
			if err := json.Unmarshal(tc.Wire, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				t.Fatalf("payload mismatch\n got %s\nwant %s", gb, wb)
			}
		})
	}
}

// TestBuildPayloadBudgetIsPassedThrough checks the budget map is the caller's
// own map (never copied or rewritten) and that the payload keeps it as is.
func TestBuildPayloadBudgetIsPassedThrough(t *testing.T) {
	budget := map[string]any{"schema_version": 1, "anything": []any{"x", nil}}
	p := buildPayload("ai", "", Request{Task: "t", Budget: budget}, 0)
	got, ok := p["budget"].(map[string]any)
	if !ok || reflect.ValueOf(got).Pointer() != reflect.ValueOf(budget).Pointer() {
		t.Fatalf("budget not passed through: %#v", p["budget"])
	}
	if p["class"] != "light" {
		t.Fatalf("class with no default = %v, want light", p["class"])
	}
	if _, ok := buildPayload("ai", "", Request{Task: "t"}, 0)["budget"]; ok {
		t.Fatal("nil budget must be omitted")
	}
}

func TestWaitMsFor(t *testing.T) {
	poll := 30 * time.Second
	cases := []struct {
		maxWait time.Duration
		want    int
	}{
		{0, 0},
		{-1, 30000},
		{-time.Hour, 30000},
		{15 * time.Second, 15000},
		{time.Minute, 30000},
		{500 * time.Microsecond, 1},
		{1, 1},
	}
	for _, c := range cases {
		if got := waitMsFor(Request{MaxWait: c.maxWait}, poll); got != c.want {
			t.Errorf("waitMsFor(%v) = %d, want %d", c.maxWait, got, c.want)
		}
	}
	if got := waitMsFor(Request{MaxWait: -1}, 50*time.Millisecond); got != 50 {
		t.Errorf("custom poll wait = %d, want 50", got)
	}
}

func TestGoldenReleaseBodies(t *testing.T) {
	g := loadGolden(t)
	for _, tc := range g.Release {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			var in struct {
				ObservedPeakMB     uint64 `json:"observed_peak_mb"`
				ObservedPeakVRAMMB uint64 `json:"observed_peak_vram_mb"`
				Shared             bool   `json:"shared"`
			}
			dec := json.NewDecoder(strings.NewReader(string(tc.Info)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil {
				t.Fatal(err)
			}
			body := releaseBody(ReleaseInfo{ObservedPeakMB: in.ObservedPeakMB, ObservedPeakVRAMMB: in.ObservedPeakVRAMMB, Shared: in.Shared})
			var want any
			if err := json.Unmarshal(tc.Body, &want); err != nil {
				t.Fatal(err)
			}
			if want == nil {
				if body != nil {
					t.Fatalf("body = %#v, want nil", body)
				}
				return
			}
			if got := roundTrip(t, body); !reflect.DeepEqual(got, want) {
				t.Fatalf("body = %#v, want %#v", got, want)
			}
		})
	}
	// Shared without any peak sends no body, as Parser does: a 0 MB peak
	// would only drag the core's estimate down.
	if b := releaseBody(ReleaseInfo{Shared: true}); b != nil {
		t.Fatalf("shared only = %#v, want no body", b)
	}
	// A VRAM peak alone omits observed_peak_mb, as Parser does (it sends
	// only the peaks it measured); a measured RAM peak of 0 next to it is
	// indistinguishable and omitted too.
	if b := releaseBody(ReleaseInfo{ObservedPeakVRAMMB: 7}); !reflect.DeepEqual(b, map[string]any{"observed_peak_vram_mb": uint64(7), "shared": false}) {
		t.Fatalf("vram only = %#v", b)
	}
	if b := releaseBody(ReleaseInfo{ObservedPeakVRAMMB: 7, Shared: true}); !reflect.DeepEqual(b, map[string]any{"observed_peak_vram_mb": uint64(7), "shared": true}) {
		t.Fatalf("vram only shared = %#v", b)
	}
}

func TestHeartbeatBody(t *testing.T) {
	got := roundTrip(t, heartbeatBody(HeartbeatInfo{}))
	if want := map[string]any{"in_use": 0.0, "loaded": false}; !reflect.DeepEqual(got, want) {
		t.Fatalf("zero info = %#v, want %#v", got, want)
	}
	at := time.Date(2026, 10, 29, 9, 2, 3, 456, time.FixedZone("CST", 8*3600))
	got = roundTrip(t, heartbeatBody(HeartbeatInfo{InUse: 2, LastUsedAt: at, Tier: TierInteractive, Loaded: true}))
	want := map[string]any{"in_use": 2.0, "loaded": true, "last_used_at": "2026-10-29T01:02:03Z", "tier": "interactive"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full info = %#v, want %#v", got, want)
	}
}

// testClient returns a Client whose URL file points at base.
func testClient(t *testing.T, base string) (*Client, string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "nimoos.url")
	if base != "" {
		if err := os.WriteFile(f, []byte(base+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Client{opts: Options{Client: "test", URLFile: f}}, f
}

func TestDoClassifiesResponses(t *testing.T) {
	type want struct {
		lease   *leaseWire
		is      []error
		isNot   []error
		anyErr  bool
		exactly error
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   want
	}{
		{"held", 200, `{"success":200,"data":{"id":"1-x","state":"held","device":"gpu.0","release_requested":true}}`,
			want{lease: &leaseWire{ID: "1-x", State: "held", Device: "gpu.0", ReleaseRequested: true}}},
		{"no content", 204, ``, want{lease: &leaseWire{}}},
		{"invalid budget", 400, `{"success":4000,"error_code":"invalid_budget"}`, want{exactly: ErrBudgetRejected}},
		{"invalid request", 400, `{"success":4000}`, want{exactly: ErrInvalidRequest}},
		{"invalid request no body", 400, ``, want{exactly: ErrInvalidRequest}},
		{"lease gone", 404, `{"success":90002}`, want{exactly: ErrLeaseGone}},
		{"plain 404", 404, `{}`, want{exactly: ErrNotFound}},
		{"disabled", 503, `{"success":90001}`, want{is: []error{errDisabled, ErrUnavailable}, isNot: []error{errTransient}}},
		{"500", 500, `{"success":500}`, want{is: []error{errTransient, ErrUnavailable}, isNot: []error{errNotSent}}},
		{"502", 502, `bad gateway`, want{is: []error{errTransient}, isNot: []error{errNotSent}}},
		{"409", 409, `{"success":409}`, want{is: []error{errRefused}, isNot: []error{errTransient}}},
		{"429", 429, ``, want{is: []error{errRefused}, isNot: []error{errTransient}}},
		{"envelope not success", 200, `{"success":500,"data":{"id":"1-x","state":"held"}}`,
			want{is: []error{ErrUnavailable}, isNot: []error{errTransient}}},
		{"empty id", 200, `{"success":200,"data":{"id":"","state":"held"}}`,
			want{is: []error{ErrUnavailable}, isNot: []error{errTransient}}},
		{"not json", 200, `<html>`, want{is: []error{ErrUnavailable}, isNot: []error{errTransient}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c, _ := testClient(t, srv.URL)
			lw, err := c.do(context.Background(), http.MethodPost, "/v1/resources/leases", map[string]any{"a": 1})
			checkErr(t, lw, err, tc.want.lease, tc.want.exactly, tc.want.is, tc.want.isNot)
		})
	}

	t.Run("url file missing", func(t *testing.T) {
		c, _ := testClient(t, "")
		lw, err := c.do(context.Background(), http.MethodGet, "/x", nil)
		checkErr(t, lw, err, nil, nil, []error{errNotSent, errTransient, ErrUnavailable}, nil)
	})
	t.Run("url file empty", func(t *testing.T) {
		c, f := testClient(t, "")
		if err := os.WriteFile(f, []byte(" \n"), 0o644); err != nil {
			t.Fatal(err)
		}
		lw, err := c.do(context.Background(), http.MethodGet, "/x", nil)
		checkErr(t, lw, err, nil, nil, []error{errNotSent, errTransient}, nil)
	})
	t.Run("dial refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		c, _ := testClient(t, "http://"+addr)
		lw, err := c.do(context.Background(), http.MethodPost, "/v1/resources/leases", map[string]any{})
		checkErr(t, lw, err, nil, nil, []error{errNotSent, errTransient}, nil)
	})
	t.Run("server hangs past ctx", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(release)
		c, _ := testClient(t, srv.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		lw, err := c.do(ctx, http.MethodGet, "/x", nil)
		checkErr(t, lw, err, nil, nil, []error{ErrUnavailable}, []error{errTransient, errNotSent})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to match context.DeadlineExceeded", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("do took %v past a 50 ms ctx", d)
		}
	})
	t.Run("http client timeout without ctx end", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(release)
		c, _ := testClient(t, srv.URL)
		c.opts.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
		lw, err := c.do(context.Background(), http.MethodGet, "/x", nil)
		checkErr(t, lw, err, nil, nil, []error{errTransient}, []error{errNotSent})
	})
}

func checkErr(t *testing.T, lw *leaseWire, err error, wantLease *leaseWire, exactly error, is, isNot []error) {
	t.Helper()
	if wantLease != nil {
		if err != nil {
			t.Fatalf("err = %v, want lease %+v", err, wantLease)
		}
		if !reflect.DeepEqual(lw, wantLease) {
			t.Fatalf("lease = %+v, want %+v", lw, wantLease)
		}
		return
	}
	if err == nil {
		t.Fatalf("err = nil (lease %+v), want an error", lw)
	}
	if lw != nil {
		t.Fatalf("lease = %+v alongside err %v", lw, err)
	}
	if exactly != nil && err != exactly {
		t.Fatalf("err = %v, want exactly %v", err, exactly)
	}
	for _, e := range is {
		if !errors.Is(err, e) {
			t.Fatalf("err = %v, want errors.Is %v", err, e)
		}
	}
	for _, e := range isNot {
		if errors.Is(err, e) {
			t.Fatalf("err = %v, must not be %v", err, e)
		}
	}
}

func TestErrorSentinelHierarchy(t *testing.T) {
	if !errors.Is(errNotSent, errTransient) || !errors.Is(errTransient, ErrUnavailable) {
		t.Fatal("errNotSent must wrap errTransient, which must wrap ErrUnavailable")
	}
	for _, e := range []error{errDisabled, errRefused} {
		if !errors.Is(e, ErrUnavailable) || errors.Is(e, errTransient) {
			t.Fatalf("%v: must be ErrUnavailable and not transient", e)
		}
	}
	for _, e := range []error{ErrNotFound, ErrLeaseGone, ErrBudgetRejected, ErrInvalidRequest} {
		if errors.Is(e, ErrUnavailable) {
			t.Fatalf("%v must not be ErrUnavailable", e)
		}
	}
}

func TestDoSendsJSONAndReReadsURLFile(t *testing.T) {
	type seen struct {
		server, method, ctype, body string
		contentLength               int64
	}
	var mu sync.Mutex
	var got []seen
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			got = append(got, seen{name, r.Method, r.Header.Get("Content-Type"), string(b), r.ContentLength})
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}
	a := httptest.NewServer(handler("a"))
	defer a.Close()
	b := httptest.NewServer(handler("b"))
	defer b.Close()

	c, f := testClient(t, a.URL)
	ctx := context.Background()
	if _, err := c.do(ctx, http.MethodPost, "/p", map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(b.URL+"/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.do(ctx, http.MethodDelete, "/d", nil); err != nil {
		t.Fatal(err)
	}
	// A typed nil map (releaseBody with nothing observed) is an empty body too.
	if _, err := c.do(ctx, http.MethodDelete, "/d", releaseBody(ReleaseInfo{})); err != nil {
		t.Fatal(err)
	}
	want := []seen{
		{"a", http.MethodPost, "application/json", `{"k":"v"}`, 9},
		{"b", http.MethodDelete, "application/json", ``, 0},
		{"b", http.MethodDelete, "application/json", ``, 0},
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %+v\nwant %+v", got, want)
	}
}

func TestDoBaseURLWinsOverURLFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := &Client{opts: Options{Client: "test", BaseURL: srv.URL + "/", URLFile: filepath.Join(t.TempDir(), "missing")}}
	if _, err := c.do(context.Background(), http.MethodGet, "/x", nil); err != nil {
		t.Fatalf("BaseURL ignored: %v", err)
	}
}
