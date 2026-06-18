package upload

import "testing"

func TestTableNameAndStatusConstants(t *testing.T) {
	if (UploadTask{}).TableName() != "o_upload_tasks" {
		t.Fatalf("table name: %s", (UploadTask{}).TableName())
	}
	for _, s := range []string{UploadStatusUploading, UploadStatusPaused, UploadStatusFailed, UploadStatusCompleted, UploadStatusCanceled} {
		if s == "" {
			t.Fatal("empty status constant")
		}
	}
	if ErrNotFound == nil {
		t.Fatal("ErrNotFound must be defined")
	}
}
