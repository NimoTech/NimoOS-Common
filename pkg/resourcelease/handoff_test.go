package resourcelease_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	"go.uber.org/zap/zapcore"

	rl "github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
	"github.com/NimoTech/NimoOS-Common/pkg/resourcelease/leasetest"
)

func TestReleaseHandoffDeletesAndRemovesFile(t *testing.T) {
	f := leasetest.New(t)
	// The previous process left one lease on the core.
	prev := newClient(t, photosOpts(f))
	old := held(t, prev)

	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	writeHandoffDoc(t, opts.HandoffFile,
		rl.HandoffEntry{ID: old.ID, Workload: "resident:a", PID: 1, Device: "gpu.1"},
		rl.HandoffEntry{ID: "404-fake", Workload: "resident:b", PID: 1, Device: "cpu"}, // already expired: 404
	)
	rep := c.ReleaseHandoff(context.Background())
	if want := sorted(old.ID, "404-fake"); !reflect.DeepEqual(rep.Released, want) {
		t.Fatalf("released %v, want %v (%+v)", rep.Released, want, rep)
	}
	if len(rep.Failed) != 0 || len(rep.Skipped) != 0 || rep.Err != nil {
		t.Fatalf("report %+v", rep)
	}
	if got := deletedIDs(f); !reflect.DeepEqual(got, sorted(old.ID, "404-fake")) {
		t.Fatalf("DELETEs %v", got)
	}
	if live := f.Live(); len(live) != 0 {
		t.Fatalf("old lease still live: %v", live)
	}
	if _, err := os.Stat(opts.HandoffFile); !os.IsNotExist(err) {
		t.Fatalf("handoff file not removed: %v", err)
	}
	// A second run finds no file.
	if rep := c.ReleaseHandoff(context.Background()); !reflect.DeepEqual(rep, rl.HandoffReport{}) {
		t.Fatalf("second run %+v", rep)
	}
}

func TestHandoffSkipsOwnLeases(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	own := held(t, c)
	handBuilt := leasetest.Lease("77-fake", "cpu", rl.StateHeld)
	c.KeepOnClose(handBuilt) // kept by this client although never registered
	writeHandoffDoc(t, opts.HandoffFile,
		rl.HandoffEntry{ID: own.ID},
		rl.HandoffEntry{ID: "77-fake"},
		rl.HandoffEntry{ID: "other-1"},
		rl.HandoffEntry{ID: "other-1"}, // duplicate: one DELETE
	)
	rep := c.ReleaseHandoff(context.Background())
	if !reflect.DeepEqual(rep.Skipped, []string{own.ID, "77-fake"}) || !reflect.DeepEqual(rep.Released, []string{"other-1"}) {
		t.Fatalf("report %+v", rep)
	}
	if got := deletedIDs(f); !reflect.DeepEqual(got, []string{"other-1"}) {
		t.Fatalf("DELETEs %v: own leases must not be deleted", got)
	}
	if !own.Usable() || !rl.RegisteredForTest(c, own.ID) {
		t.Fatal("own lease touched")
	}
	if _, err := os.Stat(opts.HandoffFile); !os.IsNotExist(err) {
		t.Fatalf("handoff file not removed: %v", err)
	}
}

func TestHandoffCorruptFileIsRemoved(t *testing.T) {
	for name, content := range map[string]string{
		"half written":  "{",
		"wrong version": `{"version":2,"client":"photos","leases":[{"id":"1-fake"}]}`,
		"not an object": `[1,2]`,
	} {
		t.Run(name, func(t *testing.T) {
			f := leasetest.New(t)
			opts := handoffOpts(t, f)
			logs := observed(&opts)
			c := newClient(t, opts)
			if err := os.WriteFile(opts.HandoffFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			rep := c.ReleaseHandoff(context.Background())
			if rep.Err == nil || len(rep.Released)+len(rep.Skipped)+len(rep.Failed) != 0 {
				t.Fatalf("report %+v", rep)
			}
			if n := len(f.Requests()); n != 0 {
				t.Fatalf("%d requests for a corrupt file", n)
			}
			if _, err := os.Stat(opts.HandoffFile); !os.IsNotExist(err) {
				t.Fatalf("corrupt file not removed: %v", err)
			}
			if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 1 {
				t.Fatalf("%d Error logs, want 1", n)
			}
		})
	}
}

func TestHandoffMissingFileIsNoop(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, handoffOpts(t, f))
	if rep := c.ReleaseHandoff(context.Background()); !reflect.DeepEqual(rep, rl.HandoffReport{}) {
		t.Fatalf("report %+v", rep)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}

func TestHandoffDisabledWithoutPath(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f)) // HandoffFile ""
	kept, other := held(t, c), held(t, c)
	n := len(f.Requests())
	if rep := c.ReleaseHandoff(context.Background()); !reflect.DeepEqual(rep, rl.HandoffReport{}) {
		t.Fatalf("ReleaseHandoff %+v", rep)
	}
	if got := len(f.Requests()); got != n {
		t.Fatal("ReleaseHandoff without a path sent requests")
	}
	c.KeepOnClose(kept)
	rep := c.Close(context.Background())
	if !reflect.DeepEqual(rep.Kept, []string{kept.ID}) || !reflect.DeepEqual(rep.Released, []string{other.ID}) || rep.HandoffErr != nil {
		t.Fatalf("Close %+v", rep)
	}
	if got := deletedIDs(f); !reflect.DeepEqual(got, []string{other.ID}) {
		t.Fatalf("DELETEs %v", got)
	}
}

func TestReleaseHandoffOnADisabledClientIsNoop(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	opts.Disabled = true
	c := newClient(t, opts)
	writeHandoffDoc(t, opts.HandoffFile, rl.HandoffEntry{ID: "1-fake"})
	if rep := c.ReleaseHandoff(context.Background()); !reflect.DeepEqual(rep, rl.HandoffReport{}) {
		t.Fatalf("report %+v", rep)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}

// Two clients misconfigured with one path must not delete each other's
// leases.
func TestHandoffOfAnotherClientIsLeftAlone(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	logs := observed(&opts)
	c := newClient(t, opts)
	content := `{"version":1,"client":"ai","written_at":"2026-10-29T01:02:03Z","leases":[{"id":"7-ai"}]}`
	if err := os.WriteFile(opts.HandoffFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := c.ReleaseHandoff(context.Background())
	if rep.Err == nil || len(rep.Released)+len(rep.Skipped)+len(rep.Failed) != 0 {
		t.Fatalf("report %+v", rep)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests for another client's file", n)
	}
	if b, err := os.ReadFile(opts.HandoffFile); err != nil || string(b) != content {
		t.Fatalf("file changed: %q, %v", b, err)
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 1 {
		t.Fatalf("%d Warn logs, want 1", n)
	}
}
