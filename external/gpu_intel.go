package external

import (
	"encoding/json"
	"os/exec"
	"sync"
	"time"
)

// IntelGpuSample mirrors the per-tick JSON object that `intel_gpu_top -J`
// emits. Only the fields we surface are decoded; extra keys are ignored.
type IntelGpuSample struct {
	Frequency struct {
		Actual float64 `json:"actual"`
	} `json:"frequency"`
	Power struct {
		GPU float64 `json:"GPU"`
	} `json:"power"`
	Engines map[string]struct {
		Busy float64 `json:"busy"`
	} `json:"engines"`
}

// IntelGpuStat is the rolling snapshot consumed by NimoOS service callers.
type IntelGpuStat struct {
	UtilizationGpu float64   `json:"utilization_gpu"`
	PowerW         float64   `json:"power_w"`
	FreqMHz        float64   `json:"freq_mhz"`
	Updated        time.Time `json:"-"`
}

var (
	intelGpuCache IntelGpuStat
	intelGpuMu    sync.RWMutex
	intelGpuOnce  sync.Once
	intelGpuKnown bool
)

// StartIntelGpuMonitor spawns intel_gpu_top in the background and keeps the
// most recent sample in memory. Safe to call multiple times; only the first
// call has any effect. If intel_gpu_top is not installed the call is a no-op.
func StartIntelGpuMonitor() {
	intelGpuOnce.Do(func() {
		if _, err := exec.LookPath("intel_gpu_top"); err != nil {
			return
		}
		intelGpuKnown = true
		go runIntelGpuTop()
	})
}

func runIntelGpuTop() {
	for {
		if err := streamIntelGpuTop(); err != nil {
			// transient failure — back off and try again
		}
		time.Sleep(5 * time.Second)
	}
}

func streamIntelGpuTop() error {
	cmd := exec.Command("intel_gpu_top", "-J", "-s", "1000")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	dec := json.NewDecoder(stdout)
	// The stream starts with an opening '[' that frames the array of samples.
	if _, err := dec.Token(); err != nil {
		return err
	}
	for dec.More() {
		var s IntelGpuSample
		if err := dec.Decode(&s); err != nil {
			return err
		}
		var maxBusy float64
		for _, e := range s.Engines {
			if e.Busy > maxBusy {
				maxBusy = e.Busy
			}
		}
		intelGpuMu.Lock()
		intelGpuCache = IntelGpuStat{
			UtilizationGpu: maxBusy,
			PowerW:         s.Power.GPU,
			FreqMHz:        s.Frequency.Actual,
			Updated:        time.Now(),
		}
		intelGpuMu.Unlock()
	}
	return nil
}

// GetIntelGpuStat returns the latest snapshot. The boolean is false if the
// monitor never started (intel_gpu_top missing) or the cache is stale (>5s old).
func GetIntelGpuStat() (IntelGpuStat, bool) {
	if !intelGpuKnown {
		return IntelGpuStat{}, false
	}
	intelGpuMu.RLock()
	defer intelGpuMu.RUnlock()
	if intelGpuCache.Updated.IsZero() || time.Since(intelGpuCache.Updated) > 5*time.Second {
		return IntelGpuStat{}, false
	}
	return intelGpuCache, true
}
