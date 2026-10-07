# mcp-tools

![CI](https://github.com/v2up-32mb/mcp-tools/actions/workflows/ci.yml/badge.svg)

一个基于 Go 1.20 的单体 MCP HTTP 服务，提供文件系统、Git、Go 导航与命令执行能力，供远程 AI 代理通过 MCP 调用本地工具。

> 目标平台：**Windows 7 SP1 x64**（Go 1.20 是支持 Win7 的最后一个 Go 版本；`CGO_ENABLED=0`、`GOAMD64=v1` 单文件二进制，无需 Python / Node / .NET 运行时）

> ⚠️ **安全警告：`unsafe_allow_all` 默认启用**
>
> 本仓库默认配置 `unsafe_allow_all: true`，启用后：
> - `fs.*` 工具可访问 `allowed_roots` 之外的任意文件路径
> - `exec_run` 可执行任意 `command + args + env`（不再限于 Go preset）
> - `exec_shell` / `exec_start_process` / `exec_list_processes` / `exec_process_logs` / `exec_stop_process` / `exec_remove_process` 可用（任意 shell 命令 + 后台进程管理）
> - `git.*` 的 `repo_path` 可以指向任意 Git 仓库
> - 模板的 `requires_confirmation` 和 `allowed_workdirs` 限制被旁路
>
> 所有操作仍需有效的 Bearer Token 并保留审计日志，但路径和命令限制被完全放开。
> **在生产环境部署时，请通过环境变量 `MCP_UNSAFE_ALLOW_ALL=false` 或配置文件 `unsafe_allow_all: false` 关闭此模式。**
> 服务启动时会在控制台日志中输出显著 WARN 告警。

当前默认开启：

```yaml
unsafe_allow_all: true
```

也就是：

- 保留 Bearer Token
- 保留控制台日志
- 保留 JSONL 审计日志
- 但对**已授权 MCP 客户端**放开文件路径、工作目录、Git 仓库与原始命令执行限制

补充维护文档：

- `AGENTS.md`：给 AI 代理/人工调用方的使用与边界说明
- `TOOLS-DEFINE.md`：给维护者的内部工具实现算法与约束说明
- `CHANGELOG.md`：版本变更记录
- `docs/releases/v1.0.0.md`：`v1.0.0` 发布说明

## 已实现能力

- **HTTP JSON 模式**
  - `POST /mcp`
  - Bearer Token 鉴权
  - `initialize` 创建会话，返回 `Mcp-Session-Id`
  - `tools/list` / `tools/call` / `resources/list` / `resources/templates/list` / `resources/read` / `resources/subscribe` / `resources/unsubscribe` / `prompts/list` / `prompts/get` / `ping`
  - `completion/complete`
- **SSE / Streamable HTTP 模式**
  - `POST /mcp` + `Accept: text/event-stream`：单次 SSE 响应
  - `GET /mcp` + `Accept: text/event-stream`：按 session 建立长连接事件流
  - `Accept` 会尊重 media range 的 `q=0`；若 JSON 与 SSE 都可接受，则仍优先 JSON 以兼容 mixed Accept 客户端
  - 如果服务重启导致旧 `Mcp-Session-Id` 已失效，服务端会把这类 stream 请求按“未初始化 session”处理并提示重新 initialize，而不是直接返回 `invalid or expired session`
  - 当某个 session 已有活动 SSE stream 时，后续 `POST /mcp` + `Accept: text/event-stream` 会把结果**异步投递**到该 stream，并返回 `202 Accepted`
  - 异步投递同样覆盖 `tools/*`、`resources/*`、`prompts/*`
- **文件工具**
  - `fs_read_file`
  - `fs_write_file`
  - `fs_list_dir`
  - `fs_stat_path`
  - `fs_make_dir`
  - `fs_move_path`
  - `fs_delete_path`
  - `fs_search_text`
  - `fs_find_files`
  - `fs_replace_text`
  - `fs_apply_unified_diff`
  - `fs_edit_lines`
  - `fs_pull_file`（签发短时效下载 URL，客户端自行下载保存）
- **Git 工具**
  - `git_status`
  - `git_diff`
  - `git_log`
  - `git_add`
  - `git_restore`
  - `git_commit`
  - `git_branch`
  - `git_switch`
  - `git_pull`
- **Go 导航工具**
  - `go_list_symbols`
  - `go_find_definition`
- **Exec 工具**
  - `exec_run`
  - `exec_run_template`
  - `exec_shell`（`unsafe_allow_all=true` 时可用，支持管道/重定向/环境变量展开）
  - `exec_start_process` / `exec_list_processes` / `exec_process_logs` / `exec_stop_process` / `exec_remove_process`（后台进程管理，`unsafe_allow_all=true` 时可用）
- 仅开放 Go preset：`go_fmt` / `go_mod_download` / `go_test` / `go_generate` / `go_build` / `go_vet` / `go_mod_tidy` / `go_get` / `go_list` / `go_work_sync`
- 同时支持固定白名单命令模板：`make_test` / `make_build` / `go_clean_testcache`（默认要求 `confirm=true`）
- 文件拉取：`fs_pull_file` 签发 HMAC 签名、短时效、可限次的下载 URL，客户端通过 `GET /file/<token>` 流式下载；支持扩展名白名单与大小上限
- 审计日志：JSON Lines
- 目录边界：启动目录 + `allowed_roots`
- 浏览器 Origin 拒绝：未配置 `allowed_origins` / `MCP_ALLOWED_ORIGINS` 时默认不接受浏览器来源请求
- `/debug/statez` 现在还会暴露 `exec_presets` / `command_templates` 的元信息摘要（包括 `env_keys`）
- `initialize` capabilities 当前会显式声明：
  - `tools.listChanged=false`
  - `resources.subscribe=true`
  - `resources.listChanged=false`
  - `prompts.listChanged=false`
  - `completions`

目前服务**不会**发送：

- `notifications/tools/list_changed`
- `notifications/resources/list_changed`
- `notifications/prompts/list_changed`

因为当前实现中工具集、资源模板与 prompt 列表都不是运行时动态注册的，所以 `listChanged=false` 与现状一致。

另外，`notifications/*` 请求当前统一按 notification 语义处理：

- 返回 `202 Accepted`
- 不返回 JSON-RPC body
- 如果请求携带了有效 session，则响应头仍会带 `MCP-Protocol-Version`

## 配置方式

现在支持三层配置来源：

1. **默认值**
2. **默认 home 配置文件**
3. **显式 YAML / 环境变量覆盖**

优先级：

```text
defaults < 默认 home 配置文件 < YAML < environment variables
```

这意味着：

- 如果你没有传 `-config`，也没有设置 `MCP_CONFIG_FILE`，服务会自动尝试读取默认配置文件
- 可以把大部分稳定配置放进 YAML
- 用环境变量覆盖敏感项或部署时差异项，例如 token、监听地址
- 默认总是把**服务启动目录**和 **~/.mcp-tools** 加入允许根目录
- 默认审计日志会写到 **~/.mcp-tools/mcp-audit.jsonl**，避免污染当前仓库目录

## YAML 配置文件

示例文件：

- `mcp-tools.minimal.yaml`：最小可用配置，适合先本地跑起来
- `mcp-tools.example.yaml`：带详细注释的完整示例

默认配置文件位置：

- Linux / macOS：`~/.mcp-tools/config.yaml`
- Windows：`%USERPROFILE%\.mcp-tools\config.yaml`

说明：

- 代码里使用 `os.UserHomeDir()` + `filepath.Join(...)` 计算路径
- 所以会自动兼容 Windows 的路径分隔符
- 如果默认路径下存在配置文件，在**未显式传 `-config`** 且**未设置 `MCP_CONFIG_FILE`** 时，会自动加载
- YAML 必须是单文档配置；空文件/仅注释文件允许存在并表示“不做 YAML 覆盖”
- 显式 `null` / `~`、空字段值或空列表项不是有效配置值，会在启动或 `-validate-config` 时直接报错
- YAML 会拒绝未知字段，配置拼写错误或额外文档会在启动或 `-validate-config` 时直接暴露

### 你通常先只需要关心 3 个字段

#### `bearer_token`

必配。所有客户端都要带：

```text
Authorization: Bearer <token>
```

#### `listen_addr`

决定服务监听范围：

- `127.0.0.1:8080`：只本机可连，推荐本地测试默认用这个
- `0.0.0.0:8080`：所有网卡都监听，适合远程 agent 接入，但风险更高

#### `log_level`

控制台日志级别，默认：

```yaml
log_level: INFO
```

可选值：

- `INFO`
- `WARN`
- `ERROR`
- `DEBUG`

行为：

- `INFO`
  - 服务启动/停止
  - 成功工具调用
  - 重要运行状态
- `WARN`
  - 非致命异常
- `ERROR`
  - 工具失败
  - 关键服务错误
- `DEBUG`
  - 脱敏的 HTTP/MCP 调试细节
  - 更完整的请求/协议字段

不会打印完整 Bearer Token。
`INFO/WARN` 默认不带 `request_id` / `session_id`；`ERROR/DEBUG` 才会带追踪字段。

#### `unsafe_allow_all`

当前 `yolo` 分支默认：

```yaml
unsafe_allow_all: true
```

语义：

- `true`
  - 允许 `fs.*` 访问 `allowed_roots` 外路径
  - 允许 `exec_run` 用 `command + args + env` 执行原始命令
  - 允许 `git.*` / `go.*` 脱离 `allowed_roots` 约束
- `false`
  - 退回主线分支那套白名单/允许目录约束

这不会关闭：

- Bearer Token 鉴权
- 控制台日志
- 审计日志

#### `allowed_roots`

额外允许访问的目录白名单。

注意：

- 服务启动目录会**自动加入**
- `~/.mcp-tools` 也会**自动加入**
- `allowed_roots` 是“附加目录”，不是完整覆盖列表
- `allowed_roots` / `MCP_ALLOWED_ROOTS` 条目不能为空；空字符串或尾随逗号会按配置错误拒绝

### `allowed_origins` 到底是什么？

这是最容易误解的配置项。

它控制的是：

- **浏览器请求里的 `Origin`**
- 也就是 **CORS / 浏览器来源限制**

它**不是**：

- 主机名白名单
- IP 白名单
- 入站网络 ACL

#### 如果配错会怎样？

如果你的 MCP 客户端是浏览器页面 / Web UI，这类请求通常会带 `Origin` 头。  
这时如果 `allowed_origins` 不匹配，服务端会直接拒绝：

```text
403 origin not allowed
```

#### 如果不配置、留空会怎样？

准确行为是：

- **没有 `Origin` 头的请求**：允许继续
- **带 `Origin` 头的请求**：会被拒绝

所以：

- CLI / Node / Go / Python 后端客户端通常不受影响
- 浏览器前端会受影响

#### 什么时候需要配？

只有你通过浏览器 UI / Web 前端接入时，才通常需要配。

例如：

```yaml
allowed_origins:
  - http://localhost:3000
  - https://your-ui.example
```

#### 什么时候可以不配？

如果你是：

- 本地 CLI
- 本地 AI agent
- 服务端程序
- Node/Go/Python MCP 客户端

通常可以先不配 `allowed_origins`。

常用字段：

- `listen_addr`
- `bearer_token`
- `allowed_roots`
- `log_level`
- `unsafe_allow_all`
- `allowed_origins`
- `audit_log_path`
- `audit_rotate_max_mb`
- `audit_rotate_max_backups`
- `command_timeout_sec`
- `output_max_bytes`
- `stream_queue_size`
- `max_request_bytes`
- `read_header_timeout_sec`
- `read_timeout_sec`
- `write_timeout_sec`
- `idle_timeout_sec`
- `session_ttl_min`
- `server_name`
- `server_version`
- `supported_protocols`
- `git.allowed_subcommands`
- `exec.presets`
- `exec.command_templates`
- `pull_file.enabled`
- `pull_file.allowed_extensions`
- `pull_file.max_bytes`
- `pull_file.url.ttl_sec`
- `pull_file.url.max_downloads`
- `pull_file.url.public_base_url`

说明：

- YAML 中的相对路径，按**配置文件所在目录**解析
- `allowed_roots` 是**附加白名单目录**；启动目录始终保留
- `allowed_roots` 列表项不能为空，避免拼写/模板错误被静默忽略
- 默认 `audit_log_path`：
  - Linux / macOS：`~/.mcp-tools/mcp-audit.jsonl`
  - Windows：`%USERPROFILE%\\.mcp-tools\\mcp-audit.jsonl`
- `audit.NewJSONLWriter` 会自动创建缺失的父目录
- 审计日志支持按大小滚动：
  - `audit_rotate_max_mb`
  - `audit_rotate_max_backups`
  - 轮转文件名形如 `mcp-audit.jsonl.1`、`mcp-audit.jsonl.2`
- 审计日志中的工具参数是摘要化记录：大文本/补丁只记录大小、行数和 sha256，`env` 只记录 key，不记录完整值
- `allowed_origins` 是浏览器 `Origin` 白名单；**不是 hostname 入站控制**；列表项不能为空或纯空白
- `supported_protocols` 若显式配置，不能为空列表；`[]` 会被拒绝
- `git.allowed_subcommands` 可显式配置为 `[]` 来禁用全部 Git 子命令，列表项本身不能为空，且只支持当前实现已有的白名单子命令，**不支持 `push`**
- `command_timeout_sec` 是 exec 默认超时；未显式设置 `timeout_sec` 的 preset/template 会继承它
- `exec.presets.<name>.enabled: false` 可禁用内置 preset
- `exec.presets.<name>.command` 不能为空；裸命令名（如 `go`）按 `PATH` 查找，带 `/` 或 `\` 的相对命令路径按配置文件目录解析
- `exec.presets.<name>.fixed_args` / `allowed_args` 可为空列表，但列表项本身不能为空或纯空白
- `exec.command_templates.<name>` 可声明固定 argv 的模板命令，供 `exec_run_template` 调用
- `exec.command_templates.<name>.command` 的 argv 片段都不能为空；首个 argv 同样区分裸命令名与 path-like 相对路径，后续 argv 保持原样
- `exec.command_templates.<name>.env` 可配置模板级固定环境变量
- `exec.command_templates.<name>.allowed_workdirs` 可限制模板只允许在指定工作目录范围内执行；其中的相对路径同样按配置文件目录解析
- `exec.command_templates.<name>.category` 可声明模板类别（如 build/test/cleanup）
- `exec.command_templates.<name>.destructive` 可标识模板是否具有破坏性副作用
- `exec.command_templates.<name>.requires_confirmation` 会要求调用方显式传 `confirm=true` 才执行
- `pull_file.enabled` 默认 `true`；设为 `false` 后 `fs_pull_file` 直接报错且 `/file/*` 返回 404
- `pull_file.allowed_extensions` 空列表 = 不限制类型；条目必须以 `.` 开头、大小写不敏感（如 `.png`、`.JPG`），列表项不能为空
- `pull_file.max_bytes` 默认 10 MiB；调用方 `max_bytes` 参数只能进一步收紧，不能放宽
- `pull_file.url.ttl_sec` 默认 300 秒；`pull_file.url.max_downloads` 默认 0 = 不限制次数（仅受 TTL 约束），大于 0 时与 TTL 双条件、先到先失效
- `pull_file.url.public_base_url` 留空时工具返回相对路径 `/file/<token>`，由客户端用自身 MCP base URL 拼接；反代/公网部署时配置绝对 `http(s)` 地址（不能带 query/fragment）即可拿到绝对 URL

## 启动

### 方式 1：仅环境变量

```bash
export MCP_BEARER_TOKEN='change-me'
export MCP_LISTEN_ADDR='0.0.0.0:8080'
export MCP_ALLOWED_ROOTS='/tmp,/root/projects'
# 可选：只允许这些浏览器来源
# export MCP_ALLOWED_ORIGINS='https://your-ui.example'

go run ./cmd/mcp-tools
```

### 方式 4：启动前校验配置

```bash
go run ./cmd/mcp-tools -config ./mcp-tools.minimal.yaml -validate-config
```

这会打印**生效后的配置摘要**并退出，适合 CI/CD、容器启动前检查。

### 方式 2：YAML + `-config`

```bash
go run ./cmd/mcp-tools -config ./mcp-tools.minimal.yaml
```

### 方式 3：YAML + 环境变量覆盖

```bash
export MCP_CONFIG_FILE=./mcp-tools.minimal.yaml
export MCP_BEARER_TOKEN='override-token'

go run ./cmd/mcp-tools
```

## 环境变量

- `MCP_CONFIG_FILE`：显式指定 YAML 配置文件路径；若未设置则会尝试默认 home 配置文件
- `MCP_BEARER_TOKEN`：Bearer Token；若 YAML 未配置则必填
- `MCP_LOG_LEVEL`：控制台日志级别，支持 `INFO/WARN/ERROR/DEBUG`
- `MCP_UNSAFE_ALLOW_ALL`：是否开启满权限模式，支持 `true/false`，当前 `yolo` 分支默认 `true`
- `MCP_LISTEN_ADDR`：默认 `0.0.0.0:8080`
- `MCP_ALLOWED_ROOTS`：逗号分隔的额外允许目录；空段（例如尾随逗号）会按配置错误拒绝
- `MCP_ALLOWED_ORIGINS`：逗号分隔的允许浏览器来源；空段（例如尾随逗号）会按配置错误拒绝
- `MCP_AUDIT_LOG_PATH`：默认：
  - Linux / macOS：`~/.mcp-tools/mcp-audit.jsonl`
  - Windows：`%USERPROFILE%\\.mcp-tools\\mcp-audit.jsonl`
- `MCP_AUDIT_ROTATE_MAX_MB`：审计日志滚动大小阈值（MiB），默认 `10`
- `MCP_AUDIT_ROTATE_MAX_BACKUPS`：审计日志最多保留的旧文件数，默认 `5`
- `MCP_COMMAND_TIMEOUT_SEC`：默认 `30`，会更新未显式设置 `timeout_sec` 的默认 exec preset/template
- `MCP_OUTPUT_MAX_BYTES`：默认 `65536`
- `MCP_STREAM_QUEUE_SIZE`：单个 SSE stream 的内部队列容量，默认 `128`
- `MCP_MAX_REQUEST_BYTES`：默认 `1048576`
- `MCP_READ_HEADER_TIMEOUT_SEC`：默认 `5`
- `MCP_READ_TIMEOUT_SEC`：默认 `15`
- `MCP_WRITE_TIMEOUT_SEC`：默认 `30`
- `MCP_IDLE_TIMEOUT_SEC`：默认 `60`
- `MCP_SESSION_TTL_MIN`：默认 `120`
- `MCP_SERVER_NAME`：默认 `mcp-tools`
- `MCP_SERVER_VERSION`：默认 `1.0.0`

## 传输层说明

### 生产默认护栏

- `POST /mcp` 请求体大小默认限制为 `1 MiB`
- SSE stream 内部消息队列默认容量为 `128`
- `ReadHeaderTimeout=5s`
- `ReadTimeout=15s`
- `WriteTimeout=0`（SSE 长连接需要不设写超时；exec 工具通过 context.Timeout 自行控制执行时间）
- `IdleTimeout=60s`
- 提供 `/healthz` 与 `/readyz`
- 提供 Bearer Token 保护的 `/debug/statez`，用于查看运行态计数和 transport 观测数据

### `/debug/statez`

只读调试接口：

- `GET /debug/statez`
- 需要 `Authorization: Bearer <token>`
- 返回 JSON，包含：
  - `config`
  - `runtime`
  - `counters`

目前重点可观测字段包括：

- `runtime.active_sessions`
- `runtime.active_stream_sessions`
- `runtime.active_stream_connections`
- `runtime.resource_subscription_sessions`
- `runtime.resource_subscription_total`
- `counters.async_stream_attempts`
- `counters.async_stream_published`
- `counters.async_stream_fallback_single_shot`
- `counters.resource_notification_attempts`
- `counters.resource_notification_sent`
- `counters.resource_notification_dropped`

### 模式 1：普通 JSON

适合传统 request-response MCP 客户端。

- 请求：`POST /mcp`
- 响应：`application/json`
- 每个请求同步返回一个 JSON-RPC 结果
- 请求正文必须是单个 JSON-RPC 对象；尾随的第二个 JSON 值或垃圾内容会按 parse error 拒绝
- 顶层数组（JSON-RPC batch）或标量虽是合法 JSON，但本服务不支持，会按 invalid request（`-32600`）拒绝并返回 `id:null`
- `jsonrpc` 必须提供且必须是字符串 `"2.0"`；缺失、非字符串或其他版本会按 invalid request（`-32600`）拒绝并保留可解析出的 `id`
- `method` 必须是字符串且非空；非字符串、缺失或空字符串会按 JSON-RPC invalid request（`-32600`）拒绝并保留可解析出的 `id`
- `params` 在本服务中必须省略、为 `null` 或为对象；数组/字符串等会按 invalid params（`-32602`）拒绝并保留可解析出的 `id`；对象内数字会按原始 JSON number 保留，工具整数参数会接受数值上等于整数的 JSON number（例如 `42.0`、`1e3`），但不会因 `float64` 舍入误接受小数
- `tools/call.params.name` 必须是非空 JSON string；非字符串不会被当作缺失名称或未知工具
- `tools/call.params.arguments` 可省略或为 `null`，按空对象传给工具；显式提供时必须是 JSON object，非 object 会在工具执行前按 invalid params（`-32602`）拒绝
- `prompts/get.params.name` 必须是非空 JSON string；非字符串不会被当作缺失名称或未知 prompt
- `prompts/get.params.arguments` 可省略或为 `null`，按空对象传给 prompt；显式提供时必须是 JSON object，非 object 会按 invalid params（`-32602`）拒绝
- 非 `notifications/*` 方法必须提供 `id`；缺失会按 invalid request（`-32600`）拒绝并返回 `id:null`
- `id` 只能是字符串、数字或 `null`；数字 ID 会按原始 JSON number 精确保留，避免大整数被 `float64` 舍入；对象/数组/布尔值会按 invalid request（`-32600`）拒绝并返回 `id:null`
- JSON-RPC error response 会显式包含 `id`；请求无法解析出 id 时返回 `id:null`

### 模式 2：单次 SSE

适合希望拿到 SSE 包装结果，但不维护长连接的客户端。

- 请求：`POST /mcp`
- Header：`Accept: text/event-stream`
- 响应：单个 SSE event，例如：

```text
event: message
data: {"jsonrpc":"2.0","id":1,"result":{...}}
```

### 模式 3：长连接 SSE stream + 异步 POST

适合更接近 streamable HTTP 的客户端。

- 先 `initialize` 获取 `Mcp-Session-Id`
- 再用 `GET /mcp` + `Accept: text/event-stream` + `Mcp-Session-Id` 打开 session 事件流
- 如果服务重启后客户端还带着旧 session 来开 stream，服务端会返回“需要重新 initialize”的提示，便于客户端在同一轮重连逻辑里丢弃旧 session 并重建
- 当该 session 已有活动 stream 时，后续 `POST /mcp` + `Accept: text/event-stream` 不再直接回包结果，而是：
  - HTTP 返回 `202 Accepted`
  - 真正的 JSON-RPC 结果异步写入 SSE stream
- 若 stream 队列已满导致结果无法投递，服务端会退化为本次 POST 的单次 SSE 响应；该响应复用已执行得到的结果，不会再次执行工具。

服务端会在长连接 stream 上发送：

- 打开连接时的注释：`: stream opened`
- 定期心跳注释：`: heartbeat`
- 业务响应事件：`event: message` + `data: {...}`
- 所有带有效 session 的响应都会携带 `MCP-Protocol-Version`，包括同步 JSON、单次 SSE、异步 `202 Accepted`、`GET /mcp` stream，以及 `DELETE /mcp`

## MCP 交互示例

### 1. initialize（JSON）

```bash
curl -i http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {
      "protocolVersion": "2025-11-25",
      "clientInfo": {"name": "demo-client", "version": "1.0.0"}
    }
  }'
```

响应头里的 `Mcp-Session-Id` 就是后续请求要带的会话 ID。

JSON 与单次 SSE 的 `initialize.params.protocolVersion` 都必须是非空字符串；缺失或非字符串会按 invalid params（`-32602`）拒绝，合法但不支持的版本会按 unsupported protocol version（`-32002`）拒绝。`initialize.params.clientInfo` 可省略或为 `null`；一旦显式提供，必须是 JSON object。

### 2. tools/list（JSON）

```bash
SESSION_ID='replace-me'

curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

### 3. tools/list（单次 SSE）

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}'
```

### 4. 打开长连接 SSE stream

```bash
curl -N http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Accept: text/event-stream'
```

你会看到类似：

```text
: stream opened

: heartbeat
```

### 5. 向已打开的 stream 异步投递结果

当上面的 stream 保持打开时，执行：

```bash
curl -i http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}'
```

此时：

- 当前 POST 响应应是 `202 Accepted`
- 真正结果会出现在前面打开的 SSE stream 中，例如：

```text
event: message
data: {"jsonrpc":"2.0","id":9,"result":{"tools":[...]}}
```

### 6. resources/list

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{}}'
```

### 7. prompts/list

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":5,"method":"prompts/list","params":{}}'
```

### 8. resources/templates/list

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":6,"method":"resources/templates/list","params":{}}'
```

当前返回的是**真正的模板语义**，例如：

- `file:///your/allowed/root/{path}`

并且会携带模板参数定义：

- `_meta.templateArguments`
- `_meta.root`

### 9. resources/read

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"file:///your/allowed/root"}}'
```

`resources/read` 当前会返回：

- `contents[].text`：目录摘要或文件内容
- `contents[].mimeType`
- `contents[].uri`：解析后的规范化 `file://` URI；路径中的空格、`#`、`?`、`%` 等会按 URI path 规则转义，返回值可直接用于后续 `resources/read` / `resources/subscribe`
- `contents[]._meta`
  - `path`
  - `name`
  - `is_dir`
  - `size`
  - `mod_time`
  - `mode`
  - 目录额外包含 `child_count` 和 `entries`

资源 URI 必须是本地 `file://` URI：允许空 host 或 `localhost`，path 会按 URI 规则解码（例如 `%20` 表示空格），不接受 query、fragment 或远程 authority。服务端返回的资源 URI 始终使用转义后的规范形式，避免文件名中的 `#` / `?` 被误解析成 fragment / query。
`resources/read` / `resources/subscribe` / `resources/unsubscribe` 的 `uri` 参数必须是 JSON string；非字符串会按 `invalid params` 拒绝。

### 9.1 resources/subscribe

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":71,"method":"resources/subscribe","params":{"uri":"file:///your/allowed/root/README.md"}}'
```

订阅后，如果该资源被 `fs_write_file` / `fs_replace_text` / `fs_apply_unified_diff` / `fs_edit_lines` / `fs_move_path` / `fs_delete_path` / `fs_make_dir` 等工具修改，对应 session 的 SSE stream 会收到：

```text
event: message
data: {"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"file:///..."}}
```

服务端会按与 `resources/read` 相同的本地 file URI 规则解析订阅 URI，并规范化为转义后的 `file://` URI，因此 `file:///root/./README.md`、`file://localhost/root/README.md` 与 `file:///root/README.md` 会匹配同一资源；通知中的 `uri` 也使用同一规范化形式。
`resources/unsubscribe` 同样会先严格解析并规范化 URI；合法但未订阅的 URI 会幂等成功，非法 URI（如远程 host、query 或 fragment）会按 invalid params 返回错误。

如果订阅的是**目录资源**，其子文件/子目录发生增删改时，目录自身的 `file://.../dir` 订阅也会收到更新通知。

### 10. prompts/get

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 7,
    "method": "prompts/get",
    "params": {
      "name": "safe_file_edit",
      "arguments": {
        "task": "update readme",
        "path": "README.md"
      }
    }
  }'
```

`prompts/list` 现在除了标准 `arguments`，还会在 `_meta` 中返回扩展发现信息：

- `title`
- `_meta.inputSchema.type`
- `_meta.inputSchema.required`
- `_meta.inputSchema.properties`

方便客户端直接用来生成更稳定的参数表单。
其中运行时要求非空的必填字符串参数会在 `_meta.inputSchema` 中声明 `minLength: 1`，例如 `safe_file_edit.task` / `path` 与 `go_dev_loop.goal` / `workdir`。
`prompts/get.params.name` 必须是非空 JSON string；`params.arguments` 可省略或为 `null`，一旦显式提供，必须是 JSON object。`prompts/get` 会按这些 schema 做运行时类型校验；例如 `expected_old_text`、`run_vet` 传错类型会返回 `invalid params`，不会被静默当作缺失值。

### 10.1 completion/complete

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 8,
    "method": "completion/complete",
    "params": {
      "ref": {"type": "ref/prompt", "name": "safe_file_edit"},
      "argument": {"name": "path", "value": "REA"}
    }
  }'
```

当前已支持的 completion：

- `safe_file_edit.path`
- `go_dev_loop.workdir`
- `go_dev_loop.test_target`
- `resources/templates/list` 返回的 `file://.../{path}` 模板参数 `path`

`completion/complete` 要求 `ref` 与 `argument` 是 JSON object；可选 `context` 及其 `context.arguments` 若显式提供也必须是 JSON object。`ref.type`、对应的 `ref.name` / `ref.uri`、`argument.name` 与 `argument.value` 都必须是 JSON string；`argument.value` 可以是空字符串，用于请求空前缀补全。对于 `go_dev_loop.test_target` 补全，`context.arguments.workdir` 若显式提供也必须是 JSON string；省略或 `null` 按未提供处理。

路径类补全会按字面前缀匹配，不会 trim 前后空白；因此以空格开头的文件名也能通过相同空格前缀补全。

`fs.*` 工具会在执行前重复校验必填字符串参数：`path` / `src` / `dst`、`fs_write_file.text`、`fs_search_text.query`、`fs_replace_text.old_text` / `new_text`、`fs_edit_lines.new_text`、`fs_apply_unified_diff.diff` 显式传入非字符串时会按 `invalid params` 拒绝，不会被误报为缺失或继续落盘。对运行时已经拒绝空字符串的字段（路径参数、`query`、`old_text`、`diff`），工具 schema 同步声明 `minLength: 1`；`text`、`new_text`、`expected_old_text` 等允许空字符串表达有效语义的字段不声明该约束。

### 11. fs_apply_unified_diff

`fs_apply_unified_diff` 是当前推荐用于**复杂多处修改**的文件编辑原语：

- 输入是**标准 unified diff 文本**
- `diff` 必须是非空白 JSON string，工具 schema 声明 `minLength: 1`；非字符串会被拒绝且不会修改文件
- 目标文件 `path` 单独传参，工具 schema 声明 `minLength: 1`；diff header 只做一致性校验
- diff header 可使用相对路径；只有真实 `..` 路径段会被视为越界，`..data/file.txt` 这类普通目录名仍按合法相对路径匹配
- 第一版只支持**单文件**，但支持**多个 hunk**
- 应用策略是**严格命中**：任一 hunk 对不上就整体失败
- hunk header 行号/行数必须是可解析的整数；溢出会作为无效 diff 拒绝
- hunk header 的非空 range 必须使用正起始行号；`0` 只允许用于 `,0` 空 range（例如文件起始处的插入/删除边界）
- 失败时会返回**结构化冲突详情**，方便 agent 重新读文件并重生 patch
- `expected_old_text` 是精确整文件前置条件；显式空字符串也会按“期望空文件”校验
- `expected_old_text` 必须是 JSON string；非字符串会被拒绝且不会修改文件
- `dry_run=true` 时只验证 patch，不写盘
- `dry_run` 必须是 JSON boolean；字符串等非布尔值会被拒绝且不会修改文件
- `context_lines` 可选，必须是非负整数；工具 schema 声明 `minimum: 0`，大于 20 时按工具语义 cap 到 20

适合场景：

- 一个文件里有多处修改
- 修改涉及上下文校验
- 想避免按行编辑导致的区间漂移

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 4,
    "method": "tools/call",
    "params": {
      "name": "fs_apply_unified_diff",
      "arguments": {
        "path": "README.md",
        "diff": "--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-# old\n+# new\n"
      }
    }
  }'
```

### 11.1 fs_edit_lines

`fs_edit_lines` 现在按**严格行语义**工作：

- `start_line` / `end_line` 是 1-based 正整数行区间；工具 schema 声明 `minimum: 1`
- `new_text` 必须是 JSON string；空字符串仍是合法删除语义
- `new_text` 会按“逻辑行”解释，不会与后续内容黏连
- `new_text` 中的**中间空行会保留**，不会被自动忽略
- `new_text == ""` 表示“不插入任何行”，也就是删除替换区间
- `new_text == "\n"` 表示插入 **1 个空行**
- `expected_old_text` 是精确区间前置条件；显式空字符串也会参与匹配
- `expected_old_text` 必须是 JSON string；非字符串会被拒绝且不会修改文件
- `context_lines` 可选，必须是非负整数；工具 schema 声明 `minimum: 0`，大于 20 时按工具语义 cap 到 20
- 调用方仍应**显式控制自己想要的换行结构**，例如想替换成两行就传两行文本

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 4,
    "method": "tools/call",
    "params": {
      "name": "fs_edit_lines",
      "arguments": {
        "path": "README.md",
        "start_line": 1,
        "end_line": 1,
        "new_text": "# updated\n"
      }
    }
  }'
```

### 11.2 fs_search_text

`fs_search_text` 的使用细节：

- 支持**字面量子串**与**正则表达式**两种匹配模式：默认按子串匹配，传 `regex: true` 时把 `query` 当作 Go 正则编译
- `regex` 是 JSON boolean；正则编译失败会按 `invalid regex` 拒绝，不会静默回退到子串匹配
- `query` 必须是 JSON string，且只要求不是空字符串；工具 schema 声明 `minLength: 1`，空格、制表符等纯空白子串会按字面量搜索
- `path` 可以是单个文件，也可以是目录；工具 schema 声明 `minLength: 1`
- 目录模式下会递归搜索
- 非 `unsafe_allow_all` 模式下，目录搜索会重新校验每个文件路径，并跳过指向 `allowed_roots` 外的符号链接
- 默认 `limit=200`，最大支持到 `1000`
- 命中达到 `limit` 后会停止搜索，并返回已收集的部分结果
- 现在已支持**超长单行**文件，不会因为默认 `bufio.Scanner` 的 64KiB 限制直接失败

**`.gitignore` 过滤（默认开启）：**

- 搜索目录时会读取根目录下的 `.gitignore` 并跳过匹配的路径；传 `use_gitignore: false` 可关闭
- 匹配是 best-effort 简化实现：仅支持根目录 `.gitignore`，支持精确路径、`*` 通配、`**/ `前缀、尾部 `/` 目录标记；**不支持** `!` 取反、`?`、`[abc]` 字符类、嵌套 `.gitignore` 或锚定 `/`
- `!` 取反模式会被显式跳过，因此 `!keep.log` 不会再被当成忽略模式误杀
- 目录命中忽略规则时按子树整棵跳过
- 读取 `.gitignore` 失败（非 `os.ErrNotExist`）会写一条 DEBUG 日志并关闭本次过滤，不影响搜索本身

示例（正则搜索）：

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 10,
    "method": "tools/call",
    "params": {
      "name": "fs_search_text",
      "arguments": {
        "path": ".",
        "query": "func\\s+\\w+\\s*\\(",
        "regex": true,
        "limit": 50
      }
    }
  }'
```

### 11.2.1 fs_find_files

`fs_find_files` 按 glob 模式递归查找文件：

- `path`：搜索根，可以是单个文件或目录（schema 声明 `minLength: 1`）
- `pattern`：glob 模式，必填且非空白（schema 声明 `minLength: 1`）
- `limit`：默认 `100`，最大 `1000`；`<=0` 回退默认，`>1000` 钳制到 `1000`；命中达到上限即停止遍历并返回已收集结果
- 返回 `files[]`，每项含 `path` / `name` / `size`

**glob 语义：**

- 模式包含独立的 `**` 段时启用**跨层级匹配**：`**` 匹配任意层级（含**零层**）。`**/*.go` 匹配根目录与所有子目录的 `.go` 文件；`**/pkg/util.go` 匹配 `pkg/util.go`（`**` 消费零段）
- 单个 `*` **不跨 `/`**：`**/pkg/*.go` 只匹配 `pkg` 直下，不含更深子目录
- 不含 `**` 段的模式保持既有语义：递归遍历目录 + 对文件 basename 做 `filepath.Match`
- `?` 与 `[abc]` 字符类按 `filepath.Match` 语义生效
- `**.go` 这类非独立段不触发跨层级语义，按单段通配处理（等价 `*.go`）
- 非 `unsafe_allow_all` 模式下，每个命中文件都会重新经 `allowed_roots` 校验，指向根外的符号链接会被静默跳过

**`.gitignore` 过滤：** 与 `fs_search_text` 共用同一实现，默认开启，`use_gitignore: false` 关闭。

示例：

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 11,
    "method": "tools/call",
    "params": {
      "name": "fs_find_files",
      "arguments": {
        "path": ".",
        "pattern": "**/*.go",
        "limit": 100
      }
    }
  }'
```

### 11.3 fs_move_path / fs_delete_path

- `fs_move_path` / `fs_delete_path` 会操作最终路径目录项本身；如果最终路径是符号链接，会移动/删除链接，而不是链接目标
- 最终路径的父目录仍会解析符号链接并做 allowed-roots 校验，不能通过 `linked-dir/file` 这类父级符号链接逃逸

### 11.4 git.* 通用约束

`git.*` 工具有几条共同规则：

- 如果不传 `repo_path`，默认使用服务启动目录
- `repo_path` 必须落在 `allowed_roots` 内，并且真实仓库根也必须仍在 `allowed_roots` 内
- `git_add` / `git_restore` / `git_diff` 的 `paths` 必须是 **repo-relative** 路径
- `git_add` / `git_restore` 的 `paths` 必须非空，工具 schema 声明 `minItems: 1`；`git_diff` 的 `paths` 仍可省略或传空数组表示全量 diff
- 不允许绝对路径，不允许 `..` 越界，不允许把路径伪装成选项，也不允许以 `:` 开头的 Git pathspec magic（如 `:/`）
- `git_commit.message` 与 `git_switch.branch` 必须是非空字符串，工具 schema 声明 `minLength: 1`
- `git_pull` 固定是 `git pull --ff-only`

### 11.5 fs_replace_text

`fs_replace_text` 的使用细节：

- 在单个文件中按**精确旧文本**替换新文本
- 默认只替换**第一处命中**
- `replace_all=true` 时替换所有命中
- `replace_all` 必须是 JSON boolean；字符串等非布尔值会被拒绝且不会修改文件
- `expected_replacements` 可用于保护性校验，要求替换前总命中数必须一致
- `expected_replacements` 必须是非负整数；工具 schema 声明 `minimum: 0`
- `old_text` / `new_text` 必须是 JSON string；`old_text` 不能为空且 schema 声明 `minLength: 1`，`new_text` 可以为空字符串

### 12. exec_run

`exec_run` 的使用细节：

- 只能运行预定义 preset，不支持任意 shell 命令
- `workdir` 必填，且必须落在 `allowed_roots` 内
- `workdir` 必须是非空 JSON string，工具 schema 声明 `minLength: 1`；非字符串会按 `invalid params` 拒绝
- `preset` / `command` 若显式提供必须是 JSON string；`command` 还必须非空且 schema 声明 `minLength: 1`
- 提供 `command` 会选择 raw command 分支，不能在类型错误或 `unsafe_allow_all=false` 时静默回退到 preset
- preset 模式下 `args` 必须是字符串数组且成员非空；`timeout_override_sec` 必须是正整数，工具 schema 声明 `minimum: 1`
- 当前 `yolo` 分支且 `unsafe_allow_all=true` 时，`exec_run` 也支持 `command + args + env` 原始命令模式；raw `args` 仍必须是字符串数组，但允许空字符串参数以保留真实 argv
- `go_test` / `go_generate` / `go_build` / `go_vet` 在没有显式 target 时，会自动补 `./...`
- `go_mod_download` / `go_mod_tidy` / `go_get` / `go_list` / `go_work_sync` 会扩展 Go toolchain 能力，但仍受 preset 白名单控制
- 所有 Go preset 会把这些目录固定到 `~/.mcp-tools/cache` 下：
  - `GOCACHE`
  - `GOMODCACHE`
  - `GOTMPDIR`
- 这样依赖下载、构建缓存和临时目录不会外溢到不可控的系统位置
- `go_mod_download` 会直接在 `workdir` 中执行 `go mod download`
- `go_mod_tidy` 会直接在 `workdir` 中执行 `go mod tidy`，不会自动补目标路径
- `go_get` 允许受控模块参数（例如 `example.com/mod@v1.2.3`）或本地 target
- 非 `go_get` preset 的位置参数只接受本地 target；`github.com/org/mod` 这类模块/导入路径会被拒绝
- 裸相对 target 会在执行前规范化为 `./...` / `./pkg` 形式，避免 `net/http`、`std`、`...` 被 Go 当作工作目录外的导入路径或全局模式
- `go_list` 适合做 Go 包/依赖信息探查
- `go_work_sync` 直接在 `workdir` 中执行 `go work sync`
- `timeout_override_sec` 必须是正整数（工具 schema 声明 `minimum: 1`）；只能**缩短**默认超时，不能放大
- 带值 Go flags（如 `-run` / `-skip` / `-tags` / `-go` / `-compat`）支持 `-flag value` 与 `-flag=value`；这些值不会被误判为 target
- Go positional targets 会解析符号链接后校验仍在 `workdir` 内，不能借 `./linked/...` 逃逸到工作目录外
- `-o=...` / `-o ...` / `-coverprofile=...` / `-coverprofile ...` 这类输出路径参数会解析符号链接后再次校验，不能写到工作目录外
- `-vettool` 当前明确不支持

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 5,
    "method": "tools/call",
    "params": {
      "name": "exec_run",
      "arguments": {
        "preset": "go_test",
        "workdir": ".",
        "args": ["./..."]
      }
    }
  }'
```

### 12.1 go_mod_tidy 示例

```bash
curl -s http://127.0.0.1:8080/mcp   -H 'Authorization: Bearer change-me'   -H "Mcp-Session-Id: ${SESSION_ID}"   -H 'Content-Type: application/json'   -d '{
    "jsonrpc": "2.0",
    "id": 5,
    "method": "tools/call",
    "params": {
      "name": "exec_run",
      "arguments": {
        "preset": "go_mod_tidy",
        "workdir": "."
      }
    }
  }'
```

### 12.2 go_generate 示例

```bash
curl -s http://127.0.0.1:8080/mcp   -H 'Authorization: Bearer change-me'   -H "Mcp-Session-Id: ${SESSION_ID}"   -H 'Content-Type: application/json'   -d '{
    "jsonrpc": "2.0",
    "id": 6,
    "method": "tools/call",
    "params": {
      "name": "exec_run",
      "arguments": {
        "preset": "go_generate",
        "workdir": "."
      }
    }
  }'
```

### 12.3 go_get 示例

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 7,
    "method": "tools/call",
    "params": {
      "name": "exec_run",
      "arguments": {
        "preset": "go_get",
        "workdir": ".",
        "args": ["github.com/google/uuid@v1.6.0"]
      }
    }
  }'
```

### 12.4 exec_run_template

`exec_run_template` 的使用细节：

- 只能运行服务端配置好的模板命令
- 客户端只能传：`template`、`workdir`、`timeout_override_sec`、`confirm`
- `template` / `workdir` 必须是非空 JSON string，工具 schema 声明 `minLength: 1`；非字符串会按 `invalid params` 拒绝
- 模板本身提供固定 argv，不支持任意 shell 字符串
- 模板可带服务端固定 `env`
- 模板配置了 `allowed_workdirs` 时，即使 `unsafe_allow_all=true` 也仍会限制可执行工作目录
- 模板可带风险语义：`category` / `destructive` / `requires_confirmation`
- 若模板 `requires_confirmation=true`，客户端必须显式传 `confirm=true` 才会执行
- `confirm` 必须是 JSON boolean；字符串等非布尔值会被拒绝且不会执行模板
- `timeout_override_sec` 必须是正整数（工具 schema 声明 `minimum: 1`），且仍然只能缩短

```bash
curl -s http://127.0.0.1:8080/mcp   -H 'Authorization: Bearer change-me'   -H "Mcp-Session-Id: ${SESSION_ID}"   -H 'Content-Type: application/json'   -d '{
    "jsonrpc": "2.0",
    "id": 7,
    "method": "tools/call",
    "params": {
      "name": "exec_run_template",
      "arguments": {
        "template": "make_test",
        "workdir": "."
      }
    }
  }'
```

#### 12.5 fs_pull_file（拉取服务器文件到客户端）

```bash
# 1. 客户端调用 fs_pull_file（JSON 模式）
curl -sS -X POST http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: <session-id>" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fs_pull_file","arguments":{"path":"progress.png"}}}'
# => {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"issued pull link"}],
#     "structuredContent":{"url":"/file/<token>","url_kind":"relative","filename":"progress.png",
#     "bytes":12345,"mime_type":"image/png","expires_in_sec":300,"max_downloads":0}, ...}}

# 2. 客户端用自己请求 MCP 的 base URL 拼接，下载到本地
curl -o ./progress.png "http://127.0.0.1:8080/file/<token>"

# 3. 配置了 public_base_url 时，直接使用返回的绝对 URL
curl -o ./progress.png "https://files.example.com/mcp-tools/file/<token>"
```

说明：

- 下载 URL 是唯一授权凭据：**持有 URL 即有权下载**，请勿转发；有效期与次数由 `pull_file.url.*` 控制
- 服务重启后所有已签发 URL 立即失效
- 每次下载都会写入一条 `fs_pull_file.download` 审计事件

### 12.5 exec_shell

`exec_shell` 执行**整条 shell 命令字符串**，补齐 `exec_run` 无法表达管道/重定向的缺口：

- **仅在 `unsafe_allow_all=true` 时可用**；关闭时调用返回 `exec_shell requires unsafe_allow_all=true`，且记为 `feature disabled` 审计
- Unix 使用 `/bin/sh -c`，Windows 使用 `cmd.exe /C`，因此管道、`>`、`&&`、环境变量展开都由系统 shell 处理
- `command` 必填且非空白；`workdir` 必填
- `env` 为 `map[string]string`，会与基础环境合并；合并时**先删除同名旧条目再追加**，避免出现重复 `KEY=value`（POSIX `execve` 对重复 key 语义未定义，可能导致 env 覆盖失效）
- `timeout_override_sec` 只能**缩短**执行时间，不能超过 `command_timeout_sec` 配置
- 审计与 `exec_run` 共用同一脱敏链路：`env` 只落 `keys` 与 `count`，**明文值不进入 JSONL 审计与控制台日志**
- 返回结构化字段含 `mode: "shell"` 与原始 `command`

示例：

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 20,
    "method": "tools/call",
    "params": {
      "name": "exec_shell",
      "arguments": {
        "command": "go build ./... 2>&1 | tail -20",
        "workdir": ".",
        "timeout_override_sec": 120
      }
    }
  }'
```

### 12.6 后台进程管理（exec_start_process 等）

这组工具让 agent 能在 **不阻塞单次调用** 的前提下跑长任务（dev server、watch、长时间构建）：

| 工具 | 作用 | 关键参数 |
| --- | --- | --- |
| `exec_start_process` | 异步启动后台进程，立即返回进程 ID | `command`（必填）、`args`、`env`、`workdir`（必填） |
| `exec_list_processes` | 列出全部后台进程及其状态、pid | 无 |
| `exec_process_logs` | 取回后台进程已捕获的 stdout/stderr | `id`（必填） |
| `exec_stop_process` | 停止后台进程 | `id`（必填）、`force` |
| `exec_remove_process` | 从进程表移除**已结束**的进程记录 | `id`（必填） |

共同约束：

- 五个工具**都要求 `unsafe_allow_all=true`**，否则返回 `feature disabled` 审计并附带明确的 `requires unsafe_allow_all=true` 错误
- 输出按进程缓冲并在总量上限处截断；`exec_process_logs` 可反复读取同一缓冲区
- `exec_stop_process` 执行**真正的进程树终止**：非 `force` 时先优雅终止（Unix 对进程组发 `SIGTERM`，Windows 用 `taskkill /T`）并给 5s 宽限期，超时升级为强杀；`force=true` 直接强杀进程树（Windows `taskkill /T /F`，Unix 进程组 `SIGKILL`）。等待退出有 10s 总超时，**不会无限阻塞**
- `exec_remove_process` 拒绝移除仍在运行的进程，只清理已结束的记录，避免进程表只增不减
- `id` 类参数在审计与控制台日志中记为 `process_id`，`exec_start_process` 记为 `command` / `workdir` / `args_count`

示例：

```bash
# 1. 启动
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0", "id": 21, "method": "tools/call",
    "params": {"name": "exec_start_process", "arguments": {
      "command": "go", "args": ["run", "./cmd/server"], "workdir": "."
    }}
  }'

# 2. 读日志（把 <id> 换成返回的进程 ID）
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0", "id": 22, "method": "tools/call",
    "params": {"name": "exec_process_logs", "arguments": {"id": "<id>"}}
  }'

# 3. 停止并清理
#    exec_stop_process -> exec_remove_process
```

## 13. Go 导航工具

这两个工具用于补齐 coding agent 的 **outline + definition** 工作流：

- `go_list_symbols`
  - 输入：单个 Go 文件 `path`，必须是 JSON string
  - 输出：该文件中的顶层 `func` / `method` / `type` / `var` / `const`
- `go_find_definition`
  - 输入：`path + line + column`；`path` 必须是 JSON string，`line` / `column` 必须是正 JSON integer，工具 schema 声明 `minimum: 1`
  - 类型错误会在 package load 前按 validation error 拒绝；正整数但越界的位置仍返回 `position_out_of_bounds`
  - `line` / `column` 必须落在标识符字符范围内；标识符后的 `(`、`.` 或空白不会被当作该标识符
  - 第一版只覆盖 package-level declarations、methods 与 imported package symbols
  - 若定义落在 `allowed_roots` 外，仍会返回位置，但会标记 `in_allowed_roots=false`

适合场景：

- 先看清一个 Go 文件里有哪些顶层声明
- 再从当前光标位置跳到定义
- 再决定是否继续 `fs_read_file` / `fs_apply_unified_diff`

### 13.1 go_list_symbols

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 8,
    "method": "tools/call",
    "params": {
      "name": "go_list_symbols",
      "arguments": {
        "path": "internal/httpapi/server.go"
      }
    }
  }'
```

### 13.2 go_find_definition

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "id": 9,
    "method": "tools/call",
    "params": {
      "name": "go_find_definition",
      "arguments": {
        "path": "internal/httpapi/server.go",
        "line": 180,
        "column": 12
      }
    }
  }'
```

如果定义位于依赖或标准库源码中，返回结构会类似：

- `definition_path`
- `definition_line`
- `definition_column`
- `symbol_name`
- `symbol_kind`
- `in_allowed_roots=false`

这意味着 agent 已经知道“定义在哪”，但**并不等于**服务端会自动放开对外部文件的读取边界。

## 演示脚本

已提供一键演示脚本：`scripts/demo_stream.sh` 和 `scripts/demo_capabilities.sh`

它会自动：

- 启动本地 `mcp-tools` 服务
- 创建 session
- 打开 `GET /mcp` 的 SSE stream
- 通过异步 `POST /mcp` 把 `tools/list` 结果投递到 stream
- 你也可以把 `resources/read` / `prompts/get` 等请求用同样方式异步投递到 stream
- 打印 stream 中实际收到的事件
- 演示 resources/templates/list（含模板参数）、读取允许目录、读取单个文件（含 metadata）、prompts/get 的 JSON 调用

运行：

```bash
bash ./scripts/demo_stream.sh
bash ./scripts/demo_capabilities.sh
```

## 容器化部署

仓库已提供：

- `Dockerfile`
- `.dockerignore`

构建：

```bash
docker build -t mcp-tools:local .
```

运行：

```bash
docker run --rm -p 8080:8080 \
  -e MCP_BEARER_TOKEN=change-me \
  -e MCP_ALLOWED_ROOTS=/workspace \
  -v "$PWD":/workspace \
  mcp-tools:local
```

如果要在容器启动前做配置校验：

```bash
docker run --rm \
  -e MCP_CONFIG_FILE=/app/mcp-tools.example.yaml \
  -v "$PWD/mcp-tools.example.yaml:/app/mcp-tools.example.yaml:ro" \
  mcp-tools:local \
  -validate-config
```

## 验证

```bash
go test ./internal/httpapi -v
go test ./...
```
