package upload

const (
	DefaultIdleTimeoutSeconds = int64(6 * 60 * 60)
	DefaultPausedTTLSeconds   = int64(3 * 24 * 60 * 60)
	DefaultCanceledTTLSeconds = int64(60 * 60)
	DefaultGCIntervalSeconds  = int64(60 * 60)
)

// GCConfig only holds the fields SweepTasks actually uses; write-side TTLs
// (idle/canceled) are passed in by the caller via the Default*Seconds constants
// at NewTask/Cancel/SetStatus time.
type GCConfig struct {
	StagingDir string
	// When StagingDirs is non-nil, GC tries removeStaging against every
	// directory it returns — after per-volume staging routing (NimoOS
	// route/v2/tus_routing_store), leftovers may not be in StagingDir.
	// nil falls back to StagingDir only, matching the old behavior.
	StagingDirs    func() []string
	PausedTTL      int64
	GCIntervalSecs int64
}
