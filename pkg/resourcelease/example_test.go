package resourcelease_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	rl "github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
)

// exampleCore is a minimal stand-in for the core scheduler so the examples
// run under go test: every lease request is granted at once and a release
// long-poll (wait_for=release) answers that the core wants the memory back.
func exampleCore() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		lease := map[string]any{
			"id":                "1-example",
			"state":             rl.StateHeld,
			"device":            "cpu",
			"release_requested": r.URL.Query().Get("wait_for") == "release",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": 200, "message": "ok", "data": lease})
	}))
}

// A task lease: acquire before the heavy work, heartbeat while it runs,
// release when done. A fail-open Result means "run anyway".
func ExampleClient_Acquire() {
	core := exampleCore()
	defer core.Close()

	c, err := rl.New(rl.Options{
		Client:  "example",
		BaseURL: core.URL, // services set URLFile (the core's nimoos.url) instead
		Retry:   rl.DefaultRetry(),
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer c.Close(context.Background()) // releases whatever is still registered

	ctx := context.Background()
	res := c.Acquire(ctx, rl.Request{
		Task:          "embed",
		Class:         rl.ClassHeavy,
		Priority:      1, // 0 interactive, 1 foreground, 2 background: always chosen by the caller, never derived from Tier
		Tier:          rl.TierUserBatch,
		Workload:      "example-embed",
		DevicePref:    []string{"cpu"}, // required, most preferred first (e.g. "gpu.0", "cpu" with AllowFallback)
		AllowFallback: true,
		EstRAMMB:      1024,
		MaxWait:       2 * time.Minute,
	})
	switch res.Outcome {
	case rl.OutcomeHeld:
		stop := c.Hold(res.Lease, 15*time.Second, nil, nil) // heartbeat <= 20 s
		defer c.Release(ctx, res.Lease)
		defer stop()
		fmt.Println("held", res.Lease.Usable())
	case rl.OutcomeFailOpen:
		// Scheduler off or unreachable: run anyway on res.Lease.Device.
	case rl.OutcomeRejected:
		return // the core refused the request as described: do not run it
	case rl.OutcomeWithdrawn:
		return // the caller gave up waiting
	}
	// ... run the job on res.Lease.Device ...

	// Output: held true
}

// A resident lease: register the loaded model, heartbeat it, and unload when
// the core asks for the memory back. Services run WatchRelease on its own
// goroutine for as long as the model stays loaded.
func ExampleClient_WatchRelease() {
	core := exampleCore()
	defer core.Close()

	c, err := rl.New(rl.Options{Client: "example", BaseURL: core.URL})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer c.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := c.Acquire(ctx, rl.Request{
		Task:       "model:clip",
		Class:      rl.ClassMemory, // a resident is memory or light, never heavy
		Priority:   2,
		Tier:       rl.TierAutoBatch,
		Kind:       rl.KindResident,
		Workload:   "clip",
		DevicePref: []string{"cpu"},
		PID:        os.Getpid(), // required for a resident: the process that holds the memory (e.g. the model server)
		EstRAMMB:   1500,
		MaxWait:    30 * time.Second,
	})
	if res.Outcome != rl.OutcomeHeld {
		return // fail-open: keep the model, unaccounted, and try again later
	}
	lease := res.Lease
	stop := c.Hold(lease, 15*time.Second,
		func() (rl.HeartbeatInfo, bool) { return rl.HeartbeatInfo{Loaded: true}, true },
		func(*rl.Lease) { /* lease lost: register the resident again */ })
	defer stop()

	c.WatchRelease(ctx, lease, func(l *rl.Lease) {
		fmt.Println("release requested")
		// ... unload the model, then hand the memory back ...
		c.Release(context.Background(), l)
	})

	// Output: release requested
}
