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
- 新增 `fs_pull_file` 工具：为服务器工作区文件签发短时效、可限次、HMAC 签名绑定的下载 URL，客户端通过 `GET /file/<token>` 自行下载保存。
  - 新配置段 `pull_file`：`enabled` / `allowed_extensions` / `max_bytes` / `url.ttl_sec` / `url.max_downloads` / `url.public_base_url`
  - 默认返回相对路径 URL（客户端按自身 MCP base URL 拼接），可配置 `public_base_url` 返回绝对 URL
  - 下载端点独立审计事件 `fs_pull_file.download`
  - 新增 `internal/pullfile` 包与 `internal/httpapi/download_test.go` 端到端覆盖

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
