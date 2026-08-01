package upload

import (
	"os"
	"path/filepath"
	"time"

	"github.com/NimoTech/NimoOS-Common/utils/logger"
	"go.uber.org/zap"
)

// removeStaging deletes the given task's data file and .info file from the staging directory (ignores not-exist errors).
func removeStaging(dir, id string) {
	os.Remove(filepath.Join(dir, id))         //nolint:errcheck
	os.Remove(filepath.Join(dir, id+".info")) //nolint:errcheck
}

// SweepTasks runs one pass of tiered GC scanning:
//   - uploading (expired) → SetStatus paused (now+PausedTTL), staging kept → transitioned++
//   - paused/failed/canceled (expired) → removeStaging + Delete → deleted++
//
// Returns this pass's transitioned/deleted counts and the first error encountered (returns immediately on error).
func SweepTasks(s Store, cfg GCConfig, now time.Time) (transitioned, deleted int, err error) {
	due, e := s.ListDueForGC(now.Unix())
	if e != nil {
		return 0, 0, e
	}
	// Directory list is resolved once per pass (enumeration does real IO, matching batch_sweeper's once-per-pass semantics).
	dirs := []string{cfg.StagingDir}
	if cfg.StagingDirs != nil {
		dirs = cfg.StagingDirs()
	}
	for _, t := range due {
		switch t.Status {
		case UploadStatusUploading:
			if e := s.SetStatus(t.ID, UploadStatusPaused, now.Unix()+cfg.PausedTTL); e != nil {
				return transitioned, deleted, e
			}
			transitioned++
		case UploadStatusPaused, UploadStatusFailed, UploadStatusCanceled:
			for _, d := range dirs {
				removeStaging(d, t.ID)
			}
			if e := s.Delete(t.ID); e != nil {
				return transitioned, deleted, e
			}
			deleted++
		}
	}
	return transitioned, deleted, nil
}

// StartGC calls SweepTasks periodically in its own goroutine, per GCConfig.GCIntervalSecs.
// If GCIntervalSecs <= 0, DefaultGCIntervalSeconds is used instead.
func StartGC(s Store, cfg GCConfig) {
	interval := cfg.GCIntervalSecs
	if interval <= 0 {
		interval = DefaultGCIntervalSeconds
	}
	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if tr, del, e := SweepTasks(s, cfg, time.Now()); e != nil {
				logger.Error("upload GC sweep failed", zap.Error(e))
			} else if tr+del > 0 {
				logger.Info("upload GC", zap.Int("transitioned", tr), zap.Int("deleted", del))
			}
		}
	}()
}
