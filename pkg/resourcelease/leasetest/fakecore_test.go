package leasetest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
)

type reply struct {
	status int
	env    map[string]any
	data   map[string]any
}

func call(t *testing.T, f *FakeCore, method, pathAndQuery string, body any) reply {
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.URL+pathAndQuery, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r.env); err != nil {
			t.Fatalf("%s %s: body %q: %v", method, pathAndQuery, raw, err)
		}
		r.data, _ = r.env["data"].(map[string]any)
	}
	return r
}

func TestFakeCoreLifecycle(t *testing.T) {
	f := New(t)
	b, err := os.ReadFile(f.URLFile)
	if err != nil || strings.TrimSpace(string(b)) != f.URL {
		t.Fatalf("url file = %q, %v; want %q", b, err, f.URL)
	}

	r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(map[string]any{"client": "photos", "task": "t", "device_pref": []string{"gpu.0"}, "wait_ms": 0}))
	if r.status != 200 || r.env["success"] != 200.0 || r.data["state"] != "held" || r.data["device"] != "gpu.0" {
		t.Fatalf("POST = %+v", r)
	}
	id, _ := r.data["id"].(string)
	if !strings.HasSuffix(id, "-fake") {
		t.Fatalf("id = %q, want <n>-fake", id)
	}
	if got := f.Live(); !reflect.DeepEqual(got, []string{id}) {
		t.Fatalf("Live = %v", got)
	}

	if r := call(t, f, http.MethodPost, "/v1/resources/leases/"+id+"/heartbeat", map[string]any{"in_use": 1, "loaded": true}); r.status != 200 || r.data["id"] != id || r.data["state"] != "held" {
		t.Fatalf("heartbeat = %+v", r)
	}
	if r := call(t, f, http.MethodDelete, "/v1/resources/leases/"+id, nil); r.status != 204 || r.env != nil {
		t.Fatalf("DELETE = %+v", r)
	}
	if len(f.Live()) != 0 {
		t.Fatalf("Live after DELETE = %v", f.Live())
	}
	if r := call(t, f, http.MethodGet, "/v1/resources/leases/"+id, nil); r.status != 404 || r.env["success"] != 90002.0 {
		t.Fatalf("GET after DELETE = %+v", r)
	}

	reqs := f.Requests()
	if len(reqs) != 4 {
		t.Fatalf("recorded %d requests: %+v", len(reqs), reqs)
	}
	if reqs[0].Method != http.MethodPost || reqs[0].Path != "/v1/resources/leases" || reqs[0].Body["client"] != "photos" {
		t.Fatalf("first request = %+v", reqs[0])
	}
	if reqs[2].Method != http.MethodDelete || reqs[2].Body != nil {
		t.Fatalf("empty DELETE recorded as %+v", reqs[2])
	}

	// A second lease gets the next number; Drop makes it vanish.
	r = call(t, f, http.MethodPost, "/v1/resources/leases", valid(map[string]any{"client": "ai"}))
	id2, _ := r.data["id"].(string)
	if id2 == id || r.data["device"] != "cpu" {
		t.Fatalf("second lease = %+v", r)
	}
	f.Drop(id2)
	if r := call(t, f, http.MethodPost, "/v1/resources/leases/"+id2+"/heartbeat", nil); r.status != 404 || r.env["success"] != 90002.0 {
		t.Fatalf("heartbeat after Drop = %+v", r)
	}
	if r := call(t, f, http.MethodDelete, "/v1/resources/leases/"+id2, nil); r.status != 404 || r.env["success"] != 90002.0 {
		t.Fatalf("DELETE after Drop = %+v", r)
	}
}

func TestFakeCoreQueueAndRelease(t *testing.T) {
	f := New(t)
	f.QueueNext(2)
	r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(map[string]any{"client": "parser", "wait_ms": 5000}))
	if r.status != 200 || r.data["state"] != "queued" {
		t.Fatalf("POST after QueueNext = %+v", r)
	}
	id := r.data["id"].(string)
	if got := f.Live(); !reflect.DeepEqual(got, []string{id}) {
		t.Fatalf("Live with a queued lease = %v", got)
	}
	start := time.Now()
	if r := call(t, f, http.MethodGet, "/v1/resources/leases/"+id+"?wait_ms=50", nil); r.data["state"] != "queued" {
		t.Fatalf("first GET = %+v", r)
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Fatalf("queued GET returned after %v, want a ~50 ms long-poll", d)
	}
	if r := call(t, f, http.MethodGet, "/v1/resources/leases/"+id+"?wait_ms=5000", nil); r.data["state"] != "held" {
		t.Fatalf("second GET = %+v", r)
	}
	// QueueNext applies to one POST only.
	if r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(nil)); r.data["state"] != "held" {
		t.Fatalf("POST after the queued one = %+v", r)
	}

	// wait_for=release blocks until RequestRelease.
	path := "/v1/resources/leases/" + id + "?wait_ms=5000&wait_for=release"
	done := make(chan reply, 1)
	go func() { done <- call(t, f, http.MethodGet, path, nil) }()
	select {
	case r := <-done:
		t.Fatalf("wait_for=release returned before RequestRelease: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	f.RequestRelease(id)
	select {
	case r := <-done:
		if r.status != 200 || r.data["release_requested"] != true || r.data["state"] != "held" {
			t.Fatalf("woken wait_for=release = %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait_for=release did not wake on RequestRelease")
	}
	if hb := call(t, f, http.MethodPost, "/v1/resources/leases/"+id+"/heartbeat", nil); hb.data["release_requested"] != true {
		t.Fatalf("heartbeat after RequestRelease = %+v", hb)
	}
	reqs := f.Requests()
	last := reqs[len(reqs)-2]
	if last.Query != "wait_ms=5000&wait_for=release" || last.Path != "/v1/resources/leases/"+id {
		t.Fatalf("recorded long-poll = %+v", last)
	}
}

func TestFakeCoreNeverGrantAndDropWakesPoll(t *testing.T) {
	f := New(t)
	f.QueueNext(0)
	id := call(t, f, http.MethodPost, "/v1/resources/leases", valid(nil)).data["id"].(string)
	for i := 0; i < 3; i++ {
		if r := call(t, f, http.MethodGet, "/v1/resources/leases/"+id, nil); r.data["state"] != "queued" {
			t.Fatalf("GET %d = %+v", i, r)
		}
	}
	done := make(chan reply, 1)
	go func() { done <- call(t, f, http.MethodGet, "/v1/resources/leases/"+id+"?wait_ms=5000", nil) }()
	time.Sleep(50 * time.Millisecond)
	f.Drop(id)
	select {
	case r := <-done:
		if r.status != 404 || r.env["success"] != 90002.0 {
			t.Fatalf("poll woken by Drop = %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Drop did not wake the long-poll")
	}
}

func TestFakeCoreHangAndRespond(t *testing.T) {
	f := New(t)
	f.Respond(func(r Recorded) (int, any, bool) {
		if r.Method == http.MethodPost && r.Path == "/v1/resources/leases" {
			return 400, map[string]any{"success": 4000, "error_code": "invalid_budget"}, true
		}
		return 0, nil, false
	})
	if r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(nil)); r.status != 400 || r.env["error_code"] != "invalid_budget" {
		t.Fatalf("overridden POST = %+v", r)
	}
	if len(f.Live()) != 0 {
		t.Fatal("an overridden POST must not create a lease")
	}
	if r := call(t, f, http.MethodGet, "/v1/resources/leases/nope", nil); r.status != 404 || r.env["success"] != 90002.0 {
		t.Fatalf("fallthrough GET = %+v", r)
	}
	if n := len(f.Requests()); n != 2 {
		t.Fatalf("recorded %d requests, want 2 (overridden ones included)", n)
	}
	f.Respond(nil)

	f.Hang(80 * time.Millisecond)
	start := time.Now()
	call(t, f, http.MethodPost, "/v1/resources/leases", valid(nil))
	if d := time.Since(start); d < 70*time.Millisecond {
		t.Fatalf("Hang ignored: %v", d)
	}
	f.Hang(0)
	// A hang far beyond the test must not block Cleanup's server close.
	f.Hang(time.Hour)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, f.URL+"/v1/resources/leases/x", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(30 * time.Millisecond)
}

func TestLeaseConstructor(t *testing.T) {
	l := Lease("3-fake", "gpu.0", resourcelease.StateHeld)
	if l.ID != "3-fake" || l.Device != "gpu.0" || !l.Usable() {
		t.Fatalf("Lease = %+v", l)
	}
	if state, rr := l.Snapshot(); state != resourcelease.StateHeld || rr {
		t.Fatalf("snapshot = %s %v", state, rr)
	}
}

func TestFakeCoreHoldQueuedPost(t *testing.T) {
	f := New(t)
	f.HoldQueuedPost(true)

	// The POST of a queued lease long-polls its wait_ms, then answers queued.
	f.QueueNext(0)
	start := time.Now()
	r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(map[string]any{"wait_ms": 150}))
	if d := time.Since(start); d < 140*time.Millisecond || r.data["state"] != resourcelease.StateQueued {
		t.Fatalf("held POST answered after %v: %+v", d, r)
	}
	// wait_ms 0 answers at once; a held POST answers at once too.
	f.QueueNext(0)
	start = time.Now()
	if r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(map[string]any{"wait_ms": 0})); r.data["state"] != resourcelease.StateQueued || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("wait_ms 0: %+v", r)
	}
	start = time.Now()
	if r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(map[string]any{"wait_ms": 5000})); r.data["state"] != resourcelease.StateHeld || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("held lease: %+v", r)
	}

	// Drop during the hold answers 404.
	f.QueueNext(0)
	go func() {
		time.Sleep(50 * time.Millisecond)
		for _, id := range f.Live() {
			if id == "4-fake" {
				f.Drop(id)
			}
		}
	}()
	if r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(map[string]any{"wait_ms": 5000})); r.status != 404 {
		t.Fatalf("dropped during hold: %+v", r)
	}

	// A client that goes away during the hold loses its queued lease.
	f.QueueNext(0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.URL+"/v1/resources/leases", strings.NewReader(validJSON(5000)))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("the hold must outlast the client")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if live := f.Live(); len(live) == 3 { // 1, 2 (queued) and 3 (held) remain
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("live = %v, want the abandoned 5-fake gone", f.Live())
}

func TestFakeCoreCreatesNothingForAGoneClient(t *testing.T) {
	f := New(t)
	// The Respond hook outlasts the client and then falls through.
	done := make(chan struct{})
	f.Respond(func(Recorded) (int, any, bool) {
		time.Sleep(200 * time.Millisecond)
		close(done)
		return 0, nil, false
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.URL+"/v1/resources/leases", strings.NewReader(validJSON(-1)))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	<-done
	time.Sleep(50 * time.Millisecond)
	if live := f.Live(); len(live) != 0 {
		t.Fatalf("live = %v", live)
	}
}

// valid is a POST body the core accepts, with extra fields set over it.
func valid(extra map[string]any) map[string]any {
	b := map[string]any{"client": "photos", "task": "t", "class": "light", "kind": "task",
		"priority": 1, "tier": "", "device_pref": []string{"cpu"}, "allow_fallback": false,
		"est_vram_mb": 0, "est_ram_mb": 0}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// validJSON is valid(nil) as JSON with wait_ms set (omitted when < 0).
func validJSON(waitMs int) string {
	b := valid(nil)
	if waitMs >= 0 {
		b["wait_ms"] = waitMs
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

func TestFakeCoreRejectsWhatTheCoreRejects(t *testing.T) {
	f := New(t)
	for name, extra := range map[string]map[string]any{
		"priority above 2":     {"priority": 50},
		"priority below 0":     {"priority": -1},
		"no device_pref":       {"device_pref": []string{}},
		"resident without pid": {"kind": "resident", "class": "memory"},
		"heavy resident":       {"kind": "resident", "class": "heavy", "pid": 7},
		"loaded task":          {"already_loaded": true},
		"running without pid":  {"already_running": true},
		"running resident":     {"kind": "resident", "pid": 7, "already_running": true},
		"unknown class":        {"class": "huge"},
		"unknown kind":         {"kind": "daemon"},
		"unknown tier":         {"tier": "urgent"},
		"no client":            {"client": ""},
		"estimate too large":   {"est_ram_mb": 65537},
	} {
		r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(extra))
		if r.status != 400 || r.env["success"] != 4000.0 || r.env["error_code"] != nil {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if live := f.Live(); len(live) != 0 {
		t.Fatalf("rejected requests created leases %v", live)
	}
	for name, extra := range map[string]map[string]any{
		"resident with pid":  {"kind": "resident", "class": "memory", "pid": 7, "already_loaded": true},
		"running task":       {"already_running": true, "pid": 7},
		"interactive prio 0": {"priority": 0, "tier": "interactive"},
		"background prio 2":  {"priority": 2, "tier": "maintenance"},
	} {
		if r := call(t, f, http.MethodPost, "/v1/resources/leases", valid(extra)); r.status != 200 {
			t.Errorf("%s: %+v", name, r)
		}
	}
}
