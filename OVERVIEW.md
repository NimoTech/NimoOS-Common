# NimoOS-Common In Depth

NimoOS-Common is the shared base library used by all NimoOS microservices, providing core capabilities like logging, JWT, HTTP utilities, file operations, system management, and inter-service communication.

---

## Core Responsibilities

- Unified structured logging (zap wrapper, supports log rotation, takes over the zap global logger)
- JWT generation and verification (ECDSA P-256)
- Timeout-aware HTTP client utilities
- File and directory operation utilities
- System command execution (with injection protection)
- Port and network utilities
- systemd service management interface
- Client interface wrappers for each microservice (Gateway, MessageBus, UserService, KVM, etc.)
- Common kernel for the resumable upload engine (`upload/`, reused by tus-style upload services)
- Storage partition detection and recommended path allocation (`utils/storage/`)
- GPU detection and utilization collection (NVIDIA / Intel)
- Standard error codes and response format
- Version parsing and data migration utilities

---

## Directory Structure

```
NimoOS-Common/
├── model/                   # shared data models
│   ├── sys_common.go        # Result standard response struct
│   ├── device.go            # DeviceInfo (network, hardware)
│   ├── app_info.go           # ComposeApp metadata
│   └── gateway.go           # Route, ChangePortRequest, SSLConfigRequest/Response
├── upload/                  # common kernel for the resumable upload engine (model/state machine/Store interface/tiered GC)
├── utils/
│   ├── logger/              # zap logging wrapper (file rotation + ReplaceGlobals)
│   ├── jwt/                 # ECDSA JWT generation/verification/JWKS + Echo middleware
│   ├── http/                # timeout-aware HTTP client (Get/Post/Put/Delete)
│   ├── file/                # file/directory operations, archiving
│   ├── command/             # shell command execution
│   ├── exec/                # safe command execution (shell injection protection)
│   ├── port/                # port detection utilities
│   ├── systemctl/           # systemd service management (via dbus)
│   ├── ssh/                 # SSH client + WebSocket terminal
│   ├── storage/             # physical partition detection, storage path recommendation
│   ├── version/             # version parsing, migration status tracking
│   ├── random/              # random name/string generation
│   ├── constants/           # standard system path constants
│   └── common_err/          # standard error codes
├── middleware/
│   └── echo.go              # CORS middleware (Echo framework)
├── external/                # client interfaces for each microservice
│   ├── gateway.go           # Gateway route registration
│   ├── user_service.go      # fetch JWT public key
│   ├── message_bus.go       # event publishing
│   ├── app_manage.go        # App status queries
│   ├── notify.go            # system notification sending
│   ├── share.go             # Samba share deletion (NimoOS core service)
│   ├── kvm_service.go       # KVM service address discovery
│   ├── gpu.go               # NVIDIA GPU detection (nvidia-smi)
│   └── gpu_intel.go         # Intel GPU utilization collection (intel_gpu_top)
└── interfaces.go            # MigrationTool interface definition
```

---

## Core Modules In Depth

### Logging (logger)

Built on `go.uber.org/zap`, wraps file rotation:

- Automatic dual output to file + console
- Log rotation: 10MB max per file, 60 backups kept, 1-day retention
- Automatically injects caller info (function name, file, line number)
- **Takes over the zap global logger**: `LogInitWithWriterSyncers` calls `zap.ReplaceGlobals` at the end, so `zap.L()`/`zap.S()` point at the initialized instance (`utils/logger/log.go`). Downstream services (e.g. Photos) call `zap.L()` directly a lot; without this Replace those logs would all land in zap's default no-op logger and get silently dropped

```go
logger.LogInit("/var/log/nimoos", "nimoos", "log")
logger.Info("Service started", zap.String("service", "app"))
zap.L().Info("also lands in the same file/console sinks")
```

---

### JWT Utilities (jwt)

ECDSA P-256 (ES256) signing:

```go
// key pair generation
GenerateKeyPair() (*ecdsa.PrivateKey, *ecdsa.PublicKey, error)

// token issuance
GetAccessToken(username, privateKey, id)   // valid for 3 hours
GetRefreshToken(username, privateKey, id)  // valid for 7 days

// token verification
Validate(token string, publicKeyFunc) (bool, *Claims, error)

// Echo middleware (auto-skips localhost)
JWT(publicKeyFunc) echo.MiddlewareFunc

// JWKS endpoint
JWKSHandler(jwksJSON) http.Handler        // serves /.well-known/jwks.json
```

**Token extraction convention for the Echo middleware** (`JWT()` in `utils/jwt/jwt_helper.go`):

- Skipper: skips verification when `RealIP` is `127.0.0.1` / `::1` (localhost is exempt).
- Token source: prefers the **raw value of the `Authorization` header**, falling back to the `?token=` query parameter if missing.
- **Important**: the extractor does not strip the `"Bearer "` prefix — the header value is fed straight into ES256 verification, meaning only a **bare JWT** is currently accepted. This is exactly how NimoOS-UI's axios sends it (`headers.Authorization = token` in `NimoOS-UI/src/service/service.js` / `index.js`, no `Bearer ` prefix); a request with the standard `Bearer ` prefix would actually fail verification. Any change to `TokenLookupFuncs` must preserve compatibility with the bare JWT form; supporting the standard Bearer form means accepting both.
- On successful verification, `claims.ID` is written into the `user_id` request header for downstream handlers to read.

---

### HTTP Client (http)

Every request carries a timeout context to prevent goroutine leaks:

```go
Get(url, timeout)
Post(url, body, timeout)
Put(url, body, timeout)
Delete(url, body, timeout)
GetWithHeader(url, timeout, headers)
```

---

### File Utilities (file)

- File read/write, create, delete, rename
- Directory creation, deletion, size stats, empty-directory detection
- Recursive file/directory copy and move
- Archive operations (zip, tar.gz, tar.bz2, tar.xz, etc.)
- Collision-free file naming (auto-appends a number)

---

### Resumable Upload Kernel (upload)

A common kernel extracted from the upload services (`upload/`), with no dependency on gorm/tusd/echo — each caller brings its own storage and HTTP layer:

- `UploadTask`: source of truth for an upload task (table name `o_upload_tasks`; gorm tags are plain struct tag strings only, raw-SQL callers can ignore them)
- Status constants: `uploading` / `paused` / `failed` / `completed` / `canceled` (`upload/task.go`)
- `Store` interface + sentinel error `ErrNotFound`; `Cancel(s, id, expiresAt)` idempotent-cancel helper (`upload/store.go`)
- `NewTask(...)` factory (`upload/factory.go`)
- Tiered GC: `SweepTasks` does a single sweep pass + `StartGC` loops it periodically, cleaning up staging directories and expired tasks per `GCConfig{StagingDir, PausedTTL, GCIntervalSecs}`; write-side TTLs are passed in by the caller via the `Default*Seconds` constants (`upload/gc.go`, `upload/config.go`)

---

### Storage Allocation (storage)

`utils/storage/allocator.go`:

```go
GetPhysicalPartitions()    // parses /proc/mounts + syscall.Statfs, counting only physical/persistent filesystem partitions
RecommendStoragePaths()    // recommends on-disk paths for Docker/AppData/UserData/SystemData
```

---

### Safe Command Execution (exec)

Uses `google/safetext` to prevent shell injection:

```go
cmd := exec.Command("docker", "ps")
output, err := cmd.CombinedOutput()
```

---

### System Management Utilities (systemctl)

Manages services through the systemd dbus interface:

```go
systemctl.ListServices(pattern, wait)
systemctl.StartService(name, wait)
systemctl.StopService(name, wait)
systemctl.IsServiceRunning(name, wait)
systemctl.EnableService(nameOrPath, wait)
```

---

### Version & Migration (version)

```go
ParseVersion("v0.4.3-beta") // → major=0, minor=4, patch=3
Compare(v1, v2 string) int  // -1, 0, 1

// migration status tracking (stored under /var/lib/nimoos/migration/)
GetGlobalMigrationStatus(serviceName)
status.Done(version)
```

---

## External Service Clients (external)

Each service discovers other services' addresses by reading `/var/run/nimoos/*.url` files:

| Client | Function | Address file |
|---|---|---|
| `GetPublicKey(runtimePath)` | fetch UserService's JWT public key | `user-service.url` |
| `NewManagementService(path)` | Gateway route registration/port management | `management.url` |
| `NewNotifyService(path)` | send system notifications | `nimoos.url` |
| `NewAppManageService(path)` | query/update App status | `app-management.url` |
| `NewShareService(path)` | delete Samba shares (`/v1/samba/shares`) | `nimoos.url` |
| `GetKVMServiceAddress(runtimePath)` | KVM service address discovery | `kvm-service.url` |
| `PublishEventInSocket(...)` | publish events to MessageBus over a Unix socket | `message-bus.sock` |

When constructing clients like Gateway / AppManage, the code first waits for the address file to appear (up to 10 retries, 1s apart), then does a single health check against `/ping` (`external/common.go`).

**GPU collection** (does not go through `.url` discovery, calls local commands directly):

- `NvidiaGPUInfoList()`: queries NVIDIA GPUs via nvidia-smi (`external/gpu.go`)
- `StartIntelGpuMonitor()` + `GetIntelGpuStat()`: a background goroutine runs `intel_gpu_top -J -s 1000`, streaming and parsing it while keeping the latest frame (the max busy value across engines is used as overall utilization, plus power/frequency); returns not-ok when `intel_gpu_top` isn't present or the cache is more than 5s stale, so callers can degrade accordingly (`external/gpu_intel.go`)

---

## Standard Response Format

```go
type Result struct {
    Success int         // HTTP status code (200/400/401/500)
    Message string      // human-readable message
    Data    interface{} // response data
}
```

---

## Standard Error Codes

| Range | Category |
|---|---|
| 200/400/401/429/500 | HTTP status |
| 10001–10013 | user-related (password, account, permissions) |
| 20001–20006 | system-related (files, ports) |
| 40001–40005 | disk-related (formatting, mounting) |
| 50001–50004 | app-related (uninstall, image) |
| 60001–60005 | file operations |

---

## Standard System Paths

```go
DefaultConfigPath   = "/etc/nimoos"
DefaultDataPath     = "/var/lib/nimoos"
DefaultLogPath      = "/var/log/nimoos"
DefaultRuntimePath  = "/var/run/nimoos"
DefaultConstantPath = "/usr/share/nimoos"
```

---

## Tech Stack

- **Logging**: go.uber.org/zap + lumberjack
- **JWT**: golang-jwt/jwt v4 (ECDSA P-256)
- **Web**: labstack/echo v4
- **WebSocket**: gorilla/websocket
- **systemd**: coreos/go-systemd/v22
- **Command safety**: google/safetext
- **Archiving**: mholt/archiver v3
