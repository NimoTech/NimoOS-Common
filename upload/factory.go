package upload

import (
	"encoding/json"
	"time"
)

// NewTask builds a new UploadTask instance from upload metadata (not persisted).
// The caller is responsible for passing the returned value to Store.Create.
//
// Parameters:
//   - id:          the upload ID assigned by tusd or another upload protocol.
//   - ownerID:     passed in after each service's handler extracts it (NimoOS uses
//     the user_id header, Photos uses X-NimoOS-User-ID).
//   - meta:        tusd MetaData / another map; uses the "filename"/"targetPath"/
//     "relativePath"/"filetype"/"fingerprint"/"batch_id"/"client_id" keys.
//   - size:        total file size in bytes.
//   - userAgent:   the request's User-Agent header.
//   - remoteAddr:  the request's source IP.
//   - idleTimeout: idle timeout in seconds (usually DefaultIdleTimeoutSeconds).
//   - now:         current time (for test injection).
func NewTask(id, ownerID string, meta map[string]string, size int64,
	userAgent, remoteAddr string, idleTimeout int64, now time.Time) *UploadTask {

	rel := meta["relativePath"]
	if rel == "" {
		rel = meta["filename"]
	}

	cm, _ := json.Marshal(map[string]string{
		"user_agent":  userAgent,
		"remote_addr": remoteAddr,
	})

	return &UploadTask{
		ID:           id,
		OwnerUserID:  ownerID,
		Filename:     meta["filename"],
		TargetPath:   meta["targetPath"],
		RelativePath: rel,
		Size:         size,
		Mime:         meta["filetype"],
		Fingerprint:  meta["fingerprint"],
		BatchID:      meta["batch_id"],
		ClientID:     meta["client_id"],
		ClientMeta:   string(cm),
		Status:       UploadStatusUploading,
		ExpiresAt:    now.Unix() + idleTimeout,
	}
}
