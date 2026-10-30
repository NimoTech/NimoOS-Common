// Package leasetest provides an in-process fake of the core resource
// scheduler (/v1/resources/leases) for tests of resourcelease clients.
package leasetest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
)

const (
	leasesPath  = "/v1/resources/leases"
	maxLongPoll = 30 * time.Second
)

// Recorded is one request the fake core received. Body is nil when the
// request had no body (or a body that is not a JSON object).
type Recorded struct {
	Method, Path, Query string
	Body                map[string]any
}

type fakeLease struct {
	id               string
	seq              int
	state            string
	device           string
	releaseRequested bool
	grantAfterPolls  int // queued only: grant on this many GETs; 0 = never
	polls            int
}

// FakeCore is an httptest server speaking the lease protocol of the core:
//
//	POST   /v1/resources/leases               -> 200 envelope, held (or queued after QueueNext)
//	GET    /v1/resources/leases/{id}           -> 200 envelope; wait_ms long-polls a queued lease,
//	                                              wait_ms&wait_for=release long-polls a held one
//	POST   /v1/resources/leases/{id}/heartbeat -> 200 envelope
//	DELETE /v1/resources/leases/{id}           -> 204
//
// Unknown ids answer 404 {"success":90002}. Lease ids look like "<n>-fake".
// A POST body the core would refuse (priority outside 0..2, empty
// device_pref, a resident without pid, an unknown class/kind/tier, ...)
// answers 400 {"success":4000} like the core; budgets are not checked.
// Unlike the real core, a queued POST answers at once even with wait_ms > 0,
// unless HoldQueuedPost(true) is set.
type FakeCore struct {
	URL     string // base URL of the server
	URLFile string // <t.TempDir()>/nimoos.url holding URL

	srv  *httptest.Server
	done chan struct{} // closed on cleanup: ends every hang and long-poll

	mu        sync.Mutex
	changed   chan struct{} // closed and replaced on every lease change
	requests  []Recorded
	leases    map[string]*fakeLease
	next      int
	queueNext []int // grantAfterPolls of the next POSTs
	hang      time.Duration
	holdPost  bool
	respond   func(r Recorded) (status int, body any, handled bool)
}

// New starts a fake core, writes its URL to <t.TempDir()>/nimoos.url and
// closes it on t.Cleanup.
func New(t testing.TB) *FakeCore {
	t.Helper()
	f := &FakeCore{
		done:    make(chan struct{}),
		changed: make(chan struct{}),
		leases:  map[string]*fakeLease{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.URL = f.srv.URL
	f.URLFile = filepath.Join(t.TempDir(), "nimoos.url")
	if err := os.WriteFile(f.URLFile, []byte(f.URL+"\n"), 0o644); err != nil {
		f.srv.Close()
		t.Fatalf("leasetest: write url file: %v", err)
	}
	t.Cleanup(func() {
		close(f.done)
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

// Requests returns a copy of every request received so far, in order.
func (f *FakeCore) Requests() []Recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Recorded, len(f.requests))
	copy(out, f.requests)
	return out
}

// Live returns the ids currently held or queued, oldest first.
func (f *FakeCore) Live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	live := make([]*fakeLease, 0, len(f.leases))
	for _, l := range f.leases {
		live = append(live, l)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].seq < live[j].seq })
	ids := make([]string, len(live))
	for i, l := range live {
		ids[i] = l.id
	}
	return ids
}

// QueueNext makes the next POST answer queued; the lease is granted on the
// grantAfterPolls-th GET of it (0 = never). Calls stack, one per POST.
func (f *FakeCore) QueueNext(grantAfterPolls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queueNext = append(f.queueNext, grantAfterPolls)
}

// Drop makes a lease vanish: later calls on it get 404 {"success":90002}
// and pending long-polls on it end with that answer.
func (f *FakeCore) Drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.leases[id]; ok {
		delete(f.leases, id)
		f.notifyLocked()
	}
}

// RequestRelease sets release_requested on a lease and wakes a pending
// wait_for=release long-poll.
func (f *FakeCore) RequestRelease(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.leases[id]; ok {
		l.releaseRequested = true
		f.notifyLocked()
	}
}

// Hang makes every handler sleep d before answering (0 = off). The sleep
// ends early when the client goes away or the fake core is closed.
func (f *FakeCore) Hang(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang = d
}

// HoldQueuedPost makes a POST that the fake core queues long-poll like the
// real core: it answers when the lease changes (granted, dropped) or after
// the body's wait_ms (capped at 30 s), whichever comes first. A client that
// goes away during the hold has its queued lease deleted, as the core does.
func (f *FakeCore) HoldQueuedPost(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdPost = on
}

// Respond installs an override hook run for every request after it is
// recorded (and after any Hang). When it reports handled, the fake core
// answers status with body JSON-encoded (nil body = no body) and does
// nothing else. Respond(nil) removes the hook.
func (f *FakeCore) Respond(fn func(r Recorded) (status int, body any, handled bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = fn
}

// Lease builds a lease as Acquire would return it, for tests that need one
// without a client. It has no owning client.
func Lease(id, device, state string) *resourcelease.Lease {
	return &resourcelease.Lease{ID: id, Device: device, State: state}
}

// notifyLocked wakes every long-poll; f.mu must be held.
func (f *FakeCore) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *FakeCore) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	rec := Recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
	if len(raw) > 0 {
		var body map[string]any
		if json.Unmarshal(raw, &body) == nil {
			rec.Body = body
		}
	}

	f.mu.Lock()
	f.requests = append(f.requests, rec)
	hang, respond := f.hang, f.respond
	f.mu.Unlock()

	if hang > 0 {
		t := time.NewTimer(hang)
		select {
		case <-t.C:
		case <-r.Context().Done():
		case <-f.done:
		}
		t.Stop()
		if r.Context().Err() != nil {
			return
		}
	}
	if respond != nil {
		if status, body, handled := respond(rec); handled {
			writeJSON(w, status, body)
			return
		}
	}
	if r.Context().Err() != nil {
		return // the client went away: create nothing, answer nothing
	}

	switch {
	case r.Method == http.MethodPost && rec.Path == leasesPath:
		f.create(w, r, rec)
	case strings.HasPrefix(rec.Path, leasesPath+"/"):
		rest := strings.TrimPrefix(rec.Path, leasesPath+"/")
		switch {
		case r.Method == http.MethodGet && !strings.Contains(rest, "/"):
			f.get(w, r, rest)
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/heartbeat") && strings.Count(rest, "/") == 1:
			f.heartbeat(w, strings.TrimSuffix(rest, "/heartbeat"))
		case r.Method == http.MethodDelete && !strings.Contains(rest, "/"):
			f.delete(w, rest)
		default:
			writeJSON(w, http.StatusNotFound, map[string]any{"success": http.StatusNotFound, "message": "no such route"})
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"success": http.StatusNotFound, "message": "no such route"})
	}
}

func (f *FakeCore) create(w http.ResponseWriter, r *http.Request, rec Recorded) {
	if msg := invalidRequest(rec.Body); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": invalidParams, "message": "invalid request: " + msg})
		return
	}
	device := "cpu"
	if prefs, ok := rec.Body["device_pref"].([]any); ok && len(prefs) > 0 {
		if d, ok := prefs[0].(string); ok && d != "" {
			device = d
		}
	}
	f.mu.Lock()
	f.next++
	l := &fakeLease{id: strconv.Itoa(f.next) + "-fake", seq: f.next, state: resourcelease.StateHeld, device: device}
	if len(f.queueNext) > 0 {
		l.state, l.grantAfterPolls = resourcelease.StateQueued, f.queueNext[0]
		f.queueNext = f.queueNext[1:]
	}
	f.leases[l.id] = l
	f.notifyLocked()
	data := l.wire()
	hold := f.holdPost && l.state == resourcelease.StateQueued
	f.mu.Unlock()
	if hold {
		if ms, ok := rec.Body["wait_ms"].(float64); ok && ms > 0 {
			var answered bool
			if data, answered = f.holdQueued(w, r, l.id, min(time.Duration(ms)*time.Millisecond, maxLongPoll)); !answered {
				return
			}
		}
	}
	writeOK(w, data)
}

// holdQueued long-polls a freshly queued lease for up to wait. It returns
// the lease to answer, or false when nothing is to be answered: the client
// went away (its queued lease is deleted), the fake core closed, or the
// lease vanished (a 404 has then been written).
func (f *FakeCore) holdQueued(w http.ResponseWriter, r *http.Request, id string, wait time.Duration) (map[string]any, bool) {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		f.mu.Lock()
		l, ok := f.leases[id]
		if !ok {
			f.mu.Unlock()
			writeGone(w)
			return nil, false
		}
		if l.state != resourcelease.StateQueued {
			data := l.wire()
			f.mu.Unlock()
			return data, true
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-changed:
		case <-t.C:
			f.mu.Lock()
			l, ok := f.leases[id]
			var data map[string]any
			if ok {
				data = l.wire()
			}
			f.mu.Unlock()
			if !ok {
				writeGone(w)
			}
			return data, ok
		case <-r.Context().Done():
			f.mu.Lock()
			if _, ok := f.leases[id]; ok {
				delete(f.leases, id)
				f.notifyLocked()
			}
			f.mu.Unlock()
			return nil, false
		case <-f.done:
			return nil, false
		}
	}
}

func (f *FakeCore) get(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	wait := time.Duration(0)
	if ms, err := strconv.Atoi(q.Get("wait_ms")); err == nil && ms > 0 {
		wait = min(time.Duration(ms)*time.Millisecond, maxLongPoll)
	}
	forRelease := q.Get("wait_for") == "release"

	f.mu.Lock()
	l, ok := f.leases[id]
	if !ok {
		f.mu.Unlock()
		writeGone(w)
		return
	}
	// As on the core, wait_for=release only changes the wait of a held
	// lease; a queued one keeps the grant long-poll.
	forRelease = forRelease && l.state != resourcelease.StateQueued
	if l.state == resourcelease.StateQueued {
		l.polls++
		if l.grantAfterPolls > 0 && l.polls >= l.grantAfterPolls {
			l.state = resourcelease.StateHeld
			f.notifyLocked()
		}
	}
	f.mu.Unlock()

	// ready reports whether the long-poll is over; f.mu must be held.
	ready := func(l *fakeLease) bool {
		if l.state == resourcelease.StateQueued {
			return false
		}
		return !forRelease || l.releaseRequested
	}
	var deadline <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		deadline = t.C
	}
	for {
		f.mu.Lock()
		l, ok := f.leases[id]
		if !ok {
			f.mu.Unlock()
			writeGone(w)
			return
		}
		if wait == 0 || ready(l) {
			data := l.wire()
			f.mu.Unlock()
			writeOK(w, data)
			return
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			wait = 0 // answer the current state on the next loop
		case <-r.Context().Done():
			return
		case <-f.done:
			return
		}
	}
}

func (f *FakeCore) heartbeat(w http.ResponseWriter, id string) {
	f.mu.Lock()
	l, ok := f.leases[id]
	var data map[string]any
	if ok {
		data = l.wire()
	}
	f.mu.Unlock()
	if !ok {
		writeGone(w)
		return
	}
	writeOK(w, data)
}

func (f *FakeCore) delete(w http.ResponseWriter, id string) {
	f.mu.Lock()
	_, ok := f.leases[id]
	if ok {
		delete(f.leases, id)
		f.notifyLocked()
	}
	f.mu.Unlock()
	if !ok {
		writeGone(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// invalidParams is common_err.INVALID_PARAMS, the success code of a 400.
const invalidParams = 4000

// maxEstMB is the core's cap on est_vram_mb / est_ram_mb.
const maxEstMB = 65536

// invalidRequest mirrors the request checks of the core's admission
// controller (pkg/resources Controller.validate) that do not depend on its
// state: it returns why the core would answer 400, or "" when it accepts
// the body. Budget validation and unknown devices are not checked.
func invalidRequest(b map[string]any) string {
	str := func(k string) string { v, _ := b[k].(string); return v }
	num := func(k string) float64 { v, _ := b[k].(float64); return v }
	flag := func(k string) bool { v, _ := b[k].(bool); return v }
	switch str("tier") {
	case "", "interactive", "user_batch", "auto_batch", "maintenance":
	default:
		return "tier " + strconv.Quote(str("tier"))
	}
	switch str("kind") {
	case "", resourcelease.KindTask:
		if flag("already_loaded") {
			return "already_loaded requires kind resident"
		}
		if flag("already_running") && num("pid") <= 0 {
			return "already_running requires pid"
		}
	case resourcelease.KindResident:
		switch {
		case flag("already_running"):
			return "already_running requires kind task"
		case num("pid") <= 0:
			return "resident lease requires pid"
		case str("class") == resourcelease.ClassHeavy:
			return "resident lease cannot be heavy"
		}
	default:
		return "kind " + strconv.Quote(str("kind"))
	}
	prefs, _ := b["device_pref"].([]any)
	prio, isNum := b["priority"].(float64)
	switch class := str("class"); {
	case str("client") == "":
		return "client required"
	case class != resourcelease.ClassHeavy && class != resourcelease.ClassLight && class != resourcelease.ClassMemory:
		return "class " + strconv.Quote(class)
	case !isNum || prio < 0 || prio > 2:
		return "priority " + strconv.FormatFloat(prio, 'g', -1, 64)
	case len(prefs) == 0:
		return "device_pref required"
	case num("est_vram_mb") > maxEstMB || num("est_ram_mb") > maxEstMB:
		return "estimate too large"
	}
	return ""
}

func (l *fakeLease) wire() map[string]any {
	return map[string]any{"id": l.id, "state": l.state, "device": l.device, "release_requested": l.releaseRequested}
}

func writeOK(w http.ResponseWriter, data map[string]any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": http.StatusOK, "message": "ok", "data": data})
}

func writeGone(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"success": resourcelease.CodeLeaseNotFound, "message": "lease not found"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	b, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
