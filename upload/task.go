// Package upload 是可恢复上传引擎的通用内核:模型、状态机、分级 GC、Store 接口。
// 本包不依赖 gorm/tusd/echo —— gorm tag 仅为纯 struct tag 字符串,供使用方的 GORM
// 层读取;原生 SQL 使用方忽略 tag、按字段名自行映射。
package upload

import "errors"

const (
	UploadStatusUploading = "uploading"
	UploadStatusPaused    = "paused"
	UploadStatusFailed    = "failed"
	UploadStatusCompleted = "completed"
	UploadStatusCanceled  = "canceled"
)

// ErrNotFound 是 Store.Get 找不到记录时应返回的哨兵错误(各实现把驱动错误映射到它)。
var ErrNotFound = errors.New("upload task not found")

// UploadTask 是上传任务真相源。字段含 gorm tag(纯字符串,不引入 gorm 依赖)。
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
