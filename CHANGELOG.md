# CHANGELOG

本项目变更记录。

格式参考 Keep a Changelog，并结合当前仓库实际开发节奏维护。

## [Unreleased]

### Changed
- **破坏性变更**：为兼容 OpenAI function calling 的命名规范（仅允许 `a-z`/`A-Z`/`0-9`/`_`/`-`），所有 MCP 工具名称中的 `.` 改为 `_`：
  - `fs.read_file` → `fs_read_file`，`fs.pull_file` → `fs_pull_file`，其余 `fs.*` 同理
  - `git.status` → `git_status`，`git.diff` → `git_diff`，其余 `git.*` 同理
  - `go.list_symbols` → `go_list_symbols`，`go.find_definition` → `go_find_definition`
  - `exec.run` → `exec_run`，`exec.run_template` → `exec_run_template`
  - 已同步：注册表、审计日志 `tool` 字段、控制台日志字段、prompt 模板文本、`fs_pull_file` 下载事件名、测试与当前文档
  - 配置键不受影响（如 `git.allowed_subcommands`、`exec.presets`、`pull_file.*` 保持不变）
  - 注意：这是破坏性变更，旧客户端需改用新工具名；历史发布说明保留旧名

### Added
- 新增 `fs_find_files` 工具：按 glob 模式递归查找文件，支持 `**` 跨层级匹配（`**` 匹配任意层级、含零层），默认启用 `.gitignore` 过滤
- `fs_search_text` 新增 `regex` 参数：`regex: true` 时把 `query` 当作 Go 正则编译，编译失败按 `invalid regex` 拒绝
- `fs_search_text` / `fs_find_files` 新增 `use_gitignore` 参数（默认 `true`）：自动读取根目录 `.gitignore` 并跳过匹配路径；best-effort 简化实现（精确路径 / `*` 通配 / `**/` 前缀 / 尾部 `/` 目录标记），不支持 `!` 取反、`?`、`[abc]`、嵌套与锚定 `/`
- 新增后台进程管理工具（`unsafe_allow_all=true` 时可用）：
  - `exec_start_process`：异步启动后台进程并返回进程 ID
  - `exec_list_processes`：列出所有后台进程
  - `exec_process_logs`：获取后台进程的 stdout/stderr 缓冲
  - `exec_stop_process`：停止后台进程
  - `exec_remove_process`：从进程表移除已结束的进程记录（运行中的进程拒绝移除）
- 新增 `exec_shell` 工具（`unsafe_allow_all=true` 时可用）：执行 shell 命令字符串，支持管道、重定向与环境变量展开（Unix 用 `/bin/sh -c`，Windows 用 `cmd /C`）
- 新增 `fs_pull_file` 工具：为服务器工作区文件签发短时效、可限次、HMAC 签名绑定的下载 URL，客户端通过 `GET /file/<token>` 自行下载保存。
  - 新配置段 `pull_file`：`enabled` / `allowed_extensions` / `max_bytes` / `url.ttl_sec` / `url.max_downloads` / `url.public_base_url`
  - 默认返回相对路径 URL（客户端按自身 MCP base URL 拼接），可配置 `public_base_url` 返回绝对 URL
  - 下载端点独立审计事件 `fs_pull_file.download`
  - 新增 `internal/pullfile` 包与 `internal/httpapi/download_test.go` 端到端覆盖

### Changed
- `fs_search_text` 描述更新为支持正则表达式与 `.gitignore` 过滤
- `mcp` 工具日志字段新增对 `exec_shell` / `exec_start_process` / `exec_process_logs` / `exec_stop_process` / `exec_remove_process` 的摘要支持
- `exec_stop_process` 实现真正的进程树终止与有限超时：非 force 先优雅终止（Unix 对进程组发 `SIGTERM`，Windows 用 `taskkill /T`）并等待 5s 宽限期，超时升级强杀进程树；`force=true` 直接强杀目标进程及其子进程（Windows `taskkill /T /F`，Unix 进程组 `SIGKILL`）；等待进程退出有 10s 总超时，不再无限阻塞
- 审计日志对 `arguments` 中的 `env` 参数统一脱敏：值替换为固定掩码 `***`、仅保留键名，覆盖 `exec_shell` / `exec_start_process` / `exec_run` 的 `env` 入口，JSONL 审计与控制台日志经过同一脱敏，敏感值不再明文落盘（注：与 `summarizeAuditArguments` 合并后，env 以 `keys`+`count` 摘要落盘）
- `fs_find_files` 支持 `**` 跨层级 glob：`**` 匹配任意层级（含零层），`**/*.go` 等含 `/` 的模式不再恒返回 0 结果；单个 `*` 保持不跨 `/`，不含 `**` 段的模式保持既有 basename 语义
- 目录搜索/查找在非 `unsafe_allow_all` 模式下会重新校验每个文件路径并跳过指向 `allowed_roots` 外的符号链接（`fs_search_text` / `fs_find_files` 统一行为）

## [1.0.0] - 2026-04-30

### Added
- `yolo` 分支默认开启 `unsafe_allow_all=true`，为已授权 MCP 客户端放开文件路径、工作目录、Git 仓库与原始命令执行限制，同时保留控制台日志与 JSONL 审计日志。
- `exec_run` 在 `unsafe_allow_all=true` 时新增原始命令模式，支持：
  - `command`
  - `args`
  - `env`
  - `workdir`
- 新增统一控制台应用日志：
  - `DEBUG`
  - `INFO`
  - `WARN`
  - `ERROR`
- 成功工具调用现在会记录高可读性的 `INFO` 控制台日志，并输出工具名、主要参数摘要、耗时与结果摘要。
- 新增审计日志按大小滚动能力：
  - `audit_rotate_max_mb`
  - `audit_rotate_max_backups`
- 新增 `internal/applog` 统一日志底座，用于服务启动/停止、工具调用与 HTTP/MCP 调试日志输出。
- 新增 Go-only 导航工具：
  - `go_list_symbols`
  - `go_find_definition`
- `go_list_symbols` 支持返回单个 Go 文件中的顶层 `func` / `method` / `type` / `var` / `const` 结构索引。
- `go_find_definition` 支持按 `path + line + column` 跳转到 package-level declarations、methods 与 imported package symbols 的定义位置，并对外部定义返回 `in_allowed_roots=false`。
- 扩展 `exec_run` 的 Go preset：
  - `go_get`
  - `go_list`
  - `go_work_sync`
- 所有 Go preset 现在会把 `GOCACHE` / `GOMODCACHE` / `GOTMPDIR` 固定到 `~/.mcp-tools/cache` 下，降低依赖下载和构建缓存对外部目录的污染。

### Fixed
- `fs.*` / `git.*` / `go.*` / `resources/read` / 资源更新链路在 `unsafe_allow_all=true` 时会一致按危险模式放开，不再出现“有些工具 yolo、有些还偷偷卡 allowed_roots”的半放行状态。
- 移除独立 `debug_http_log` 开关，统一改由 `log_level=DEBUG` 控制脱敏 HTTP/MCP 调试日志。
- 恢复项目 Go 基线到 `go 1.20`，并将 `golang.org/x/tools` / `golang.org/x/mod` / `golang.org/x/sync` 依赖链回退到与 Windows 7 目标场景一致的兼容版本。
- 调整 `GET /mcp` 的 stale-session 处理：服务重启后若客户端仍带旧 `Mcp-Session-Id` 开 stream，服务端现在按“未初始化 session”返回重连提示，避免首个重连动作直接落成 `invalid or expired session`。
- 加固路径解析，修复 symlink 与多级缺失路径组合下的允许目录绕过风险。
- 加固 `exec_run` 参数校验，禁止通过 `flag=value` 形式把输出写到工作目录外，并禁用 `-vettool`。
- 修复 `fs_search_text` 在超长单行文件上因 `bufio.Scanner` token 上限而失败的问题。

## [0.1.0] - 2026-04-11

### Added
- 基于 Go 1.20 的单体 MCP HTTP 服务骨架。
- JSON、单次 SSE、`GET /mcp` 长连接 stream + async `POST /mcp` 传输模式。
- Bearer Token 鉴权、允许目录隔离、Origin 拒绝、审计日志、`/healthz` 与 `/readyz`。
- MCP capabilities：
  - `tools/list`
  - `tools/call`
  - `resources/list`
  - `resources/templates/list`
  - `resources/read`
  - `resources/subscribe`
  - `resources/unsubscribe`
  - `prompts/list`
  - `prompts/get`
  - `completion/complete`
  - `ping`
- 文件工具：
  - `fs_read_file`
  - `fs_write_file`
  - `fs_list_dir`
  - `fs_stat_path`
  - `fs_make_dir`
  - `fs_move_path`
  - `fs_delete_path`
  - `fs_search_text`
  - `fs_edit_lines`
- Git 白名单工具：
  - `git_status`
  - `git_diff`
  - `git_log`
  - `git_add`
  - `git_restore`
  - `git_commit`
  - `git_branch`
  - `git_switch`
  - `git_pull`
- Exec 工具：
  - `exec_run`
  - 内置 Go preset：`go_fmt` / `go_test` / `go_build` / `go_vet`
- 初始文档与示例：
  - `README.md`
  - `AGENTS.md`
  - `TOOLS-DEFINE.md`
  - `mcp-tools.example.yaml`
  - `scripts/demo_stream.sh`
  - `scripts/demo_capabilities.sh`
