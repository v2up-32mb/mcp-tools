# AGENTS.md

本文件面向两类读者：

1. 接入本仓库 MCP 服务的远程 AI 代理 / 本地自动化调用方
2. 后续维护本仓库的人类开发者

目标只有一个：让调用方知道**该怎么安全、稳定、低出错地使用这台 MCP 服务**。

---

## 1. 服务定位

这是一个基于 Go 1.20 的单体 MCP 服务，主要面向“远程 AI 代理安全操作本地工作区”的场景。

当前能力边界：

- 文件系统工具：读、写、列目录、查找、局部编辑、移动、删除
- Git 白名单工具：只开放固定子命令，不开放任意 git 命令
- Exec 工具：只开放受限 Go toolchain preset，不开放任意 shell
- MCP 能力：tools / resources / prompts / completion
- 传输层：JSON、单次 SSE、带 session 的 GET stream + async POST
- 运行观测：Bearer Token 保护的 `/debug/statez`

---

## 2. Transport 与协议约定

### 2.1 HTTP JSON

默认入口：

- `POST /mcp`

特点：

- 同步 request-response
- 如果请求 `Accept` 同时包含 `application/json` 和 `text/event-stream`，服务端优先返回 JSON
- 这是为了兼容官方 Streamable HTTP 客户端的 mixed Accept 行为

### 2.2 单次 SSE

当且仅当客户端显式偏好 SSE（典型是 `Accept: text/event-stream`，且不同时要求 JSON）时，`POST /mcp` 会返回单次 SSE。

事件格式：

```text
event: message
data: {jsonrpc response or notification}
```

### 2.3 长连接 stream

- `GET /mcp`
- 必须带：
  - `Authorization: Bearer <token>`
  - `Accept: text/event-stream`
  - `Mcp-Session-Id: <session id>`

行为：

- 仅允许在 session 已初始化后建立
- 打开后会先发送 `: stream opened`
- 会周期性发送 `: heartbeat`
- 已有活动 stream 的 session，再发 `POST /mcp` + SSE Accept 时，结果会异步投递到该 stream，HTTP 只返回 `202 Accepted`

### 2.4 Session 与协议头

#### 初始化

先调用：

- `initialize`

服务端返回：

- `Mcp-Session-Id`
- `MCP-Protocol-Version`

初始化后客户端应发送：

- `notifications/initialized`

#### 协议头

所有带有效 session 的响应应视为会返回：

- `MCP-Protocol-Version`

`GET /debug/statez` 不需要 session，但需要 Bearer Token。

如果请求中显式携带 `MCP-Protocol-Version` 且与 session 不一致，服务端会拒绝请求。

#### 删除 session

- `DELETE /mcp`
- 需要 `Mcp-Session-Id`
- 删除时会同时清理：
  - session
  - active stream
  - 该 session 的 resource subscriptions

### 2.5 Generic notifications

所有 `notifications/*` 当前统一按 notification 语义处理：

- 返回 `202 Accepted`
- 不返回 JSON-RPC body
- 若携带有效 session，响应头仍会带 `MCP-Protocol-Version`

---

## 3. 当前 capabilities

`initialize` 当前会声明：

- `tools.listChanged = false`
- `resources.subscribe = true`
- `resources.listChanged = false`
- `prompts.listChanged = false`
- `completions = {}`

当前**不会**发送：

- `notifications/tools/list_changed`
- `notifications/resources/list_changed`
- `notifications/prompts/list_changed`

原因：

- 当前工具集、资源模板、prompt 列表不是运行时动态注册
- 所以 `listChanged=false` 与实现一致

---

## 4. 支持的 MCP methods

### tools

- `tools/list`
- `tools/call`

### resources

- `resources/list`
- `resources/templates/list`
- `resources/read`
- `resources/subscribe`
- `resources/unsubscribe`

### prompts

- `prompts/list`
- `prompts/get`

### utilities

- `completion/complete`
- `ping`

---

## 5. 工具清单

### 5.1 文件工具

- `fs.read_file`
- `fs.write_file`
- `fs.list_dir`
- `fs.stat_path`
- `fs.make_dir`
- `fs.move_path`
- `fs.delete_path`
- `fs.search_text`
- `fs.replace_text`
- `fs.edit_lines`

### 5.2 Git 工具

- `git.status`
- `git.diff`
- `git.log`
- `git.add`
- `git.restore`
- `git.commit`
- `git.branch`
- `git.switch`
- `git.pull`

明确不支持：

- `git push`
- 任意原始 git 子命令透传
- 任意 credential / global config 相关操作

### 5.3 Exec 工具

- `exec.run`
- `exec.run_template`

当前内置 preset 面向 Go：

- `go_fmt`
- `go_mod_download`
- `go_test`
- `go_generate`
- `go_build`
- `go_vet`
- `go_mod_tidy`

当前内置 command templates：

- `make_test`
- `make_build`
- `go_clean_testcache`
- `go_mod_tidy`

当前内置 command templates：

- `make_test`
- `make_build`
- `go_clean_testcache`

这不是 shell。客户端不能发送任意命令字符串。

---

## 6. 安全边界

### 6.1 目录范围

所有文件、git、exec 行为都必须落在：

- 服务启动目录
- 配置中的 `allowed_roots`

内部会统一做：

- 路径规范化
- 绝对路径化
- 允许根目录校验
- 仓库/工作目录白名单校验

### 6.2 鉴权

- Bearer Token 必需
- 浏览器 `Origin` 若未被允许，则拒绝

### 6.3 审计

每次工具调用都会进入 JSONL 审计日志。

重点字段包括：

- tool 名
- 参数摘要
- target path / workdir
- stdout / stderr 摘要
- result digest
- exit code
- 成功/失败

### 6.4 资源通知

当前会在以下文件修改型工具成功后发送：

- `fs.write_file`
- `fs.replace_text`
- `fs.edit_lines`
- `fs.make_dir`
- `fs.move_path`
- `fs.delete_path`

通知方法：

- `notifications/resources/updated`

且会传播到：

- 具体资源本身
- 允许根目录范围内的祖先目录 URI

所以订阅目录资源时，可以感知子项变更。

### 6.5 运行观测

当前提供：

- `/healthz`
- `/readyz`
- `/debug/statez`

其中 `/debug/statez` 为只读调试接口，当前主要返回：

- 活跃 session 数
- 活跃 SSE stream session / connection 数
- 资源订阅 session / total 数
- async stream 投递次数 / 成功次数 / fallback 次数
- `notifications/resources/updated` 的投递/丢弃计数

---

## 7. `fs.edit_lines` 使用约定（重要）

这是最推荐给 AI 代理使用的低上下文编辑工具。

### 当前语义

- `start_line` / `end_line` 为 **1-based** 行区间
- `new_text` 按**逻辑行**解释
- 服务端会按**严格行替换**执行，不再把后续内容黏连到替换片段后面
- `new_text` 中的**中间空行会保留**，不会被自动忽略
- `new_text == ""` 表示删除目标区间；`new_text == "
"` 表示替换成 1 个空行
- 调用方仍应**显式控制自己想要的换行结构**

### 推荐做法

1. 先 `fs.read_file`
2. 再用 `fs.edit_lines`
3. 有并发敏感性时附带 `expected_old_text`
4. 编辑后再 `fs.read_file` 或 `exec.run` 做验证

### 什么时候不用 `fs.edit_lines`

以下场景建议直接 `fs.write_file`：

- 要重写整个文件
- 需要做复杂结构化重排
- 已经明确掌握完整目标内容

---

## 7.1 `fs.replace_text` 使用约定

- 按精确旧文本替换新文本
- 默认只替换第一处命中
- `replace_all=true` 时替换所有命中
- `expected_replacements` 可用于命中数保护

## 7.2 `fs.search_text` 使用约定

- 当前是**子串匹配**，不是正则
- `path` 可以是文件或目录
- 目录会递归搜索
- 默认 `limit=200`，上限 `1000`
- 现在已支持超长单行文件

## 7.3 `git.*` 使用约定

- 不传 `repo_path` 时，默认以服务启动目录作为仓库入口
- `repo_path` 和真实仓库根都必须落在 `allowed_roots` 内
- `git.add` / `git.restore` / `git.diff` 的 `paths` 必须是 repo-relative
- `git.pull` 固定为 `--ff-only`

## 7.4 `exec.run` 使用约定

- 不是 shell，只能运行预定义 preset
- `workdir` 必填，且必须落在 `allowed_roots` 内
- `go_test` / `go_generate` / `go_build` / `go_vet` 没有显式 target 时会自动补 `./...`
- `go_mod_download` 会直接在 `workdir` 里执行 `go mod download`
- `go_mod_tidy` 会直接在 `workdir` 里执行 `go mod tidy`
- `timeout_override_sec` 只能缩短默认超时
- `-vettool` 不支持
- `-o=...` / `-coverprofile=...` 这类 inline 路径值也会再次校验，不能写到 workdir 外

---

## 7.5 `exec.run_template` 使用约定

- 不是任意 shell，而是引用服务端配置好的固定模板
- 客户端只能指定 `template` 和 `workdir`
- 模板可以有服务端固定 `env`，客户端不能覆盖
- 模板可以额外配置 `allowed_workdirs`，进一步收紧工作目录范围
- `workdir` 仍然必须落在 `allowed_roots` 内
- 适合逐步放开命令执行，而不炸开安全边界

## 8. 推荐调用流程

### 文件修改型任务

1. `initialize`
2. `notifications/initialized`
3. `tools/list` 或直接进入文件工具
4. `fs.read_file`
5. `fs.edit_lines` / `fs.write_file`
6. `fs.read_file` 验证
7. 如需要，再 `exec.run` / `git.*`
8. 完成后可 `DELETE /mcp`

### 代码任务（Go）

1. 读取目标文件
2. 最小改动优先使用 `fs.edit_lines`
3. `exec.run(go_fmt)`
4. `exec.run(go_test)`
5. 若需要，再 `git.status` / `git.diff`

---

## 9. 维护约束

后续修改服务时，请保持以下文档同步：

- `README.md`：对外用户文档
- `AGENTS.md`：对调用方/维护者的使用约束文档
- `TOOLS-DEFINE.md`：内部工具算法与边界说明

如果以下任一发生变化，必须同步更新 `TOOLS-DEFINE.md`：

- 工具参数 schema
- 路径/仓库/workdir 校验规则
- 资源通知触发条件
- `fs.edit_lines` 行编辑语义
- git / exec 白名单
