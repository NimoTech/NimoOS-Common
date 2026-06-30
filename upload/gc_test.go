package upload

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSweepTasksTieredCleanup 验证分级 GC 语义:
//   - canceled(过期) → removeStaging + Delete(deleted++)
//   - uploading(过期) → SetStatus paused,不删 staging(transitioned++)
//   - paused(未过期) → 不处理
//   - completed(expires=0) → 不处理
func TestSweepTasksTieredCleanup(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_000_000, 0)
	past := now.Unix() - 10 // 已过期
	future := now.Unix() + 3600

	// 在 staging 目录为 canceled 任务创建伪文件
	canceledID := "canceled-task-id"
	if err := os.WriteFile(filepath.Join(dir, canceledID), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, canceledID+".info"), []byte("info"), 0644); err != nil {
		t.Fatal(err)
	}

	// 为 uploading 任务也创建 staging 文件(GC 不应删除它)
	uploadingID := "uploading-task-id"
	uploadingFile := filepath.Join(dir, uploadingID)
	if err := os.WriteFile(uploadingFile, []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	s := newFakeStore()
	// canceled 任务,已过期
	_ = s.Create(&UploadTask{ID: canceledID, Status: UploadStatusCanceled, ExpiresAt: past})
	// uploading 任务,已过期(应转为 paused,不删 staging)
	_ = s.Create(&UploadTask{ID: uploadingID, Status: UploadStatusUploading, ExpiresAt: past})
	// paused 任务,未过期(不处理)
	_ = s.Create(&UploadTask{ID: "paused-fresh", Status: UploadStatusPaused, ExpiresAt: future})
	// completed 任务,expires=0(不处理)
	_ = s.Create(&UploadTask{ID: "completed-no-expire", Status: UploadStatusCompleted, ExpiresAt: 0})

	cfg := GCConfig{
		StagingDir:     dir,
		PausedTTL:      DefaultPausedTTLSeconds,
		GCIntervalSecs: DefaultGCIntervalSeconds,
	}

	transitioned, deleted, err := SweepTasks(s, cfg, now)
	if err != nil {
		t.Fatalf("SweepTasks error: %v", err)
	}

	// 断言计数
	if transitioned != 1 {
		t.Errorf("expected transitioned=1, got %d", transitioned)
	}
	if deleted != 1 {
		t.Errorf("expected deleted=1, got %d", deleted)
	}

	// canceled 任务的 staging 文件应已删除
	if _, err := os.Stat(filepath.Join(dir, canceledID)); !os.IsNotExist(err) {
		t.Error("expected canceled staging file to be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, canceledID+".info")); !os.IsNotExist(err) {
		t.Error("expected canceled staging .info file to be removed")
	}

	// canceled 任务记录应已从 store 删除
	if _, err := s.Get(canceledID); err != ErrNotFound {
		t.Error("expected canceled task to be deleted from store")
	}

	// uploading 任务的 staging 文件应保留
	if _, err := os.Stat(uploadingFile); os.IsNotExist(err) {
		t.Error("expected uploading staging file to be preserved")
	}

	// uploading 任务应转为 paused,且设置了新的 expiresAt
	got, err := s.Get(uploadingID)
	if err != nil {
		t.Fatalf("Get uploading task: %v", err)
	}
	if got.Status != UploadStatusPaused {
		t.Errorf("expected uploading task transitioned to paused, got %s", got.Status)
	}
	expectedExpires := now.Unix() + DefaultPausedTTLSeconds
	if got.ExpiresAt != expectedExpires {
		t.Errorf("expected expiresAt=%d, got %d", expectedExpires, got.ExpiresAt)
	}

	// paused-fresh 和 completed 任务应未受影响
	pausedFresh, _ := s.Get("paused-fresh")
	if pausedFresh.Status != UploadStatusPaused {
		t.Error("paused-fresh task should not be modified")
	}
	completedTask, _ := s.Get("completed-no-expire")
	if completedTask.Status != UploadStatusCompleted {
		t.Error("completed task should not be modified")
	}
}

// TestSweepTasksFailedAndPausedDeleted 验证 failed/paused(过期)都会被删除
func TestSweepTasksFailedAndPausedDeleted(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2_000_000, 0)
	past := now.Unix() - 1

	failedID := "failed-task"
	pausedID := "paused-task"

	// 创建对应 staging 文件
	for _, id := range []string{failedID, pausedID} {
		_ = os.WriteFile(filepath.Join(dir, id), []byte("x"), 0644)
	}

	s := newFakeStore()
	_ = s.Create(&UploadTask{ID: failedID, Status: UploadStatusFailed, ExpiresAt: past})
	_ = s.Create(&UploadTask{ID: pausedID, Status: UploadStatusPaused, ExpiresAt: past})

	cfg := GCConfig{
		StagingDir:     dir,
		PausedTTL:      DefaultPausedTTLSeconds,
		GCIntervalSecs: DefaultGCIntervalSeconds,
	}

	_, deleted, err := SweepTasks(s, cfg, now)
	if err != nil {
		t.Fatalf("SweepTasks error: %v", err)
	}
	if deleted != 2 {
		t.Errorf("expected deleted=2, got %d", deleted)
	}

	// staging 文件已清理
	for _, id := range []string{failedID, pausedID} {
		if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
			t.Errorf("staging file for %s should be removed", id)
		}
		if _, err := s.Get(id); err != ErrNotFound {
			t.Errorf("task %s should be deleted from store", id)
		}
	}
}
