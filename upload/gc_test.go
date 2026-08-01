package upload

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSweepTasksTieredCleanup verifies tiered GC semantics:
//   - canceled (expired) → removeStaging + Delete (deleted++)
//   - uploading (expired) → SetStatus paused, staging kept (transitioned++)
//   - paused (not expired) → not processed
//   - completed (expires=0) → not processed
func TestSweepTasksTieredCleanup(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_000_000, 0)
	past := now.Unix() - 10 // already expired
	future := now.Unix() + 3600

	// create dummy files in the staging dir for the canceled task
	canceledID := "canceled-task-id"
	if err := os.WriteFile(filepath.Join(dir, canceledID), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, canceledID+".info"), []byte("info"), 0644); err != nil {
		t.Fatal(err)
	}

	// also create a staging file for the uploading task (GC should not remove it)
	uploadingID := "uploading-task-id"
	uploadingFile := filepath.Join(dir, uploadingID)
	if err := os.WriteFile(uploadingFile, []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}

	s := newFakeStore()
	// canceled task, expired
	_ = s.Create(&UploadTask{ID: canceledID, Status: UploadStatusCanceled, ExpiresAt: past})
	// uploading task, expired (should transition to paused, staging kept)
	_ = s.Create(&UploadTask{ID: uploadingID, Status: UploadStatusUploading, ExpiresAt: past})
	// paused task, not expired (not processed)
	_ = s.Create(&UploadTask{ID: "paused-fresh", Status: UploadStatusPaused, ExpiresAt: future})
	// completed task, expires=0 (not processed)
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

	// assert counts
	if transitioned != 1 {
		t.Errorf("expected transitioned=1, got %d", transitioned)
	}
	if deleted != 1 {
		t.Errorf("expected deleted=1, got %d", deleted)
	}

	// canceled task's staging file should be removed
	if _, err := os.Stat(filepath.Join(dir, canceledID)); !os.IsNotExist(err) {
		t.Error("expected canceled staging file to be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, canceledID+".info")); !os.IsNotExist(err) {
		t.Error("expected canceled staging .info file to be removed")
	}

	// canceled task record should be deleted from store
	if _, err := s.Get(canceledID); err != ErrNotFound {
		t.Error("expected canceled task to be deleted from store")
	}

	// uploading task's staging file should be preserved
	if _, err := os.Stat(uploadingFile); os.IsNotExist(err) {
		t.Error("expected uploading staging file to be preserved")
	}

	// uploading task should transition to paused, with a new expiresAt set
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

	// paused-fresh and completed tasks should be unaffected
	pausedFresh, _ := s.Get("paused-fresh")
	if pausedFresh.Status != UploadStatusPaused {
		t.Error("paused-fresh task should not be modified")
	}
	completedTask, _ := s.Get("completed-no-expire")
	if completedTask.Status != UploadStatusCompleted {
		t.Error("completed task should not be modified")
	}
}

// TestSweepTasksFailedAndPausedDeleted verifies that both failed and paused (expired) tasks get deleted
func TestSweepTasksFailedAndPausedDeleted(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2_000_000, 0)
	past := now.Unix() - 1

	failedID := "failed-task"
	pausedID := "paused-task"

	// create the corresponding staging files
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

	// staging files should be cleaned up
	for _, id := range []string{failedID, pausedID} {
		if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
			t.Errorf("staging file for %s should be removed", id)
		}
		if _, err := s.Get(id); err != ErrNotFound {
			t.Errorf("task %s should be deleted from store", id)
		}
	}
}

// TestSweepTasksRemovesStagingFromAllDirs verifies that when StagingDirs is non-nil, GC
// tries removeStaging against every returned directory — covering the scenario where,
// after per-volume staging routing, leftovers aren't in StagingDir (dirA is empty,
// leftovers actually land in dirB).
func TestSweepTasksRemovesStagingFromAllDirs(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	now := time.Unix(3_000_000, 0)
	past := now.Unix() - 1

	taskID := "t1"
	// dirA (cfg.StagingDir) has no files; dirB has t1 and t1.info
	if err := os.WriteFile(filepath.Join(dirB, taskID), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, taskID+".info"), []byte("info"), 0644); err != nil {
		t.Fatal(err)
	}

	s := newFakeStore()
	_ = s.Create(&UploadTask{ID: taskID, Status: UploadStatusPaused, ExpiresAt: past})

	cfg := GCConfig{
		StagingDir: dirA,
		StagingDirs: func() []string {
			return []string{dirA, dirB}
		},
		PausedTTL:      DefaultPausedTTLSeconds,
		GCIntervalSecs: DefaultGCIntervalSeconds,
	}

	_, deleted, err := SweepTasks(s, cfg, now)
	if err != nil {
		t.Fatalf("SweepTasks error: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected deleted=1, got %d", deleted)
	}

	if _, err := os.Stat(filepath.Join(dirB, taskID)); !os.IsNotExist(err) {
		t.Error("expected dirB staging file to be removed")
	}
	if _, err := os.Stat(filepath.Join(dirB, taskID+".info")); !os.IsNotExist(err) {
		t.Error("expected dirB staging .info file to be removed")
	}
	if _, err := s.Get(taskID); err != ErrNotFound {
		t.Error("expected task to be deleted from store")
	}
}

// TestSweepTasksNilStagingDirsFallsBack verifies that when StagingDirs is nil, cleanup
// falls back to cfg.StagingDir only (the old behavior), preserving backward compatibility.
func TestSweepTasksNilStagingDirsFallsBack(t *testing.T) {
	dirA := t.TempDir()
	now := time.Unix(4_000_000, 0)
	past := now.Unix() - 1

	taskID := "t1"
	if err := os.WriteFile(filepath.Join(dirA, taskID), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	s := newFakeStore()
	_ = s.Create(&UploadTask{ID: taskID, Status: UploadStatusPaused, ExpiresAt: past})

	cfg := GCConfig{
		StagingDir:     dirA,
		StagingDirs:    nil,
		PausedTTL:      DefaultPausedTTLSeconds,
		GCIntervalSecs: DefaultGCIntervalSeconds,
	}

	_, deleted, err := SweepTasks(s, cfg, now)
	if err != nil {
		t.Fatalf("SweepTasks error: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected deleted=1, got %d", deleted)
	}
	if _, err := os.Stat(filepath.Join(dirA, taskID)); !os.IsNotExist(err) {
		t.Error("expected dirA staging file to be removed")
	}
	if _, err := s.Get(taskID); err != ErrNotFound {
		t.Error("expected task to be deleted from store")
	}
}
