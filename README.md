# mcp-tools

一个基于 Go 1.20 的单体 MCP HTTP 服务，提供受限的文件系统、Git 与 Go 工具链执行能力，供远程 AI 代理通过 MCP 调用本地工具。

补充维护文档：

- `AGENTS.md`：给 AI 代理/人工调用方的使用与边界说明
- `TOOLS-DEFINE.md`：给维护者的内部工具实现算法与约束说明
- `CHANGELOG.md`：版本变更记录
- `docs/releases/v0.1.0.md`：`v0.1.0` 发布说明

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
  - 当某个 session 已有活动 SSE stream 时，后续 `POST /mcp` + `Accept: text/event-stream` 会把结果**异步投递**到该 stream，并返回 `202 Accepted`
  - 异步投递同样覆盖 `tools/*`、`resources/*`、`prompts/*`
- **文件工具**
  - `fs.read_file`
  - `fs.write_file`
  - `fs.list_dir`
  - `fs.stat_path`
  - `fs.make_dir`
  - `fs.move_path`
  - `fs.delete_path`
  - `fs.search_text`
  - `fs.edit_lines`
- **Git 工具**
  - `git.status`
  - `git.diff`
  - `git.log`
  - `git.add`
  - `git.restore`
  - `git.commit`
  - `git.branch`
  - `git.switch`
  - `git.pull`
- **Exec 工具**
  - `exec.run`
  - 仅开放 Go preset：`go_fmt` / `go_test` / `go_build` / `go_vet` / `go_mod_tidy`
- 审计日志：JSON Lines
- 目录边界：启动目录 + `allowed_roots`
- 浏览器 Origin 拒绝：未配置 `allowed_origins` / `MCP_ALLOWED_ORIGINS` 时默认不接受浏览器来源请求
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

#### `debug_http_log`

用于排查 MCP 客户端接入问题。

开启后，服务端会打印**脱敏**的 HTTP/MCP 调试日志，例如：

- 请求方法、路径、Accept、Origin
- 是否带 `Authorization`
- `Authorization` 的 scheme（例如 `Bearer`）
- 是否带 session header
- `MCP-Protocol-Version`
- 解码后的 JSON-RPC method
- 被拒绝的原因（例如缺 token、协议版本不支持）

不会打印完整 Bearer Token。

#### `allowed_roots`

额外允许访问的目录白名单。

注意：

- 服务启动目录会**自动加入**
- `~/.mcp-tools` 也会**自动加入**
- `allowed_roots` 是“附加目录”，不是完整覆盖列表

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
- `debug_http_log`
- `allowed_origins`
- `audit_log_path`
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

说明：

- YAML 中的相对路径，按**配置文件所在目录**解析
- `allowed_roots` 是**附加白名单目录**；启动目录始终保留
- 默认 `audit_log_path`：
  - Linux / macOS：`~/.mcp-tools/mcp-audit.jsonl`
  - Windows：`%USERPROFILE%\\.mcp-tools\\mcp-audit.jsonl`
- `audit.NewJSONLWriter` 会自动创建缺失的父目录
- `allowed_origins` 是浏览器 `Origin` 白名单；**不是 hostname 入站控制**
- `git.allowed_subcommands` 只能配置当前实现支持的白名单子命令，**不支持 `push`**
- `exec.presets.<name>.enabled: false` 可禁用内置 preset

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
- `MCP_DEBUG_HTTP_LOG`：是否开启脱敏的 HTTP/MCP 调试日志，支持 `true/false`
- `MCP_LISTEN_ADDR`：默认 `0.0.0.0:8080`
- `MCP_ALLOWED_ROOTS`：逗号分隔的额外允许目录
- `MCP_ALLOWED_ORIGINS`：逗号分隔的允许浏览器来源
- `MCP_AUDIT_LOG_PATH`：默认：
  - Linux / macOS：`~/.mcp-tools/mcp-audit.jsonl`
  - Windows：`%USERPROFILE%\\.mcp-tools\\mcp-audit.jsonl`
- `MCP_COMMAND_TIMEOUT_SEC`：默认 `30`
- `MCP_OUTPUT_MAX_BYTES`：默认 `65536`
- `MCP_STREAM_QUEUE_SIZE`：单个 SSE stream 的内部队列容量，默认 `128`
- `MCP_MAX_REQUEST_BYTES`：默认 `1048576`
- `MCP_READ_HEADER_TIMEOUT_SEC`：默认 `5`
- `MCP_READ_TIMEOUT_SEC`：默认 `15`
- `MCP_WRITE_TIMEOUT_SEC`：默认 `30`
- `MCP_IDLE_TIMEOUT_SEC`：默认 `60`
- `MCP_SESSION_TTL_MIN`：默认 `120`
- `MCP_SERVER_NAME`：默认 `mcp-tools`
- `MCP_SERVER_VERSION`：默认 `0.1.0`

## 传输层说明

### 生产默认护栏

- `POST /mcp` 请求体大小默认限制为 `1 MiB`
- SSE stream 内部消息队列默认容量为 `128`
- `ReadHeaderTimeout=5s`
- `ReadTimeout=15s`
- `WriteTimeout=30s`
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
- 当该 session 已有活动 stream 时，后续 `POST /mcp` + `Accept: text/event-stream` 不再直接回包结果，而是：
  - HTTP 返回 `202 Accepted`
  - 真正的 JSON-RPC 结果异步写入 SSE stream

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
- `contents[]._meta`
  - `path`
  - `name`
  - `is_dir`
  - `size`
  - `mod_time`
  - `mode`
  - 目录额外包含 `child_count` 和 `entries`

### 9.1 resources/subscribe

```bash
curl -s http://127.0.0.1:8080/mcp \
  -H 'Authorization: Bearer change-me' \
  -H "Mcp-Session-Id: ${SESSION_ID}" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":71,"method":"resources/subscribe","params":{"uri":"file:///your/allowed/root/README.md"}}'
```

订阅后，如果该资源被 `fs.write_file` / `fs.edit_lines` / `fs.move_path` / `fs.delete_path` / `fs.make_dir` 等工具修改，对应 session 的 SSE stream 会收到：

```text
event: message
data: {"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"file:///..."}}
```

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

### 11. fs.edit_lines

`fs.edit_lines` 现在按**严格行语义**工作：

- `start_line` / `end_line` 是 1-based 行区间
- `new_text` 会按“逻辑行”解释，不会与后续内容黏连
- `new_text` 中的**中间空行会保留**，不会被自动忽略
- `new_text == ""` 表示“不插入任何行”，也就是删除替换区间
- `new_text == "
"` 表示插入 **1 个空行**
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
      "name": "fs.edit_lines",
      "arguments": {
        "path": "README.md",
        "start_line": 1,
        "end_line": 1,
        "new_text": "# updated\n"
      }
    }
  }'
```

### 11.1 fs.search_text

`fs.search_text` 的使用细节：

- 当前是**按子串匹配**，不是正则
- `path` 可以是单个文件，也可以是目录
- 目录模式下会递归搜索
- 默认 `limit=200`，最大支持到 `1000`
- 现在已支持**超长单行**文件，不会因为默认 `bufio.Scanner` 的 64KiB 限制直接失败

### 11.2 git.* 通用约束

`git.*` 工具有几条共同规则：

- 如果不传 `repo_path`，默认使用服务启动目录
- `repo_path` 必须落在 `allowed_roots` 内，并且真实仓库根也必须仍在 `allowed_roots` 内
- `git.add` / `git.restore` / `git.diff` 的 `paths` 必须是 **repo-relative** 路径
- 不允许绝对路径，不允许 `..` 越界，不允许把路径伪装成选项
- `git.pull` 固定是 `git pull --ff-only`

### 12. exec.run

`exec.run` 的使用细节：

- 只能运行预定义 preset，不支持任意 shell 命令
- `workdir` 必填，且必须落在 `allowed_roots` 内
- `go_test` / `go_build` / `go_vet` 在没有显式 target 时，会自动补 `./...`
- `go_mod_tidy` 会直接在 `workdir` 中执行 `go mod tidy`，不会自动补目标路径
- `timeout_override_sec` 只能**缩短**默认超时，不能放大
- `-o=...` / `-coverprofile=...` 这类 inline 路径参数也会再次校验，不能写到工作目录外
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
      "name": "exec.run",
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
      "name": "exec.run",
      "arguments": {
        "preset": "go_mod_tidy",
        "workdir": "."
      }
    }
  }'
```

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
