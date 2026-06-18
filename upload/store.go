package upload

import "errors"

// Store 是上传任务的持久化接口。各服务自行实现(GORM/原生 SQL 均可),
// 通用内核(Cancel/SweepTasks/StartGC)只依赖此接口,不依赖任何具体驱动。
type Store interface {
	// Create 持久化一条新任务记录。
	Create(t *UploadTask) error
	// Get 按 id 查询任务;缺失时必须返回 ErrNotFound。
	Get(id string) (*UploadTask, error)
	// ListActiveByOwner 返回指定 owner 的活跃任务(uploading/paused/failed)。
	ListActiveByOwner(owner string) ([]UploadTask, error)
	// ListDueForGC 返回 expires_at > 0 且 <= now 的任务。
	ListDueForGC(now int64) ([]UploadTask, error)
	// UpdateOffset 更新已上传字节数及续期过期时间。
	UpdateOffset(id string, offset, expiresAt int64) error
	// SetStatus 更新任务状态及过期时间。
	SetStatus(id, status string, expiresAt int64) error
	// SetFailed 将任务标记为失败并记录错误信息。
	SetFailed(id, errMsg string, lastErrorAt, expiresAt int64) error
	// Delete 物理删除任务记录。
	Delete(id string) error
}

// Cancel 幂等取消指定任务。
//   - 任务不存在(ErrNotFound)、已完成(completed)、已取消(canceled) → 返回 (false, nil)。
//   - 其余状态(uploading/paused/failed) → 置 canceled、更新 expiresAt → 返回 (true, nil)。
func Cancel(s Store, id string, expiresAt int64) (bool, error) {
	t, err := s.Get(id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if t.Status == UploadStatusCompleted || t.Status == UploadStatusCanceled {
		return false, nil
	}
	if err := s.SetStatus(id, UploadStatusCanceled, expiresAt); err != nil {
		return false, err
	}
	return true, nil
}
