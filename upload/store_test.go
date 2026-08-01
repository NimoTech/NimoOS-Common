package upload

import (
	"testing"
	"time"
)

// fakeStore is an in-memory Store implementation used only for testing.
type fakeStore struct {
	m map[string]*UploadTask
}

func newFakeStore() *fakeStore {
	return &fakeStore{m: make(map[string]*UploadTask)}
}

func (f *fakeStore) Create(t *UploadTask) error {
	f.m[t.ID] = t
	return nil
}

func (f *fakeStore) Get(id string) (*UploadTask, error) {
	t, ok := f.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return t, nil
}

func (f *fakeStore) ListActiveByOwner(owner string) ([]UploadTask, error) {
	var result []UploadTask
	for _, t := range f.m {
		if t.OwnerUserID == owner &&
			(t.Status == UploadStatusUploading || t.Status == UploadStatusPaused || t.Status == UploadStatusFailed) {
			result = append(result, *t)
		}
	}
	return result, nil
}

func (f *fakeStore) ListDueForGC(now int64) ([]UploadTask, error) {
	var result []UploadTask
	for _, t := range f.m {
		if t.ExpiresAt > 0 && t.ExpiresAt <= now {
			result = append(result, *t)
		}
	}
	return result, nil
}

func (f *fakeStore) UpdateOffset(id string, offset, expiresAt int64) error {
	t, ok := f.m[id]
	if !ok {
		return ErrNotFound
	}
	t.Offset = offset
	t.ExpiresAt = expiresAt
	return nil
}

func (f *fakeStore) SetStatus(id, status string, expiresAt int64) error {
	t, ok := f.m[id]
	if !ok {
		return ErrNotFound
	}
	t.Status = status
	t.ExpiresAt = expiresAt
	return nil
}

func (f *fakeStore) SetFailed(id, errMsg string, lastErrorAt, expiresAt int64) error {
	t, ok := f.m[id]
	if !ok {
		return ErrNotFound
	}
	t.Status = UploadStatusFailed
	t.Error = errMsg
	t.LastErrorAt = lastErrorAt
	t.ExpiresAt = expiresAt
	return nil
}

func (f *fakeStore) Delete(id string) error {
	if _, ok := f.m[id]; !ok {
		return ErrNotFound
	}
	delete(f.m, id)
	return nil
}

// TestCancelIdempotent verifies Cancel's idempotent semantics.
func TestCancelIdempotent(t *testing.T) {
	now := time.Now()
	expires := now.Unix() + 3600

	t.Run("not found returns false nil", func(t *testing.T) {
		s := newFakeStore()
		ok, err := Cancel(s, "nonexistent", expires)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Fatal("expected false for not-found task")
		}
	})

	t.Run("uploading transitions to canceled returns true", func(t *testing.T) {
		s := newFakeStore()
		_ = s.Create(&UploadTask{ID: "t1", Status: UploadStatusUploading})
		ok, err := Cancel(s, "t1", expires)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Fatal("expected true when canceling uploading task")
		}
		got, _ := s.Get("t1")
		if got.Status != UploadStatusCanceled {
			t.Fatalf("expected canceled, got %s", got.Status)
		}
	})

	t.Run("already canceled returns false nil", func(t *testing.T) {
		s := newFakeStore()
		_ = s.Create(&UploadTask{ID: "t2", Status: UploadStatusCanceled})
		ok, err := Cancel(s, "t2", expires)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Fatal("expected false for already-canceled task")
		}
	})

	t.Run("completed returns false nil", func(t *testing.T) {
		s := newFakeStore()
		_ = s.Create(&UploadTask{ID: "t3", Status: UploadStatusCompleted})
		ok, err := Cancel(s, "t3", expires)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Fatal("expected false for completed task")
		}
	})

	t.Run("paused transitions to canceled returns true", func(t *testing.T) {
		s := newFakeStore()
		_ = s.Create(&UploadTask{ID: "t4", Status: UploadStatusPaused})
		ok, err := Cancel(s, "t4", expires)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Fatal("expected true when canceling paused task")
		}
		got, _ := s.Get("t4")
		if got.Status != UploadStatusCanceled {
			t.Fatalf("expected canceled, got %s", got.Status)
		}
	})

	t.Run("failed transitions to canceled returns true", func(t *testing.T) {
		s := newFakeStore()
		_ = s.Create(&UploadTask{ID: "t5", Status: UploadStatusFailed})
		ok, err := Cancel(s, "t5", expires)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Fatal("expected true when canceling failed task")
		}
		got, _ := s.Get("t5")
		if got.Status != UploadStatusCanceled {
			t.Fatalf("expected canceled, got %s", got.Status)
		}
	})
}
