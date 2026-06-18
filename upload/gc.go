package upload

import (
	"os"
	"path/filepath"
	"time"

	"github.com/NimoTech/NimoOS-Common/utils/logger"
	"go.uber.org/zap"
)

// removeStaging 删除 staging 目录中指定任务的数据文件与 .info 文件(忽略不存在错误)。
func removeStaging(dir, id string) {
	os.Remove(filepath.Join(dir, id))          //nolint:errcheck
	os.Remove(filepath.Join(dir, id+".info")) //nolint:errcheck
}

// SweepTasks 执行一次分级 GC 扫描:
//   - uploading(过期) → SetStatus paused(now+PausedTTL),不删 staging → transitioned++
//   - paused/failed/canceled(过期) → removeStaging + Delete → deleted++
//
// 返回本轮 transitioned/deleted 计数与首个遇到的错误(遇错即返回)。
func SweepTasks(s Store, cfg GCConfig, now time.Time) (transitioned, deleted int, err error) {
	due, e := s.ListDueForGC(now.Unix())
	if e != nil {
		return 0, 0, e
	}
	for _, t := range due {
		switch t.Status {
		case UploadStatusUploading:
			if e := s.SetStatus(t.ID, UploadStatusPaused, now.Unix()+cfg.PausedTTL); e != nil {
				return transitioned, deleted, e
			}
			transitioned++
		case UploadStatusPaused, UploadStatusFailed, UploadStatusCanceled:
			removeStaging(cfg.StagingDir, t.ID)
			if e := s.Delete(t.ID); e != nil {
				return transitioned, deleted, e
			}
			deleted++
		}
	}
	return transitioned, deleted, nil
}

// StartGC 在独立 goroutine 中按 GCConfig.GCIntervalSecs 定期调用 SweepTasks。
// 若 GCIntervalSecs <= 0,则使用 DefaultGCIntervalSeconds。
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
