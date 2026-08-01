// Package upload is the common kernel for the resumable upload engine: model, state
// machine, tiered GC, and Store interface. This package does not depend on
// gorm/tusd/echo — the gorm tags are plain struct tag strings for the caller's GORM
// layer to read; raw-SQL callers ignore the tags and map fields by name themselves.
package upload

import "errors"

const (
	UploadStatusUploading = "uploading"
	UploadStatusPaused    = "paused"
	UploadStatusFailed    = "failed"
	UploadStatusCompleted = "completed"
	UploadStatusCanceled  = "canceled"
)

// ErrNotFound is the sentinel error Store.Get should return when a record isn't found
// (each implementation maps its driver errors to it).
var ErrNotFound = errors.New("upload task not found")

// UploadTask is the source of truth for an upload task. Fields carry gorm tags (plain strings, no gorm dependency).
type UploadTask struct {
	ID           string `gorm:"column:id;primaryKey" json:"id"`
	OwnerUserID  string `gorm:"column:owner_user_id;index" json:"owner_user_id"`
	Filename     string `gorm:"column:filename" json:"filename"`
	TargetPath   string `gorm:"column:target_path" json:"target_path"`
	RelativePath string `gorm:"column:relative_path" json:"relative_path"`
	Size         int64  `gorm:"column:size" json:"size"`
	Mime         string `gorm:"column:mime" json:"mime"`
	Fingerprint  string `gorm:"column:fingerprint;index" json:"fingerprint"`
	ContentHash  string `gorm:"column:content_hash;index" json:"content_hash"`
	UploadURL    string `gorm:"column:upload_url" json:"upload_url"`
	Offset       int64  `gorm:"column:uploaded_offset" json:"offset"`
	Status       string `gorm:"column:status;index" json:"status"`
	RetryCount   int    `gorm:"column:retry_count" json:"retry_count"`
	Error        string `gorm:"column:error" json:"error"`
	LastErrorAt  int64  `gorm:"column:last_error_at" json:"last_error_at"`
	BatchID      string `gorm:"column:batch_id;index" json:"batch_id"`
	ClientID     string `gorm:"column:client_id" json:"client_id"`
	ClientMeta   string `gorm:"column:client_meta" json:"client_meta"`
	CreatedAt    int64  `gorm:"column:created_at;autoCreateTime" json:"created_at"`
	UpdatedAt    int64  `gorm:"column:updated_at;autoUpdateTime" json:"updated_at"`
	ExpiresAt    int64  `gorm:"column:expires_at;index" json:"expires_at"`
}

func (UploadTask) TableName() string { return "o_upload_tasks" }
