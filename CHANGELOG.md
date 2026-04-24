# CHANGELOG

本项目变更记录。

格式参考 Keep a Changelog，并结合当前仓库实际开发节奏维护。

## [Unreleased]

### Added
- 新增 Go-only 导航工具：
  - `go.list_symbols`
  - `go.find_definition`
- `go.list_symbols` 支持返回单个 Go 文件中的顶层 `func` / `method` / `type` / `var` / `const` 结构索引。
- `go.find_definition` 支持按 `path + line + column` 跳转到 package-level declarations、methods 与 imported package symbols 的定义位置，并对外部定义返回 `in_allowed_roots=false`。

### Fixed
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
