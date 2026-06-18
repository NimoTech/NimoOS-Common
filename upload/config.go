package upload

const (
	DefaultIdleTimeoutSeconds = int64(6 * 60 * 60)
	DefaultPausedTTLSeconds   = int64(3 * 24 * 60 * 60)
	DefaultCanceledTTLSeconds = int64(60 * 60)
	DefaultGCIntervalSeconds  = int64(60 * 60)
)

// GCConfig 由各服务注入(尤其 StagingDir 各不相同)。
type GCConfig struct {
	StagingDir     string
	IdleTimeout    int64
	PausedTTL      int64
	CanceledTTL    int64
	GCIntervalSecs int64
}
