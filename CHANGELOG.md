# CHANGELOG

本项目变更记录。

格式参考 Keep a Changelog，并结合当前仓库实际开发节奏维护。

## [Unreleased]

### Added
- `fs.search_text` 新增 `regex` 参数，支持正则表达式搜索
- `fs.search_text` 新增 `use_gitignore` 参数，支持 `.gitignore` 过滤
- 新增 `fs.find_files` 工具，支持按 glob 模式递归查找文件，支持 `.gitignore` 过滤
- 新增后台进程管理工具（`unsafe_allow_all=true` 时可用）：
  - `exec.start_process`：异步启动后台进程并返回进程 ID
  - `exec.list_processes`：列出所有后台进程
  - `exec.process_logs`：获取后台进程的 stdout/stderr 缓冲
  - `exec.stop_process`：停止后台进程
  - `exec.remove_process`：从进程表移除已结束的进程记录（运行中的进程拒绝移除）
- 新增 `exec.shell` 工具（`unsafe_allow_all=true` 时可用）：执行 shell 命令字符串，支持管道、重定向与环境变量展开（Unix 用 `/bin/sh -c`，Windows 用 `cmd /C`）

### Changed
- `fs.search_text` 描述更新为支持正则表达式与 `.gitignore` 过滤
- `mcp` 工具日志字段新增对 `exec.shell` / `exec.start_process` / `exec.process_logs` / `exec.stop_process` 的摘要支持
- `exec.stop_process` 实现真正的进程树终止与有限超时：非 force 先优雅终止（Unix 对进程组发 `SIGTERM`，Windows 用 `taskkill /T`）并等待 5s 宽限期，超时升级强杀进程树；`force=true` 直接强杀目标进程及其子进程（Windows `taskkill /T /F`，Unix 进程组 `SIGKILL`）；等待进程退出有 10s 总超时，不再无限阻塞
- 审计日志对 `arguments` 中的 `env` 参数统一脱敏：值替换为固定掩码 `***`、仅保留键名，覆盖 `exec.shell` / `exec.start_process` / `exec.run` 的 `env` 入口，JSONL 审计与控制台日志经过同一脱敏，敏感值不再明文落盘
- `fs.find_files` 支持 `**` 跨层级 glob：`**` 匹配任意层级（含零层），`**/*.go` 等含 `/` 的模式不再恒返回 0 结果；单个 `*` 保持不跨 `/`，不含 `**` 段的模式保持既有 basename 语义

## [1.0.0] - 2026-04-30

### Added
- `yolo` 分支默认开启 `unsafe_allow_all=true`，为已授权 MCP 客户端放开文件路径、工作目录、Git 仓库与原始命令执行限制，同时保留控制台日志与 JSONL 审计日志。
- `exec.run` 在 `unsafe_allow_all=true` 时新增原始命令模式，支持：
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
  - `go.list_symbols`
  - `go.find_definition`
- `go.list_symbols` 支持返回单个 Go 文件中的顶层 `func` / `method` / `type` / `var` / `const` 结构索引。
- `go.find_definition` 支持按 `path + line + column` 跳转到 package-level declarations、methods 与 imported package symbols 的定义位置，并对外部定义返回 `in_allowed_roots=false`。
- 扩展 `exec.run` 的 Go preset：
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
- 加固 `exec.run` 参数校验，禁止通过 `flag=value` 形式把输出写到工作目录外，并禁用 `-vettool`。
- 修复 `fs.search_text` 在超长单行文件上因 `bufio.Scanner` token 上限而失败的问题。

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
  - `fs.read_file`
  - `fs.write_file`
  - `fs.list_dir`
  - `fs.stat_path`
  - `fs.make_dir`
  - `fs.move_path`
  - `fs.delete_path`
  - `fs.search_text`
  - `fs.edit_lines`
- Git 白名单工具：
  - `git.status`
  - `git.diff`
  - `git.log`
  - `git.add`
  - `git.restore`
  - `git.commit`
  - `git.branch`
  - `git.switch`
  - `git.pull`
- Exec 工具：
  - `exec.run`
  - 内置 Go preset：`go_fmt` / `go_test` / `go_build` / `go_vet`
- 初始文档与示例：
  - `README.md`
  - `AGENTS.md`
  - `TOOLS-DEFINE.md`
  - `mcp-tools.example.yaml`
  - `scripts/demo_stream.sh`
  - `scripts/demo_capabilities.sh`
