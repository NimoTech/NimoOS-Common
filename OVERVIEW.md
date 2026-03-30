# CasaOS-Common 详解

CasaOS-Common 是所有 CasaOS 微服务共用的基础库，提供日志、JWT、HTTP 工具、文件操作、系统管理、服务间通信等核心能力。

---

## 核心职责

- 统一的结构化日志（zap 封装，支持日志轮转）
- JWT 生成与验证（ECDSA P-256）
- 超时感知的 HTTP 客户端工具
- 文件和目录操作工具
- 系统命令执行（含注入防护）
- 端口和网络工具
- systemd 服务管理接口
- 各微服务的客户端接口封装（Gateway、MessageBus、UserService 等）
- 标准错误码与响应格式
- 版本解析与数据迁移工具

---

## 目录结构

```
CasaOS-Common/
├── model/                   # 共享数据模型
│   ├── sys_common.go        # Result 标准响应结构
│   ├── device.go            # DeviceInfo（网络、硬件）
│   ├── app_info.go          # ComposeApp 元数据
│   └── gateway.go           # Route、ChangePortRequest
├── utils/
│   ├── logger/              # zap 日志封装（文件轮转）
│   ├── jwt/                 # ECDSA JWT 生成/验证/JWKS
│   ├── http/                # 超时 HTTP 客户端（Get/Post/Put/Delete）
│   ├── file/                # 文件/目录操作、归档
│   ├── command/             # shell 命令执行
│   ├── exec/                # 安全命令执行（防 shell 注入）
│   ├── port/                # 端口探测工具
│   ├── systemctl/           # systemd 服务管理（via dbus）
│   ├── ssh/                 # SSH 客户端 + WebSocket 终端
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
│   └── gpu.go               # GPU 检测
└── interfaces.go            # MigrationTool 接口定义
```

---

## 核心模块详解

### 日志（logger）

基于 `go.uber.org/zap`，封装了文件轮转：

- 自动写入文件 + 控制台双输出
- 日志轮转：单文件最大 10MB，保留 60 个备份，1 天保留期
- 自动注入调用者信息（函数名、文件、行号）

```go
logger.LogInit("/var/log/casaos", "casaos", "log")
logger.Info("Service started", zap.String("service", "app"))
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

// 迁移状态追踪（存储于 /var/lib/casaos/migration/）
GetGlobalMigrationStatus(serviceName)
status.Done(version)
```

---

## 外部服务客户端（external）

各服务通过读取 `/var/run/casaos/*.url` 文件发现其他服务地址：

| 客户端 | 功能 | 地址文件 |
|---|---|---|
| `GetPublicKey(runtimePath)` | 获取 UserService JWT 公钥 | `user-service.url` |
| `NewManagementService(path)` | Gateway 路由注册/端口管理 | `management.url` |
| `NewNotifyService(path)` | 发送系统通知 | `casaos.url` |
| `NewAppManageService(path)` | 查询/更新 App 状态 | `app-management.url` |
| `PublishEventInSocket(...)` | 通过 Unix socket 发布事件到 MessageBus | `message-bus.sock` |

所有客户端构造时自动做健康 ping，连接失败时重试（最多 10 次）。

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
DefaultConfigPath   = "/etc/casaos"
DefaultDataPath     = "/var/lib/casaos"
DefaultLogPath      = "/var/log/casaos"
DefaultRuntimePath  = "/var/run/casaos"
DefaultConstantPath = "/usr/share/casaos"
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
