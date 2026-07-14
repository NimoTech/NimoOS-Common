# NimoOS-Common 详解

NimoOS-Common 是所有 NimoOS 微服务共用的基础库，提供日志、JWT、HTTP 工具、文件操作、系统管理、服务间通信等核心能力。

---

## 核心职责

- 统一的结构化日志（zap 封装，支持日志轮转，并接管 zap 全局 logger）
- JWT 生成与验证（ECDSA P-256）
- 超时感知的 HTTP 客户端工具
- 文件和目录操作工具
- 系统命令执行（含注入防护）
- 端口和网络工具
- systemd 服务管理接口
- 各微服务的客户端接口封装（Gateway、MessageBus、UserService、KVM 等）
- 可恢复上传引擎通用内核（`upload/`，供 tus 类上传服务复用）
- 存储分区探测与推荐路径分配（`utils/storage/`）
- GPU 探测与利用率采集（NVIDIA / Intel）
- 标准错误码与响应格式
- 版本解析与数据迁移工具

---

## 目录结构

```
NimoOS-Common/
├── model/                   # 共享数据模型
│   ├── sys_common.go        # Result 标准响应结构
│   ├── device.go            # DeviceInfo（网络、硬件）
│   ├── app_info.go          # ComposeApp 元数据
│   └── gateway.go           # Route、ChangePortRequest、SSLConfigRequest/Response
├── upload/                  # 可恢复上传引擎通用内核（模型/状态机/Store 接口/分级 GC）
├── utils/
│   ├── logger/              # zap 日志封装（文件轮转 + ReplaceGlobals）
│   ├── jwt/                 # ECDSA JWT 生成/验证/JWKS + Echo 中间件
│   ├── http/                # 超时 HTTP 客户端（Get/Post/Put/Delete）
│   ├── file/                # 文件/目录操作、归档
│   ├── command/             # shell 命令执行
│   ├── exec/                # 安全命令执行（防 shell 注入）
│   ├── port/                # 端口探测工具
│   ├── systemctl/           # systemd 服务管理（via dbus）
│   ├── ssh/                 # SSH 客户端 + WebSocket 终端
│   ├── storage/             # 物理分区探测、存储路径推荐
│   ├── version/             # 版本解析、迁移状态追踪
│   ├── random/              # 随机名称/字符串生成
│   ├── constants/           # 系统标准路径常量
│   └── common_err/          # 标准错误码
├── middleware/
│   └── echo.go              # CORS 中间件（Echo 框架）
├── external/                # 各微服务客户端接口
│   ├── gateway.go           # Gateway 路由注册
│   ├── user_service.go      # 获取 JWT 公钥
│   ├── message_bus.go       # 事件发布
│   ├── app_manage.go        # App 状态查询
│   ├── notify.go            # 系统通知发送
│   ├── share.go             # Samba 共享删除（NimoOS 核心服务）
│   ├── kvm_service.go       # KVM 服务地址发现
│   ├── gpu.go               # NVIDIA GPU 检测（nvidia-smi）
│   └── gpu_intel.go         # Intel GPU 利用率采集（intel_gpu_top）
└── interfaces.go            # MigrationTool 接口定义
```

---

## 核心模块详解

### 日志（logger）

基于 `go.uber.org/zap`，封装了文件轮转：

- 自动写入文件 + 控制台双输出
- 日志轮转：单文件最大 10MB，保留 60 个备份，1 天保留期
- 自动注入调用者信息（函数名、文件、行号）
- **接管 zap 全局 logger**：`LogInitWithWriterSyncers` 末尾调用 `zap.ReplaceGlobals`，使 `zap.L()`/`zap.S()` 指向已初始化实例（`utils/logger/log.go`）。下游服务（如 Photos）大量直接使用 `zap.L()`，不 Replace 的话这些日志会全部进入 zap 默认的 no-op logger 被静默丢弃

```go
logger.LogInit("/var/log/nimoos", "nimoos", "log")
logger.Info("Service started", zap.String("service", "app"))
zap.L().Info("also lands in the same file/console sinks")
```

---

### JWT 工具（jwt）

ECDSA P-256（ES256）签名：

```go
// 密钥对生成
GenerateKeyPair() (*ecdsa.PrivateKey, *ecdsa.PublicKey, error)

// 令牌签发
GetAccessToken(username, privateKey, id)   // 3 小时有效
GetRefreshToken(username, privateKey, id)  // 7 天有效

// 令牌验证
Validate(token string, publicKeyFunc) (bool, *Claims, error)

// Echo 中间件（自动跳过 localhost）
JWT(publicKeyFunc) echo.MiddlewareFunc

// JWKS 端点
JWKSHandler(jwksJSON) http.Handler        // 服务 /.well-known/jwks.json
```

**Echo 中间件的 token 提取约定**（`utils/jwt/jwt_helper.go` 的 `JWT()`）：

- Skipper：`RealIP` 为 `127.0.0.1` / `::1` 时跳过验证（localhost 免验）。
- token 来源：优先取 **`Authorization` 头的原始整值**，缺失时回退 `?token=` 查询参数。
- **重要**：提取器不剥离 `"Bearer "` 前缀，头的值被原样送进 ES256 验签——即当前只接受**裸 JWT**。NimoOS-UI 的 axios 正是这样发的（`NimoOS-UI/src/service/service.js` / `index.js` 中 `headers.Authorization = token`，无 `Bearer ` 前缀），带标准 `Bearer ` 前缀的请求反而会验签失败。改动 `TokenLookupFuncs` 时必须保持对裸 JWT 的兼容；若要支持标准 Bearer 形式，须两种形式都接受。
- 验证通过后把 `claims.ID` 写入请求头 `user_id`，供下游 handler 读取。

---

### HTTP 客户端（http）

所有请求携带超时上下文，防止 goroutine 泄漏：

```go
Get(url, timeout)
Post(url, body, timeout)
Put(url, body, timeout)
Delete(url, body, timeout)
GetWithHeader(url, timeout, headers)
```

---

### 文件工具（file）

- 文件读写、创建、删除、重命名
- 目录创建、删除、大小统计、空目录检测
- 文件/目录递归复制与移动
- 归档操作（zip、tar.gz、tar.bz2、tar.xz 等）
- 无重名冲突的文件命名（自动追加编号）

---

### 可恢复上传内核（upload）

从上传服务中抽取的通用内核（`upload/`），不依赖 gorm/tusd/echo，供各使用方自带存储与 HTTP 层：

- `UploadTask`：上传任务真相源（表名 `o_upload_tasks`；gorm tag 仅为纯 struct tag 字符串，原生 SQL 使用方可忽略）
- 状态常量：`uploading` / `paused` / `failed` / `completed` / `canceled`（`upload/task.go`）
- `Store` 接口 + 哨兵错误 `ErrNotFound`；`Cancel(s, id, expiresAt)` 幂等取消助手（`upload/store.go`）
- `NewTask(...)` 工厂（`upload/factory.go`）
- 分级 GC：`SweepTasks` 单轮清扫 + `StartGC` 周期循环，按 `GCConfig{StagingDir, PausedTTL, GCIntervalSecs}` 清理暂存目录与过期任务；写侧 TTL 由调用方用 `Default*Seconds` 常量传入（`upload/gc.go`、`upload/config.go`）

---

### 存储分配（storage）

`utils/storage/allocator.go`：

```go
GetPhysicalPartitions()    // 解析 /proc/mounts + syscall.Statfs，仅统计物理/持久文件系统分区
RecommendStoragePaths()    // 推荐 Docker/AppData/UserData/SystemData 落盘路径
```

---

### 安全命令执行（exec）

使用 `google/safetext` 防止 shell 注入：

```go
cmd := exec.Command("docker", "ps")
output, err := cmd.CombinedOutput()
```

---

### 系统管理工具（systemctl）

通过 systemd dbus 接口管理服务：

```go
systemctl.ListServices(pattern, wait)
systemctl.StartService(name, wait)
systemctl.StopService(name, wait)
systemctl.IsServiceRunning(name, wait)
systemctl.EnableService(nameOrPath, wait)
```

---

### 版本与迁移（version）

```go
ParseVersion("v0.4.3-beta") // → major=0, minor=4, patch=3
Compare(v1, v2 string) int  // -1, 0, 1

// 迁移状态追踪（存储于 /var/lib/nimoos/migration/）
GetGlobalMigrationStatus(serviceName)
status.Done(version)
```

---

## 外部服务客户端（external）

各服务通过读取 `/var/run/nimoos/*.url` 文件发现其他服务地址：

| 客户端 | 功能 | 地址文件 |
|---|---|---|
| `GetPublicKey(runtimePath)` | 获取 UserService JWT 公钥 | `user-service.url` |
| `NewManagementService(path)` | Gateway 路由注册/端口管理 | `management.url` |
| `NewNotifyService(path)` | 发送系统通知 | `nimoos.url` |
| `NewAppManageService(path)` | 查询/更新 App 状态 | `app-management.url` |
| `NewShareService(path)` | 删除 Samba 共享（`/v1/samba/shares`） | `nimoos.url` |
| `GetKVMServiceAddress(runtimePath)` | KVM 服务地址发现 | `kvm-service.url` |
| `PublishEventInSocket(...)` | 通过 Unix socket 发布事件到 MessageBus | `message-bus.sock` |

Gateway / AppManage 等客户端构造时先等待地址文件出现（最多重试 10 次、间隔 1s），再向 `/ping` 做一次健康检查（`external/common.go`）。

**GPU 采集**（不走 `.url` 发现，直接调本机命令）：

- `NvidiaGPUInfoList()`：经 nvidia-smi 查询 NVIDIA GPU（`external/gpu.go`）
- `StartIntelGpuMonitor()` + `GetIntelGpuStat()`：后台 goroutine 跑 `intel_gpu_top -J -s 1000` 流式解析，维护最新一帧（engines 最大 busy 作为整体利用率，附功率/频率）；`intel_gpu_top` 不存在或缓存超过 5s 时返回 not-ok，调用方据此降级（`external/gpu_intel.go`）

---

## 标准响应格式

```go
type Result struct {
    Success int         // HTTP 状态码（200/400/401/500）
    Message string      // 可读消息
    Data    interface{} // 响应数据
}
```

---

## 标准错误码

| 范围 | 类别 |
|---|---|
| 200/400/401/429/500 | HTTP 状态 |
| 10001–10013 | 用户相关（密码、账户、权限） |
| 20001–20006 | 系统相关（文件、端口） |
| 40001–40005 | 磁盘相关（格式化、挂载） |
| 50001–50004 | 应用相关（卸载、镜像） |
| 60001–60005 | 文件操作 |

---

## 系统标准路径

```go
DefaultConfigPath   = "/etc/nimoos"
DefaultDataPath     = "/var/lib/nimoos"
DefaultLogPath      = "/var/log/nimoos"
DefaultRuntimePath  = "/var/run/nimoos"
DefaultConstantPath = "/usr/share/nimoos"
```

---

## 技术栈

- **日志**：go.uber.org/zap + lumberjack
- **JWT**：golang-jwt/jwt v4（ECDSA P-256）
- **Web**：labstack/echo v4
- **WebSocket**：gorilla/websocket
- **systemd**：coreos/go-systemd/v22
- **命令安全**：google/safetext
- **归档**：mholt/archiver v3
