package resourcelease_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	rl "github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
	"github.com/NimoTech/NimoOS-Common/pkg/resourcelease/leasetest"
)

// handoffOpts is photosOpts with a handoff file in its own temp dir.
func handoffOpts(t *testing.T, f *leasetest.FakeCore) rl.Options {
	opts := photosOpts(f)
	opts.HandoffFile = filepath.Join(t.TempDir(), "resource-leases-photos.json")
	return opts
}

type handoffDoc struct {
	Version   int               `json:"version"`
	Client    string            `json:"client"`
	WrittenAt string            `json:"written_at"`
	Leases    []rl.HandoffEntry `json:"leases"`
}

func readHandoffDoc(t *testing.T, path string) handoffDoc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read handoff file: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatalf("handoff file is not JSON: %v (%s)", err, b)
	}
	want := []string{"client", "leases", "version", "written_at"}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("handoff keys = %v, want %v", got, want)
	}
	var doc handoffDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func writeHandoffDoc(t *testing.T, path string, entries ...rl.HandoffEntry) {
	t.Helper()
	b, err := json.Marshal(handoffDoc{Version: 1, Client: "photos", WrittenAt: "2026-10-29T01:02:03Z", Leases: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func entryIDs(es []rl.HandoffEntry) []string {
	ids := make([]string, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.ID)
	}
	sort.Strings(ids)
	return ids
}

func deletedIDs(f *leasetest.FakeCore) []string {
	var ids []string
	for _, r := range deletes(f) {
		ids = append(ids, strings.TrimPrefix(r.Path, leasesPath+"/"))
	}
	sort.Strings(ids)
	return ids
}

func sorted(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func TestCloseReleasesHeldAndQueuedConcurrently(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	a, b := held(t, c), held(t, c)

	f.QueueNext(0) // never granted
	done := make(chan rl.Result, 1)
	go func() { done <- c.Acquire(context.Background(), req(-1), rl.WithPollWait(10*time.Second)) }()
	eventually(t, 2*time.Second, func() bool { return len(gets(f)) == 1 }, "queued acquire never long-polled")

	// Each DELETE takes 300 ms: three in sequence would take 900 ms.
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodDelete {
			time.Sleep(300 * time.Millisecond)
		}
		return 0, nil, false
	})
	start := time.Now()
	rep := c.Close(context.Background())
	elapsed := time.Since(start)

	if want := sorted(a.ID, b.ID, "3-fake"); !reflect.DeepEqual(rep.Released, want) {
		t.Fatalf("released = %v, want %v (report %+v)", rep.Released, want, rep)
	}
	if rep.CancelledQueued != 1 || len(rep.Failed) != 0 || len(rep.Kept) != 0 || rep.HandoffErr != nil {
		t.Fatalf("report %+v", rep)
	}
	if elapsed > 750*time.Millisecond {
		t.Fatalf("Close took %v: DELETEs are not concurrent", elapsed)
	}
	select {
	case res := <-done:
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonClosed {
			t.Fatalf("blocked acquire = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked acquire did not return after Close")
	}
	time.Sleep(50 * time.Millisecond)
	if got := deletedIDs(f); !reflect.DeepEqual(got, sorted(a.ID, b.ID, "3-fake")) {
		t.Fatalf("DELETEs = %v: the closed Acquire must leave its DELETE to Close", got)
	}
	if live := f.Live(); len(live) != 0 {
		t.Fatalf("live after Close: %v", live)
	}
	for _, l := range []*rl.Lease{a, b} {
		if s, _ := l.Snapshot(); s != rl.StateReleased {
			t.Fatalf("lease %s state %q after Close", l.ID, s)
		}
	}
}

// detachedTransport sends every request even after its context ended,
// as when the answer is already on its way when Close cancels the call.
type detachedTransport struct{ base http.RoundTripper }

func (d detachedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return d.base.RoundTrip(r.WithContext(context.WithoutCancel(r.Context())))
}

func TestCloseDeletesALateGrant(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	opts.HTTPClient = &http.Client{Timeout: 35 * time.Second, Transport: detachedTransport{base: &http.Transport{}}}
	c := newClient(t, opts)

	posted, answer := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodPost && r.Path == leasesPath {
			once.Do(func() { close(posted) })
			<-answer // answer (held) only after Close
		}
		return 0, nil, false
	})
	done := make(chan rl.Result, 1)
	go func() { done <- c.Acquire(context.Background(), req(-1)) }()
	<-posted

	rep := c.Close(context.Background())
	if len(rep.Released)+len(rep.Failed)+len(rep.Kept) != 0 {
		t.Fatalf("nothing was registered yet: %+v", rep)
	}
	close(answer)

	select {
	case res := <-done:
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonClosed {
			t.Fatalf("late grant result = %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("acquire did not return")
	}
	eventually(t, 2*time.Second, func() bool { return len(deletes(f)) == 1 }, "the late grant was not deleted")
	if got := deletedIDs(f); !reflect.DeepEqual(got, []string{"1-fake"}) {
		t.Fatalf("DELETEs = %v", got)
	}
	if live := f.Live(); len(live) != 0 {
		t.Fatalf("orphan lease left on the core: %v", live)
	}
}

func TestCloseKeepsKeepOnCloseLeases(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	r := rl.Request{Task: "clip", DevicePref: []string{"cpu"}, Kind: rl.KindResident, Workload: "resident:clip_textual", PID: 1234, MaxWait: -1}
	kept := c.Acquire(context.Background(), r).Lease
	other := held(t, c)
	c.KeepOnClose(kept)

	rep := c.Close(context.Background())
	if !reflect.DeepEqual(rep.Kept, []string{kept.ID}) || !reflect.DeepEqual(rep.Released, []string{other.ID}) || rep.HandoffErr != nil {
		t.Fatalf("report %+v", rep)
	}
	if got := deletedIDs(f); !reflect.DeepEqual(got, []string{other.ID}) {
		t.Fatalf("DELETEs = %v: the kept lease must stay", got)
	}
	if s, _ := kept.Snapshot(); s != rl.StateHeld {
		t.Fatalf("kept lease state %q", s)
	}

	doc := readHandoffDoc(t, opts.HandoffFile)
	if doc.Version != 1 || doc.Client != "photos" {
		t.Fatalf("doc %+v", doc)
	}
	if ts, err := time.Parse(time.RFC3339, doc.WrittenAt); err != nil || time.Since(ts) > time.Minute || !strings.HasSuffix(doc.WrittenAt, "Z") {
		t.Fatalf("written_at %q: %v", doc.WrittenAt, err)
	}
	want := []rl.HandoffEntry{{ID: kept.ID, Workload: "resident:clip_textual", PID: 1234, Device: "cpu"}}
	if !reflect.DeepEqual(doc.Leases, want) {
		t.Fatalf("leases = %+v, want %+v", doc.Leases, want)
	}
	st, err := os.Stat(opts.HandoffFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v: %v", st.Mode(), err)
	}
	ents, _ := os.ReadDir(filepath.Dir(opts.HandoffFile))
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
}

// KeepOnClose works by lease id: a clone (Photos hands clones around)
// marks the registered original, also while other goroutines use it.
func TestKeepOnCloseOnACloneKeepsTheOriginal(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	orig := held(t, c)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.Heartbeat(context.Background(), orig)
			_, _ = orig.Snapshot()
		}
	}()
	clone := orig.Clone()
	c.KeepOnClose(clone)
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()

	rep := c.Close(context.Background())
	if !reflect.DeepEqual(rep.Kept, []string{orig.ID}) || len(rep.Released) != 0 {
		t.Fatalf("report %+v", rep)
	}
	if n := len(deletes(f)); n != 0 {
		t.Fatalf("%d DELETEs: the original must not be deleted", n)
	}
	doc := readHandoffDoc(t, opts.HandoffFile)
	if want := []rl.HandoffEntry{{ID: orig.ID, Workload: "w", PID: 42, Device: "gpu.1"}}; !reflect.DeepEqual(doc.Leases, want) {
		t.Fatalf("leases %+v", doc.Leases)
	}
}

func TestKeepOnCloseMarkGoesWithTheLease(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	l := held(t, c)
	c.KeepOnClose(l)
	c.Release(context.Background(), l) // released afterwards: no longer kept
	c.KeepOnClose(&rl.Lease{FailOpen: true, State: rl.StateHeld})
	c.KeepOnClose(nil)

	rep := c.Close(context.Background())
	if len(rep.Kept) != 0 || len(rep.Released) != 0 {
		t.Fatalf("report %+v", rep)
	}
	if _, err := os.Stat(opts.HandoffFile); !os.IsNotExist(err) {
		t.Fatalf("handoff file written without kept leases: %v", err)
	}
}

func TestCloseBoundedAgainstHungCore(t *testing.T) {
	t.Run("CloseTimeout", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f)) // CloseTimeout 0 = 3 s
		a, b := held(t, c), held(t, c)
		f.Hang(10 * time.Second)
		start := time.Now()
		rep := c.Close(context.Background())
		elapsed := time.Since(start)
		if elapsed > 3500*time.Millisecond || elapsed < 2500*time.Millisecond {
			t.Fatalf("Close took %v, want about 3 s", elapsed)
		}
		if want := sorted(a.ID, b.ID); !reflect.DeepEqual(rep.Failed, want) || len(rep.Released) != 0 {
			t.Fatalf("report %+v", rep)
		}
	})
	t.Run("ctx deadline first", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		a := held(t, c)
		f.Hang(10 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		rep := c.Close(ctx)
		if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
			t.Fatalf("Close took %v past a 300 ms ctx", elapsed)
		}
		if !reflect.DeepEqual(rep.Failed, []string{a.ID}) {
			t.Fatalf("report %+v", rep)
		}
	})
}

func TestCloseIsIdempotentAndAcquireAfterCloseFailsOpen(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	if rep := c.Close(context.Background()); !reflect.DeepEqual(rep.Released, []string{l.ID}) {
		t.Fatalf("first Close %+v", rep)
	}
	n := len(f.Requests())
	if rep := c.Close(context.Background()); !reflect.DeepEqual(rep, rl.CloseReport{}) {
		t.Fatalf("second Close %+v", rep)
	}
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonClosed || res.Lease == nil {
		t.Fatalf("acquire after Close %+v", res)
	}
	c.Release(context.Background(), l)
	c.ReleaseWith(context.Background(), l, rl.ReleaseInfo{ObservedPeakMB: 5})
	c.Heartbeat(context.Background(), l)
	if got := len(f.Requests()); got != n {
		t.Fatalf("%d requests after Close, want none", got-n)
	}
	if s, _ := l.Snapshot(); s != rl.StateReleased {
		t.Fatalf("state %q", s)
	}
}

func TestCloseStopsHolds(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	kept := held(t, c) // stays held: only Close's cancel can stop its hold
	c.KeepOnClose(kept)
	stop := c.Hold(l, 15*time.Millisecond, nil, nil)
	defer stop()
	stopKept := c.Hold(kept, 15*time.Millisecond, nil, nil)
	defer stopKept()
	eventually(t, 2*time.Second, func() bool { return len(heartbeats(f)) >= 4 }, "no heartbeats")
	start := time.Now()
	c.Close(context.Background())
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v waiting for holds", d)
	}
	n := len(heartbeats(f))
	time.Sleep(100 * time.Millisecond)
	if got := len(heartbeats(f)); got != n {
		t.Fatalf("%d heartbeats after Close returned", got-n)
	}
	// A Hold started after Close (here on a lease built by hand) does nothing.
	stop2 := c.Hold(leasetest.Lease("99-fake", "cpu", rl.StateHeld), 10*time.Millisecond, nil, nil)
	time.Sleep(40 * time.Millisecond)
	stop2()
	if got := len(heartbeats(f)); got != n {
		t.Fatal("Hold after Close sent heartbeats")
	}
}

func TestCloseReleasesOthersWhenHandoffWriteFails(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	logs := observed(&opts)
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	opts.HandoffFile = filepath.Join(ro, "resource-leases-photos.json")
	if os.Geteuid() == 0 { // root writes into a read-only dir: use a file as the parent
		parent := filepath.Join(dir, "plain")
		if err := os.WriteFile(parent, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		opts.HandoffFile = filepath.Join(parent, "resource-leases-photos.json")
	}
	c := newClient(t, opts)
	kept, other := held(t, c), held(t, c)
	c.KeepOnClose(kept)

	rep := c.Close(context.Background())
	if rep.HandoffErr == nil {
		t.Fatal("HandoffErr must report the failed write")
	}
	if !reflect.DeepEqual(rep.Kept, []string{kept.ID}) || !reflect.DeepEqual(rep.Released, []string{other.ID}) {
		t.Fatalf("report %+v", rep)
	}
	if got := deletedIDs(f); !reflect.DeepEqual(got, []string{other.ID}) {
		t.Fatalf("DELETEs = %v", got)
	}
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 1 {
		t.Fatalf("%d Error logs, want 1", n)
	}
	ents, _ := os.ReadDir(ro)
	if len(ents) != 0 {
		t.Fatalf("files left in the read-only dir: %v", ents)
	}
}

func TestCloseMergesUnprocessedHandoff(t *testing.T) {
	t.Run("ReleaseHandoff never ran", func(t *testing.T) {
		f := leasetest.New(t)
		opts := handoffOpts(t, f)
		c := newClient(t, opts)
		l := held(t, c)
		writeHandoffDoc(t, opts.HandoffFile,
			rl.HandoffEntry{ID: "old-1", Workload: "resident:a", PID: 7, Device: "gpu.0"},
			rl.HandoffEntry{ID: l.ID, Workload: "stale", PID: 7, Device: "cpu"}, // same id: the new entry wins
		)
		c.KeepOnClose(l)
		rep := c.Close(context.Background())
		if rep.HandoffErr != nil {
			t.Fatal(rep.HandoffErr)
		}
		doc := readHandoffDoc(t, opts.HandoffFile)
		if got := entryIDs(doc.Leases); !reflect.DeepEqual(got, sorted("old-1", l.ID)) {
			t.Fatalf("leases %+v", doc.Leases)
		}
		for _, e := range doc.Leases {
			if e.ID == l.ID && (e.Workload != "w" || e.PID != 42 || e.Device != "gpu.1") {
				t.Fatalf("own entry %+v", e)
			}
			if e.ID == "old-1" && (e.Workload != "resident:a" || e.PID != 7 || e.Device != "gpu.0") {
				t.Fatalf("old entry %+v", e)
			}
		}
	})
	t.Run("after ReleaseHandoff only own entries", func(t *testing.T) {
		f := leasetest.New(t)
		opts := handoffOpts(t, f)
		c := newClient(t, opts)
		l := held(t, c)
		writeHandoffDoc(t, opts.HandoffFile, rl.HandoffEntry{ID: "old-1"})
		c.ReleaseHandoff(context.Background())
		writeHandoffDoc(t, opts.HandoffFile, rl.HandoffEntry{ID: "old-2"}) // e.g. a stray rewrite
		c.KeepOnClose(l)
		c.Close(context.Background())
		doc := readHandoffDoc(t, opts.HandoffFile)
		if got := entryIDs(doc.Leases); !reflect.DeepEqual(got, []string{l.ID}) {
			t.Fatalf("leases %+v", doc.Leases)
		}
	})
	t.Run("corrupt old file is overwritten", func(t *testing.T) {
		f := leasetest.New(t)
		opts := handoffOpts(t, f)
		c := newClient(t, opts)
		l := held(t, c)
		if err := os.WriteFile(opts.HandoffFile, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.KeepOnClose(l)
		if rep := c.Close(context.Background()); rep.HandoffErr != nil {
			t.Fatal(rep.HandoffErr)
		}
		if got := entryIDs(readHandoffDoc(t, opts.HandoffFile).Leases); !reflect.DeepEqual(got, []string{l.ID}) {
			t.Fatalf("leases %v", got)
		}
	})
}

// A ReleaseHandoff still deleting old ids must not remove the file a
// concurrent Close wrote.
func TestReleaseHandoffKeepsTheFileCloseWrote(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	l := held(t, c)
	writeHandoffDoc(t, opts.HandoffFile, rl.HandoffEntry{ID: "old-1"})
	inDelete, unblock := make(chan struct{}), make(chan struct{})
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.Path, "/old-1") {
			close(inDelete)
			<-unblock
		}
		return 0, nil, false
	})
	done := make(chan rl.HandoffReport, 1)
	go func() { done <- c.ReleaseHandoff(context.Background()) }()
	<-inDelete
	c.KeepOnClose(l)
	if rep := c.Close(context.Background()); rep.HandoffErr != nil {
		t.Fatal(rep.HandoffErr)
	}
	close(unblock)
	rep := <-done
	if !reflect.DeepEqual(rep.Released, []string{"old-1"}) || rep.Err != nil {
		t.Fatalf("handoff report %+v", rep)
	}
	if got := entryIDs(readHandoffDoc(t, opts.HandoffFile).Leases); !reflect.DeepEqual(got, []string{l.ID}) {
		t.Fatalf("leases %v", got)
	}
}

// A ReleaseHandoff after Close (the 60 s fallback racing SIGTERM) must not
// delete the leases Close just left to the successor.
func TestReleaseHandoffAfterCloseKeepsTheKeptLeases(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	l := held(t, c)
	c.KeepOnClose(l)
	if rep := c.Close(context.Background()); rep.HandoffErr != nil || !reflect.DeepEqual(rep.Kept, []string{l.ID}) {
		t.Fatalf("close report %+v", rep)
	}
	rep := c.ReleaseHandoff(context.Background())
	if len(rep.Released)+len(rep.Skipped)+len(rep.Failed) != 0 || rep.Err != nil {
		t.Fatalf("handoff report %+v", rep)
	}
	if n := len(deletes(f)); n != 0 {
		t.Fatalf("%d DELETEs after Close", n)
	}
	if got := entryIDs(readHandoffDoc(t, opts.HandoffFile).Leases); !reflect.DeepEqual(got, []string{l.ID}) {
		t.Fatalf("leases %v", got)
	}

	// Closed without a file of its own: an old file is left for the next
	// process too.
	f2 := leasetest.New(t)
	opts2 := handoffOpts(t, f2)
	c2 := newClient(t, opts2)
	writeHandoffDoc(t, opts2.HandoffFile, rl.HandoffEntry{ID: "old-1"})
	c2.Close(context.Background())
	if rep := c2.ReleaseHandoff(context.Background()); len(rep.Released) != 0 || len(f2.Requests()) != 0 {
		t.Fatalf("closed client released the old file: %+v", rep)
	}
	if got := entryIDs(readHandoffDoc(t, opts2.HandoffFile).Leases); !reflect.DeepEqual(got, []string{"old-1"}) {
		t.Fatalf("leases %v", got)
	}
}

// A queued lease granted while Close is still deleting it: Acquire must not
// hand it out as held, and the lease must go.
func TestAcquireGrantedDuringCloseFailsOpenClosed(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	opts.HTTPClient = &http.Client{Timeout: 35 * time.Second, Transport: detachedTransport{base: &http.Transport{}}}
	c := newClient(t, opts)

	f.QueueNext(0)
	polled, answer := make(chan struct{}), make(chan struct{})
	inDelete, unblockDelete := make(chan struct{}), make(chan struct{})
	var pollOnce, delOnce, answerOnce, unblockOnce sync.Once
	release := func() {
		answerOnce.Do(func() { close(answer) })
		unblockOnce.Do(func() { close(unblockDelete) })
	}
	t.Cleanup(release) // a failing run must not leave handlers blocked
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		switch {
		case r.Method == http.MethodGet && r.Path == leasesPath+"/1-fake":
			pollOnce.Do(func() { close(polled) })
			<-answer
			return 200, env(map[string]any{"id": "1-fake", "state": rl.StateHeld, "device": "gpu.1"}), true
		case r.Method == http.MethodDelete:
			delOnce.Do(func() {
				close(inDelete)
				<-unblockDelete
			})
		}
		return 0, nil, false
	})
	done := make(chan rl.Result, 1)
	go func() { done <- c.Acquire(context.Background(), req(-1)) }()
	<-polled
	closed := make(chan rl.CloseReport, 1)
	go func() { closed <- c.Close(context.Background()) }()
	<-inDelete // Close's DELETE is out but not answered
	answerOnce.Do(func() { close(answer) })

	select {
	case res := <-done:
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonClosed {
			t.Fatalf("grant during Close = %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("acquire did not return")
	}
	release()
	<-closed
	eventually(t, 2*time.Second, func() bool { return len(f.Live()) == 0 }, "granted lease left on the core")
}

// Close landing between ReleaseHandoff's read of the old file and its skip
// check: a kept id of this process that is also in the old file must not be
// deleted, and the file Close writes still lists it.
func TestReleaseHandoffRacingCloseKeepsTheKeptLease(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	x := held(t, c)
	c.KeepOnClose(x)
	writeHandoffDoc(t, opts.HandoffFile, rl.HandoffEntry{ID: x.ID}, rl.HandoffEntry{ID: "old-1"})

	closed := make(chan rl.CloseReport, 1)
	rl.SetHandoffAfterReadForTest(func() {
		go func() { closed <- c.Close(context.Background()) }()
		eventually(t, 2*time.Second, func() bool { return rl.ClosedForTest(c) }, "Close did not start")
	})
	t.Cleanup(func() { rl.SetHandoffAfterReadForTest(nil) })
	c.ReleaseHandoff(context.Background())
	rl.SetHandoffAfterReadForTest(nil)
	var rep rl.CloseReport
	select {
	case rep = <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if rep.HandoffErr != nil || !reflect.DeepEqual(rep.Kept, []string{x.ID}) {
		t.Fatalf("close report %+v", rep)
	}
	if indexOf(f.Requests(), deleteOf(x.ID)) >= 0 {
		t.Fatalf("kept lease %s deleted", x.ID)
	}
	got := entryIDs(readHandoffDoc(t, opts.HandoffFile).Leases)
	sort.Strings(got)
	if !reflect.DeepEqual(got, sorted(x.ID, "old-1")) {
		t.Fatalf("handoff leases %v, want %s and the untaken old-1", got, x.ID)
	}
}
