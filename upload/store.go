package upload

import "errors"

// Store is the persistence interface for upload tasks. Each service implements it on
// its own (GORM or raw SQL, either works); the common kernel (Cancel/SweepTasks/StartGC)
// depends only on this interface, not on any concrete driver.
type Store interface {
	// Create persists a new task record.
	Create(t *UploadTask) error
	// Get looks up a task by id; must return ErrNotFound when missing.
	Get(id string) (*UploadTask, error)
	// ListActiveByOwner returns the given owner's active tasks (uploading/paused/failed).
	ListActiveByOwner(owner string) ([]UploadTask, error)
	// ListDueForGC returns tasks with expires_at > 0 and <= now.
	ListDueForGC(now int64) ([]UploadTask, error)
	// UpdateOffset updates the uploaded byte offset and renews the expiry time.
	UpdateOffset(id string, offset, expiresAt int64) error
	// SetStatus updates the task's status and expiry time.
	SetStatus(id, status string, expiresAt int64) error
	// SetFailed marks the task as failed and records the error message.
	SetFailed(id, errMsg string, lastErrorAt, expiresAt int64) error
	// Delete physically removes the task record.
	Delete(id string) error
}

// Cancel idempotently cancels the given task.
//   - Task not found (ErrNotFound), already completed, or already canceled → returns (false, nil).
//   - Any other status (uploading/paused/failed) → set to canceled, update expiresAt → returns (true, nil).
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
