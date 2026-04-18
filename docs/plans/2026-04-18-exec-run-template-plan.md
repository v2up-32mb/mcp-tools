# exec.run_template Implementation Plan

## 背景
当前 `exec.run` 采用固定 preset 模式，已经足够安全，但在真实开发场景里仍然偏刚性。用户希望“逐步放开命令行执行”，同时明确不希望直接开放任意 shell。

## 目标
新增一个受控的模板命令执行能力 `exec.run_template`：

- 模板由服务端配置声明
- 客户端只能引用模板名
- 不允许任意 shell 字符串执行
- 继续受 `allowed_roots`、审计、timeout、输出截断约束

## v1 范围
### 新工具
- `exec.run_template`

### 新配置
- `exec.command_templates.<name>`

建议模板结构：
- `command`: string[]
- `timeout_sec`: int
- `read_only`: bool

### 默认模板
- `make_test` → `make test`
- `make_build` → `make build`
- `go_clean_testcache` → `go clean -testcache`

## 非目标
- 不开放任意 shell
- 不支持管道、重定向、shell 内置语义
- v1 不支持模板额外 args
- v1 不支持环境变量透传配置

## 核心实现步骤
1. 扩展 `config.Config` 与 YAML 结构
2. 定义默认 command templates
3. 在 `internal/tools/execx` 中新增 `exec.run_template`
4. 复用现有 workdir 校验、timeout、stdout/stderr 截断、审计逻辑
5. 更新 MCP 工具 description
6. 补测试与文档

## 验证标准
- `tools/list` 中出现 `exec.run_template`
- 默认模板可执行
- 未知模板被拒绝
- workdir 越界被拒绝
- timeout override 只可缩短
- 全量 `go test ./...` 通过
