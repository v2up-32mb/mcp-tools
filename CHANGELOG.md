# CHANGELOG

本项目变更记录。

格式参考 Keep a Changelog，并结合当前仓库实际开发节奏维护。

## [Unreleased]

## [1.0.0] - 2026-04-30

### Added
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
