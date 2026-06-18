package upload

import (
	"encoding/json"
	"time"
)

// NewTask 从上传元数据构造一个新的 UploadTask 实例(不持久化)。
// 调用方负责把返回值传给 Store.Create。
//
// 参数说明:
//   - id:          tusd 或其它上传协议分配的上传 ID。
//   - ownerID:     经各服务 handler 取值后传入(NimoOS 用 user_id header,Photos 用 X-NimoOS-User-ID)。
//   - meta:        tusd MetaData / 其它 map;使用 "filename"/"targetPath"/"relativePath"/
//                  "filetype"/"fingerprint"/"batch_id"/"client_id" 等键。
//   - size:        文件总字节数。
//   - userAgent:   请求 User-Agent header。
//   - remoteAddr:  请求来源 IP。
//   - idleTimeout: 空闲超时秒数(通常 DefaultIdleTimeoutSeconds)。
//   - now:         当前时间(便于测试注入)。
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
