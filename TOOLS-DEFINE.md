# TOOLS-DEFINE.md

本文件面向仓库维护者，记录内部工具的**实现算法、边界条件、安全检查、通知行为**。

目标：后续人工维护时，不必每次重新从代码反推每个工具的行为模型。

---

## 1. 总体设计

工具分三类：

- `fs.*`：文件系统工具
- `git.*`：受限 Git 子命令
- `exec.run`：受限命令执行 preset
- `exec.run_template`：受限固定模板命令执行
- `go.*`：Go-only 导航工具

统一约束：

1. 所有入口最终都经过 MCP registry 注册
2. 所有工具调用都会写审计日志
3. 所有路径/仓库/workdir 都必须落在允许目录内
4. 工具错误统一包装成 MCP tool error，而不是任意 panic / 原始错误泄露

---

## 2. 共有安全机制

### 2.1 allowed roots

核心原则：

- 文件系统目标路径必须在 `AllowedRoots` 内
- 相对路径默认相对于 `StartupDirectory`
- 解析流程会做规范化和白名单判断

### 2.2 审计

每个工具最终都会产生 `mcp.AuditData`，常见字段：

- `TargetPath`
- `Workdir`
- `Allowed`
- `EnvKeys`
- `Stdout`
- `Stderr`
- `ExitCode`
- `ResultDigest`

### 2.3 资源通知

对于会修改文件系统状态的工具，HTTP 层会在成功后触发：

- `notifications/resources/updated`

传播范围：

- 具体文件/目录 URI
- 允许根目录范围内的祖先目录 URI

---

## 3. `fs.*` 工具定义

## 3.1 `fs.read_file`

### 作用
读取允许目录中的 UTF-8 文本文件。

### 关键步骤
1. 解析 `path`
2. 路径落入 allowed roots
3. `os.ReadFile`
4. 返回文本内容 + bytes 摘要

### 副作用
- 无
- 不发送资源更新通知

---

## 3.2 `fs.write_file`

### 作用
原子写入整个文件。

### 关键步骤
1. 解析 `path`
2. 若父目录不存在，则 `MkdirAll`
3. 调用 `atomicWrite`
4. 返回写入字节数

### 核心算法
`atomicWrite(path, payload)`：

1. 读取旧文件权限（若文件已存在）
2. 在同目录创建临时文件
3. 写入 payload
4. `Close`
5. `Rename` 覆盖目标文件

### 副作用
- 会发送 `resources/updated`

---

## 3.3 `fs.list_dir`

### 作用
列目录内容。

### 关键步骤
1. 解析 `path`
2. `os.ReadDir`
3. 同时构造：
   - 文本列表
   - 结构化 entry 列表
4. 排序后返回

### 副作用
- 无通知

---

## 3.4 `fs.stat_path`

### 作用
读取路径元信息。

### 返回重点
- `size`
- `mode`
- `mod_time`
- `is_dir`

### 副作用
- 无通知

---

## 3.5 `fs.make_dir`

### 作用
递归建目录。

### 关键步骤
1. 解析 `path`
2. `os.MkdirAll(path, 0755)`

### 副作用
- 会发送 `resources/updated`

---

## 3.6 `fs.move_path`

### 作用
重命名或移动路径。

### 关键步骤
1. 解析 `src`
2. 解析 `dst`
3. 为 `dst` 父目录补 `MkdirAll`
4. `os.Rename(src, dst)`

### 副作用
- 成功后会对 `src` 和 `dst` 相关 URI 发送更新通知
- 通知也会传播到相关父目录 URI

---

## 3.7 `fs.delete_path`

### 作用
删除文件或空目录。

### 关键步骤
1. 解析 `path`
2. `os.Remove(path)`

### 注意
- 当前不是递归删除
- 若目录非空会失败

### 副作用
- 成功后会发送 `resources/updated`

---

## 3.8 `fs.replace_text`

### 作用
在单个文件中按精确旧文本替换新文本。

### 输入参数
- `path`
- `old_text`
- `new_text`
- `replace_all`（可选）
- `expected_replacements`（可选）

### 关键步骤
1. 解析 `path`
2. 要求 `old_text` 非空
3. 读取整个文件内容
4. 用 `strings.Count` 统计命中数
5. 若没有命中则失败
6. 若提供 `expected_replacements`，则要求总命中数完全一致
7. 默认只替换第一处命中；`replace_all=true` 时替换全部
8. `atomicWrite`

### 副作用
- 成功后会发送 `resources/updated`

---

## 3.9 `fs.search_text`

### 作用
在文件或目录树中按子串搜索。

### 关键步骤
1. 解析 `path`
2. 要求 `query` 非空
3. 若目标为文件，则单文件扫描
4. 若目标为目录，则 `WalkDir`
5. 读取文件后按字节切分换行，不使用默认 64KiB token 上限的 `bufio.Scanner`
6. 命中超过 limit 时提前停止

### 边界说明
- 当前是**子串匹配**，不是正则匹配
- `limit` 默认 200，最大 1000
- 现在可以处理**超长单行**文件，不会因为 `bufio.Scanner: token too long` 直接失败

### 返回结构
每个 match 包括：
- `path`
- `line`
- `text`

### 副作用
- 无通知

---

## 3.10 `fs.apply_unified_diff`

### 作用
对单个文件应用**标准 unified diff**，用于复杂多处修改。

### 输入参数
- `path`
- `diff`
- `expected_old_text`（可选）
- `dry_run`（可选）
- `context_lines`（可选）

### 关键步骤
1. 解析 `path`
2. 解析 `diff` 为 unified diff 结构
3. 要求只包含**单文件** patch
4. 校验 `--- / +++` header 与 `path` 一致（支持常见 `a/` / `b/` 前缀）
5. 读取当前文件快照
6. 若提供 `expected_old_text`，先做整文件前置校验
7. 在内存中逐个 hunk 做**严格命中**校验与模拟应用
8. 任一 hunk 失败则整体失败，不落盘
9. 全部 hunk 通过后，若 `dry_run=false`，执行 `atomicWrite`
10. 返回摘要、受影响范围、digest 与 context snippet

### 严格命中规则
- 第一版不做 fuzz 匹配
- context 行必须逐行完全匹配
- delete 行必须逐行完全匹配
- 任一 hunk 不匹配则整体失败

### 错误模型
失败返回结构化冲突详情，常见 `reason` 包括：
- `path_header_mismatch`
- `multi_file_diff_not_supported`
- `header_mismatch`
- `context_mismatch`
- `delete_mismatch`
- `expected_old_text_mismatch`

### 副作用
- 成功后会发送 `resources/updated`

---

## 3.11 `fs.edit_lines`

### 作用
按 **1-based 行区间** 做严格行替换，减少上下文传输成本。

### 输入参数
- `path`
- `start_line`
- `end_line`
- `new_text`
- `expected_old_text`（可选）
- `context_lines`（可选）

### 关键算法
1. 解析 `path`
2. 校验 `start_line` / `end_line`
3. 读取整个文件内容
4. 用 `splitLinesPreserve(oldFile)` 把原文件拆成“保留原始换行的行切片”
5. 校验编辑区间不能越界
6. 取出旧区间文本：
   - `oldText = strings.Join(lines[start-1:end], "")`
7. 若提供 `expected_old_text`，则要求它与 `oldText` **完全相等**
8. 对 `new_text` 调用 `normalizeReplacementLines(newText)`：
   - 按逻辑行拆分
   - 即使输入没有尾换行，也会把每一逻辑行标准化成一条完整行
   - 这样可以避免替换后与后续内容黏连
9. 用切片拼接构造新文件：
   - `lines[:start-1]`
   - `replacement`
   - `lines[end:]`
10. `strings.Join(updated, "")`
11. `atomicWrite`
12. 重新渲染上下文片段 `context_snippet`

### 语义说明
当前语义是：

- **严格按行替换**
- `new_text` 按逻辑行解释
- `new_text` 中的**中间空行会保留**，不会被自动忽略
- `new_text == ""` 表示删除目标区间
- `new_text == "\n"` 表示替换成 1 个空行
- 调用方仍需显式控制自己想替换成几行
- 但即使忘记在 `new_text` 末尾补换行，也不会再把后续内容黏上去

### 已覆盖测试
- 扩行：1 行替换成 2 行
- 缩行：2 行替换成 1 行
- 单行 replacement 无尾换行
- 多行 replacement 无结尾换行
- `expected_old_text` 精确匹配 / 不匹配

### 副作用
- 成功后会发送 `resources/updated`

---

## 4. `git.*` 工具定义

## 4.1 总体约束

Git 工具并不暴露任意 git 命令。
只允许固定白名单：

- `status`
- `diff`
- `log`
- `add`
- `restore`
- `commit`
- `branch`
- `switch`
- `pull`

### 共同安全策略
1. 先解析 `repo_path`；若未提供则默认使用 `StartupDirectory`
2. `repo_path` 必须落在 allowed roots 中
3. 再执行 `git rev-parse --show-toplevel`
4. 真实仓库根也必须仍然落在 allowed roots 中
5. 所有 git 命令都带：
   - `GIT_TERMINAL_PROMPT=0`
6. 非白名单子命令直接拒绝
7. `git.add` / `git.restore` / `git.diff` 的 `paths` 必须是 repo-relative，不能是绝对路径，也不能用 `..` 越界

---

## 4.2 `git.status`
固定执行：

```text
git status --short --branch
```

只读。

## 4.3 `git.diff`
固定前缀：

```text
git diff [-- paths...]
```

仅允许显式 repo-relative paths。

## 4.4 `git.log`
固定模式：

```text
git log --oneline -<limit>
```

`limit` 有上界。

## 4.5 `git.add`
仅允许显式 repo-relative paths。

## 4.6 `git.restore`
仅允许显式 repo-relative paths。

## 4.7 `git.commit`
要求：

- `message` 非空

执行：

```text
git commit -m <message>
```

## 4.8 `git.branch`
只读列分支。

## 4.9 `git.switch`
要求：

- branch 非空
- 不能以 `-` 开头
- 不能包含空白字符

## 4.10 `git.pull`
固定为：

```text
git pull --ff-only
```

---

## 5. `exec.run` 工具定义

### 作用
在允许目录下执行预定义 preset。

### 关键步骤
1. 读取 `preset`
2. 从配置中查找 `ExecPresets[preset]`
3. 若不存在，直接拒绝
4. 解析 `workdir`
5. 若相对路径，则相对于 `StartupDirectory`
6. 要求 `workdir` 落在 allowed roots 内
7. 校验 `args`
8. 合成 argv：
   - `FixedArgs`
   - 用户额外参数
   - 若无 target，则为部分 preset 自动补 `./...`
9. `timeout_override_sec` 只允许**缩短**，不允许放大超过 preset 默认超时
10. `exec.CommandContext`
11. stdout/stderr 截断后写审计

### 当前 preset 设计
面向 Go：

- `go_fmt`
- `go_mod_download`
- `go_test`
- `go_generate`
- `go_build`
- `go_vet`
- `go_mod_tidy`
- `go_get`
- `go_list`
- `go_work_sync`

### 参数白名单策略
- 以 `-` 开头的参数必须匹配 `AllowedArgs`
- 非 flag 参数默认必须是 workdir 内的本地 target
- 仅 `go_get` 第一版允许受控模块参数（如 `example.com/mod@v1.2.3`）
- `go_test` / `go_generate` / `go_build` / `go_vet` 在没有显式 target 时会自动补 `./...`
- `go_mod_download` 会直接在 `workdir` 中执行 `go mod download`
- `go_mod_tidy` 不会自动补 target，而是直接在 `workdir` 中执行 `go mod tidy`
- `go_work_sync` 不会自动补 target，而是直接在 `workdir` 中执行 `go work sync`
- `timeout_override_sec` 只允许缩短默认超时
- 禁止绝对路径、URL、`..` 越界路径
- 对 `-o=...`、`-coverprofile=...` 这类 inline 路径参数也会再次校验，不能借 `flag=value` 写到 workdir 外
- `-vettool` 当前明确不支持

### Go managed cache 策略
- 所有 Go preset 当前都会注入固定 env：
  - `GOCACHE`
  - `GOMODCACHE`
  - `GOTMPDIR`
- 默认写入 `~/.mcp-tools/cache/...`
- `runExecCommand` 在执行前会主动 `mkdir -p` 这些目录
- 目标是把依赖下载、编译缓存与临时目录写入收口到受控范围，而不是散落到外部系统目录

### 输出策略
- stdout/stderr 会按 `OutputMaxBytes` 截断
- 返回结构中会带：
  - `preset`
  - `workdir`
  - `stdout`
  - `stderr`
  - `argv`
  - `env_keys`（若 preset 配置了固定 env）

---

## 5.1 `exec.run_template` 工具定义

### 作用
在允许目录下执行服务端配置好的固定命令模板。

### 输入参数
- `template`
- `workdir`
- `timeout_override_sec`（可选）

### 关键步骤
1. 读取 `template`
2. 从配置中查找 `CommandTemplates[template]`
3. 若不存在则拒绝
4. 解析 `workdir` 并要求其落在 allowed roots 内
5. 若模板配置了固定 `env`，则附加到子进程环境中
6. 若模板配置了 `allowed_workdirs`，则 `workdir` 还必须命中这些范围之一
7. 若模板 `requires_confirmation=true`，则调用方必须显式传 `confirm=true` 才允许执行
8. 模板风险语义（`category` / `destructive` / `requires_confirmation`）会随 schema、result、statez 一起暴露
9. 模板提供固定 argv，客户端不再额外传 args
10. `timeout_override_sec` 只允许缩短
11. 执行并审计

### 默认模板
- `make_test`
- `make_build`
- `go_clean_testcache`（默认 `requires_confirmation=true`）

### discoverability
- `exec.run_template` 的 schema 中，`template` 字段会带当前模板名 `enum`
- `/debug/statez` 会返回模板元信息（command/category/destructive/requires_confirmation/read_only/env_keys/allowed_workdirs）和模板调用计数

---

## 5.2 `go.*` 工具定义

### 5.2.1 总体约束

- 第一版是 **Go-only**
- 不依赖 `gopls` 守护进程
- 内部使用：
  - `go/packages` 负责加载真实 Go package / type 信息
  - `go/ast` 负责抽取文件结构
- 所有 `path` 仍需落在 `allowed_roots` 内
- `go.find_definition` 若跳到 `allowed_roots` 外，只返回位置，不放开读取边界

### 5.2.2 `go.list_symbols`

#### 作用
返回单个 Go 文件中的顶层结构索引，供 agent 做 outline。

#### 输入参数
- `path`

#### 关键步骤
1. 解析 `path`
2. 要求目标必须是 `.go` 文件
3. 用 `go/packages` 加载该文件所属 package
4. 只遍历目标文件 AST
5. 抽取顶层声明：
   - `func`
   - `method`
   - `type`
   - `var`
   - `const`
6. 为每个 symbol 返回名称、类型、导出性与源码范围

#### 边界说明
- 第一版只做**单文件** symbols
- 不做 package-level 聚合
- 不做 workspace-wide symbols

### 5.2.3 `go.find_definition`

#### 作用
按 `path + line + column` 解析 Go 标识符并返回定义位置。

#### 输入参数
- `path`
- `line`
- `column`

#### 关键步骤
1. 解析 `path`
2. 用 `go/packages` 加载所属 package，并要求拿到 type info
3. 把 `line + column` 转成 `token.Pos`
4. 在目标文件 AST 中找到对应 identifier
5. 优先处理 selector / method selection
6. 用 `types.Info` 取 `Uses` / `Defs` / `Selections`
7. 只接受第一版支持的对象范围：
   - package-level `func`
   - `method`
   - package-level `type`
   - package-level `var`
   - package-level `const`
   - imported package symbols
8. 返回定义位置 + 最小符号摘要

#### 边界说明
- 当前不保证支持局部变量 / label 等更细粒度局部目标
- 若定义落在 `allowed_roots` 外，会返回：
  - `definition_path`
  - `definition_line`
  - `definition_column`
  - `in_allowed_roots=false`

#### 常见失败 reason
- `not_go_file`
- `package_load_failed`
- `parse_failed`
- `identifier_not_found`
- `definition_not_resolved`
- `position_out_of_bounds`
- `path_outside_allowed_roots`

---

## 6. HTTP / transport 与工具关系

### `tools/call`
工具执行统一走 MCP registry。

执行成功后：
- 返回 `mcp.Result`
- 记录审计
- 若属于文件修改型工具，则进一步触发 `resources/updated`

### SSE / Streamable HTTP 注意点
当前兼容策略：

- mixed `Accept: application/json, text/event-stream` 时优先 JSON
- 纯 SSE 请求才返回单次 SSE
- 长连接 stream 里所有 JSON-RPC 消息统一用：

```text
event: message
```

这样更兼容官方 Streamable HTTP 客户端。

内部实现约束：

- `streamHub` 在 publish / publishNotification 时保持读锁遍历当前 stream 集合，避免和 `unregister` / `closeSession` 并发时触发 `send on closed channel`
- 每个 stream 当前使用可配置的 queue；默认 **128** 条 envelope 缓冲，减少 burst 异步请求下退化成单次 SSE 的概率
- 如果后续想再调小缓冲，必须先重跑并发专项测试

### `/debug/statez`

当前服务提供 Bearer Token 保护的只读调试接口：

- `GET /debug/statez`

主要用于维护者判断：

- 当前 active sessions / streams / resource subscriptions
- async stream publish 是否频繁 fallback
- `notifications/resources/updated` 是否存在明显投递丢失
- 当前 `exec_presets` / `command_templates` 的元信息摘要（包括 `env_keys`）

---

## 7. 后续维护时最容易改坏的点

1. **`fs.apply_unified_diff` 的 patch 语义**
   - 不要再退回到按原样拼接 replacement 文本，否则会重新引入行黏连问题

2. **mixed Accept 的分流逻辑**
   - 不要只因为看到 `text/event-stream` 就强制走 SSE
   - 否则会破坏官方 SDK 客户端初始化

3. **resource update 通知传播**
   - 不仅是具体文件，还包括父目录 URI

4. **git/exec 白名单**
   - 新增能力时不要引入任意命令透传

5. **notification 语义**
   - `notifications/*` 当前是 `202 + empty body`
   - 改动前必须确认与现有客户端兼容性
