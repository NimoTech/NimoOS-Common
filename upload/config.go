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
	StagingDir     string
	PausedTTL      int64
	GCIntervalSecs int64
}
