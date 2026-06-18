package upload

import (
	"encoding/json"
	"testing"
	"time"
)

// TestNewTask 验证 NewTask 工厂的字段映射。
func TestNewTask(t *testing.T) {
	now := time.Unix(1700000000, 0)
	idle := DefaultIdleTimeoutSeconds

	meta := map[string]string{
		"filename":     "photo.jpg",
		"targetPath":   "/photos/2024",
		"relativePath": "2024/photo.jpg",
		"filetype":     "image/jpeg",
		"fingerprint":  "fp-abc",
		"batch_id":     "batch-1",
		"client_id":    "cli-xyz",
	}

	task := NewTask("upload-id-1", "user-42", meta, 1234567, "Mozilla/5.0", "192.168.1.1", idle, now)

	if task.ID != "upload-id-1" {
		t.Fatalf("ID mismatch: %s", task.ID)
	}
	if task.OwnerUserID != "user-42" {
		t.Fatalf("OwnerUserID mismatch: %s", task.OwnerUserID)
	}
	if task.Filename != "photo.jpg" {
		t.Fatalf("Filename mismatch: %s", task.Filename)
	}
	if task.TargetPath != "/photos/2024" {
		t.Fatalf("TargetPath mismatch: %s", task.TargetPath)
	}
	if task.RelativePath != "2024/photo.jpg" {
		t.Fatalf("RelativePath mismatch: %s", task.RelativePath)
	}
	if task.Size != 1234567 {
		t.Fatalf("Size mismatch: %d", task.Size)
	}
	if task.Mime != "image/jpeg" {
		t.Fatalf("Mime mismatch: %s", task.Mime)
	}
	if task.Fingerprint != "fp-abc" {
		t.Fatalf("Fingerprint mismatch: %s", task.Fingerprint)
	}
	if task.BatchID != "batch-1" {
		t.Fatalf("BatchID mismatch: %s", task.BatchID)
	}
	if task.ClientID != "cli-xyz" {
		t.Fatalf("ClientID mismatch: %s", task.ClientID)
	}
	if task.Status != UploadStatusUploading {
		t.Fatalf("Status mismatch: %s", task.Status)
	}
	expectedExpires := now.Unix() + idle
	if task.ExpiresAt != expectedExpires {
		t.Fatalf("ExpiresAt mismatch: got %d, want %d", task.ExpiresAt, expectedExpires)
	}

	// ClientMeta 须含 user_agent 和 remote_addr
	var cm map[string]string
	if err := json.Unmarshal([]byte(task.ClientMeta), &cm); err != nil {
		t.Fatalf("ClientMeta not valid JSON: %v", err)
	}
	if cm["user_agent"] != "Mozilla/5.0" {
		t.Fatalf("ClientMeta user_agent mismatch: %s", cm["user_agent"])
	}
	if cm["remote_addr"] != "192.168.1.1" {
		t.Fatalf("ClientMeta remote_addr mismatch: %s", cm["remote_addr"])
	}
}

// TestNewTaskRelativePathFallback 验证 relativePath 为空时回退到 filename。
func TestNewTaskRelativePathFallback(t *testing.T) {
	now := time.Now()
	meta := map[string]string{
		"filename": "backup.tar.gz",
		// relativePath 故意省略
	}
	task := NewTask("id-2", "owner-1", meta, 0, "", "", DefaultIdleTimeoutSeconds, now)
	if task.RelativePath != "backup.tar.gz" {
		t.Fatalf("expected relativePath to fall back to filename, got: %s", task.RelativePath)
	}
}
