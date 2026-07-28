package upload

const (
	DefaultIdleTimeoutSeconds = int64(6 * 60 * 60)
	DefaultPausedTTLSeconds   = int64(3 * 24 * 60 * 60)
	DefaultCanceledTTLSeconds = int64(60 * 60)
	DefaultGCIntervalSeconds  = int64(60 * 60)
)

// GCConfig 仅含 SweepTasks 实际使用的字段;写侧 TTL(idle/canceled)由调用方用
// Default*Seconds 常量在 NewTask/Cancel/SetStatus 时传入。
type GCConfig struct {
	StagingDir string
	// StagingDirs 非 nil 时,GC 删除任务残片会对返回的每个目录都尝试
	// removeStaging——per-volume 暂存路由(NimoOS route/v2/tus_routing_store)
	// 之后残片可能不在 StagingDir。nil 退化为仅 StagingDir,与旧行为一致。
	StagingDirs    func() []string
	PausedTTL      int64
	GCIntervalSecs int64
}
