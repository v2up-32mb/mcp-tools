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
5. YAML 配置加载允许空文件/仅注释文件表示“不做 YAML 覆盖”；非空配置只接受单个文档，并启用未知字段拒绝，显式 `null` / `~` 文档以及字段值/列表项中的显式空值都会被拒绝，避免安全关键配置拼写错误、额外文档或空占位被静默忽略；`allowed_roots` / `MCP_ALLOWED_ROOTS` 与 `allowed_origins` / `MCP_ALLOWED_ORIGINS` 条目不能为空；`supported_protocols` 显式空列表会被拒绝；`git.allowed_subcommands` 可显式为空以禁用全部 Git 子命令，但列表项本身不能是空字符串

当前 `yolo` 分支新增：

- `unsafe_allow_all`

当该配置为 `true` 时：

- `fs.*` 路径解析不再要求落在 `allowed_roots`
- `git.*` 的 `repo_path` / repo root 不再要求落在 `allowed_roots`
- `go.*` 的目标文件不再要求落在 `allowed_roots`
- `exec.run` 允许原始命令模式

---

## 2. 共有安全机制

### 2.1 allowed roots

核心原则：

- 文件系统目标路径必须在 `AllowedRoots` 内
- 相对路径默认相对于 `StartupDirectory`
- 解析流程会做规范化和白名单判断
- 配置中的 `allowed_roots` 是附加白名单目录，列表项不能为空；相对项按配置文件目录解析
- 环境变量 `MCP_ALLOWED_ROOTS` 同样是附加白名单目录，逗号分隔的每一段都会保留并校验；空段（包括尾随逗号）按配置错误拒绝
- `allowed_origins` / `MCP_ALLOWED_ORIGINS` 是浏览器 `Origin` 白名单，条目会 trim 并去重，但空白条目或环境变量空段会按配置错误拒绝

例外：

- 当 `unsafe_allow_all=true` 时，上述 allowed-roots 限制对工具调用层失效
- 此时仍会做：
  - 路径规范化
  - 绝对路径化
  - 目录存在性检查（对 workdir/repo）

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

审计日志 writer 当前支持：

- 默认 JSONL 追加写
- 按大小滚动
- 固定份数旧文件保留

相关配置：

- `audit_rotate_max_mb`
- `audit_rotate_max_backups`

轮转规则：

1. 启动时仅 `Stat` 一次当前文件大小
2. 每次写入前先 `Marshal` 当前 event
3. 用内存中的 `currentSize + len(payload)` 判断是否超阈值
4. 若即将超限：
   - 关闭当前文件
   - 旧文件按 `.N -> .N+1` 倒序重命名
   - 当前文件重命名为 `.1`
   - 超出 `max_backups` 的最老文件删除
   - 新建空的当前文件
5. 写入成功后更新内存中的 `currentSize`

实现前提：

- 默认只考虑当前进程写审计日志
- 轮转失败时直接返回错误，不静默降级
- 轮转失败后会尽量重新打开当前审计日志，避免 writer 持有已关闭文件句柄导致后续写入无法重试

参数记录策略：

- 审计事件记录参数摘要，不直接保存大文本/敏感参数全文
- `text` / `content` / `old_text` / `new_text` / `expected_old_text` / `diff` 仅记录 bytes、lines、sha256
- `env` 仅记录 key 列表和数量
- `exec.*` 的 `args` 仅记录数量

### 2.3 资源通知

对于会修改文件系统状态的工具，HTTP 层会在成功后触发：

- `notifications/resources/updated`

传播范围：

- 具体文件/目录 URI
- 允许根目录范围内的祖先目录 URI

`resources/read` / `resources/subscribe` / `resources/unsubscribe` 的 `uri` 参数必须是 JSON string。它们统一按本地 `file://` URI 解析路径：允许空 host 或 `localhost`，按 URI path escaping 解码（例如 `%20` 为空格），拒绝 query、fragment 和非本地 authority。读资源响应、订阅存储和更新通知都会使用解析后再按 URI path escaping 转义的规范化 `file://` URI，避免 `./`、符号链接等等价路径不一致，也避免文件名中的 `#` / `?` 被后续客户端误解析成 fragment / query。
`resources/unsubscribe` 的幂等性只适用于合法且可规范化的 URI；非法 URI 仍应返回 invalid params，不能被静默当作“无订阅”成功。

过期 session 被访问或 `/debug/statez` 采样时，会同步清理该 session 的 stream 与 resource subscriptions，避免旧订阅长期影响通知投递/丢弃计数。

### 2.4 控制台日志

当前统一使用 `internal/applog`。

目标：

- 控制台日志给人读
- 审计日志给机器和事后追查

配置：

- `log_level`
  - `INFO`
  - `WARN`
  - `ERROR`
  - `DEBUG`

输出约定：

- 多行块状日志
- 第一行固定为时间 / 级别 / 组件
- 第二行是主消息
- 后续行按固定顺序输出字段块

级别语义：

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
  - HTTP/MCP 生命周期细节
  - 脱敏调试字段全展开

字段约束：

- `INFO/WARN` 不输出 `request_id` / `session_id`
- `ERROR/DEBUG` 才输出追踪字段
- 工具调用只输出白名单参数摘要
- 大字段只输出摘要，不打印完整 `content` / `new_text` / `diff`

### 2.5 yolo 分支危险模式

`unsafe_allow_all=true` 时的行为模型：

1. `fs.*`
   - `resolvePathArg` 走 `security.ResolvePathUnsafe`
2. `exec.run`
   - 除 preset 模式外，额外接受原始：
     - `command`
     - `args`
     - `env`
     - `workdir`
3. `git.*`
   - `resolveRepo` 不再调用 allowed-roots workdir 校验
4. `go.*`
   - Go 文件路径解析不再受 `allowed_roots` 限制
5. HTTP 资源读取/资源更新
   - `resources/read` 与资源更新目标解析同样按 unsafe 模式放开

这不会关闭：

- Bearer Token
- 控制台日志
- 审计日志

### 2.6 数字参数校验

工具层不能假设客户端一定执行 JSON Schema 校验，因此所有数值参数在调用层再次校验：

- HTTP JSON-RPC `params` 内数字会以 `json.Number` 保留原始 token；直接工具调用测试里的 `float64` 仍会被兼容校验
- 整数参数必须按原始 token/值解析为整数，接受数值上等于整数的 JSON number（如 `42.0`、`1e3`），但仍拒绝小数、NaN/Inf 和超出平台 `int` 范围的值，避免大数小数经 `float64` 舍入后被误接受
- 可选整数参数类型错误时返回 tool error，不再静默忽略
- `limit` / `context_lines` 等有上界的参数按工具语义 cap 或拒绝
- `timeout_override_sec` 必须是正整数；工具 schema 声明 `minimum: 1`，调用层仍会再次校验，只允许缩短默认 timeout，且会先避免 `time.Duration` 乘法溢出

---

## 3. `fs.*` 工具定义

### 通用参数校验

- `path` / `src` / `dst` 等路径参数必须是非空 JSON string；显式非字符串会返回 `must be a string`，不会被静默当作缺失参数。
- 必填文本参数会在工具执行和落盘前再次做类型校验：`fs.write_file.text`、`fs.search_text.query`、`fs.replace_text.old_text` / `new_text`、`fs.edit_lines.new_text`、`fs.apply_unified_diff.diff` 显式非字符串均直接校验失败。
- 运行时已拒绝空字符串的字段会在 schema 中同步声明 `minLength: 1`：路径参数（`path` / `src` / `dst`）、`fs.search_text.query`、`fs.replace_text.old_text`、`fs.apply_unified_diff.diff`。
- 空字符串语义仍由各工具单独定义：`text` / `new_text` 可为空，`query` 只拒绝 `""`，`old_text` 不能为空，`diff` 不能为空白。

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
2. 校验 `text` 是 JSON string（允许空字符串）
3. 若父目录不存在，则 `MkdirAll`
4. 调用 `atomicWrite`
5. 返回写入字节数

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
1. 解析 `src`，但不跟随最终路径组件的符号链接
2. 解析 `dst`，但不跟随最终路径组件的符号链接
3. 为 `dst` 父目录补 `MkdirAll`
4. `os.Rename(src, dst)`

### 边界说明
- 若 `src` 或已存在的 `dst` 最终路径组件是符号链接，移动的是符号链接目录项本身
- 父目录仍会解析符号链接并校验 allowed roots，不能借父级符号链接逃逸

### 副作用
- 成功后会对 `src` 和 `dst` 相关 URI 发送更新通知
- 通知也会传播到相关父目录 URI

---

## 3.7 `fs.delete_path`

### 作用
删除文件或空目录。

### 关键步骤
1. 解析 `path`，但不跟随最终路径组件的符号链接
2. `os.Remove(path)`

### 注意
- 当前不是递归删除
- 若目录非空会失败
- 若最终路径组件是符号链接，删除的是符号链接本身；父目录仍会解析符号链接并校验 allowed roots

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
2. 校验 `old_text` / `new_text` 是 JSON string，并要求 `old_text` 非空
3. 读取整个文件内容
4. 用 `strings.Count` 统计命中数
5. 若没有命中则失败
6. 若提供 `expected_replacements`，则要求总命中数完全一致
7. 默认只替换第一处命中；`replace_all=true` 时替换全部
8. 若提供 `replace_all`，必须是 JSON boolean；非布尔值直接校验失败，不落盘
9. `atomicWrite`

### 副作用
- 成功后会发送 `resources/updated`

### 边界说明
- `expected_replacements` 必须是整数
- `expected_replacements < 0` 会被拒绝（未提供时内部使用 `-1` 表示“不校验”）
- `expected_replacements` schema 声明 `minimum: 0`；调用层仍会重复校验
- `path` / `old_text` schema 声明 `minLength: 1`；`new_text` 允许空字符串，不声明该约束

---

## 3.9 `fs.search_text`

### 作用
在文件或目录树中按子串搜索。

### 关键步骤
1. 解析 `path`
2. 校验 `query` 是 JSON string 且不是空字符串；不做 trim，空格/制表符等纯空白 query 仍是合法字面子串
3. 若目标为文件，则单文件扫描
4. 若目标为目录，则 `WalkDir`
5. 非 `unsafe_allow_all` 模式下，对每个待读取文件重新做 allowed-root 校验，跳过指向范围外的符号链接
6. 读取文件后按字节切分换行，不使用默认 64KiB token 上限的 `bufio.Scanner`
7. 命中达到 limit 时提前停止并返回已收集的部分结果

### 边界说明
- 当前是**子串匹配**，不是正则匹配
- `query` 必须是 JSON string；非字符串直接校验失败
- `query == ""` 拒绝，schema 声明 `minLength: 1`；`strings.TrimSpace(query) == ""` 但原始 query 非空时允许搜索
- `path` schema 声明 `minLength: 1`
- `limit` 必须是整数；默认 200，最大 1000
- `limit <= 0` 使用默认值 200，`limit > 1000` cap 到 1000
- 目录模式不会通过文件符号链接读取 `allowed_roots` 外内容；`unsafe_allow_all=true` 时仍按 yolo 语义放开
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
2. 校验 `diff` 是非空白 JSON string，并解析为 unified diff 结构
3. 要求只包含**单文件** patch
4. 校验 `--- / +++` header 与 `path` 一致（支持常见 `a/` / `b/` 前缀；相对 header 只把实际 `..` 路径段视为越界，`..data/file.txt` 这类普通目录名仍可匹配启动目录内文件）
5. 读取当前文件快照
6. 若提供 `expected_old_text`，先做整文件前置校验；显式空字符串表示期望当前文件为空
7. 在内存中逐个 hunk 做**严格命中**校验与模拟应用
8. 任一 hunk 失败则整体失败，不落盘
9. 若提供 `dry_run`，必须是 JSON boolean；非布尔值直接校验失败，不落盘
10. 全部 hunk 通过后，若 `dry_run=false`，执行 `atomicWrite`
11. 返回摘要、受影响范围、digest 与 context snippet

### 严格命中规则
- 第一版不做 fuzz 匹配
- context 行必须逐行完全匹配
- delete 行必须逐行完全匹配
- 任一 hunk 不匹配则整体失败
- hunk header 中的行号与行数必须能解析为平台 `int`，溢出会作为无效 diff 拒绝
- hunk header 的非空旧/新 range 起始行号必须为正数；`0` 只允许搭配 `,0` 空 range，避免 `@@ -1 +0 @@` 这类无效新文件行号被接受

### 错误模型
失败返回结构化冲突详情，常见 `reason` 包括：
- `invalid_unified_diff`
- `path_header_mismatch`
- `multi_file_diff_not_supported`
- `header_mismatch`
- `context_mismatch`
- `delete_mismatch`
- `expected_old_text_mismatch`

### 副作用
- 成功后会发送 `resources/updated`

### 边界说明
- `diff` 必须是 JSON string；非字符串直接校验失败，不落盘
- `path` / `diff` schema 声明 `minLength: 1`
- `expected_old_text` 必须是 JSON string；非字符串直接校验失败，不落盘
- `context_lines` 必须是整数
- `context_lines < 0` 会被拒绝，`context_lines > 20` cap 到 20
- `context_lines` schema 声明 `minimum: 0`；调用层仍会重复校验并保留大值 cap 语义

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
7. 若提供 `expected_old_text`，则要求它与 `oldText` **完全相等**；显式空字符串也会参与匹配
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
- `start_line` / `end_line` / `context_lines` 必须是整数
- `start_line` / `end_line` schema 声明 `minimum: 1`
- `context_lines` schema 声明 `minimum: 0`
- `new_text` 必须是 JSON string；非字符串直接校验失败，不落盘
- `new_text` 按逻辑行解释
- `new_text` 中的**中间空行会保留**，不会被自动忽略
- `new_text == ""` 表示删除目标区间
- `new_text == "\n"` 表示替换成 1 个空行
- `expected_old_text` 必须是 JSON string；非字符串直接校验失败，不落盘
- `context_lines < 0` 会被拒绝，`context_lines > 20` cap 到 20
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

## 3.12 `fs.pull_file`

### 作用
把服务器工作区内的文件**以短时效下载 URL 的形式**拉回客户端。

典型场景：客户端需要查看服务器上的任务进度图片（如 `progress.png`），
先调用 `fs.pull_file` 拿到 URL，再 `curl -o` 下载到本地交给 LLM 识别。

### 定位
本工具**不返回文件内容**，只签发下载 URL：

- 通过 `GET /file/<token>` 下载，URL 本身是唯一授权凭据（无二级凭证）
- 相对路径 URL（`/file/<token>`）由客户端用自身请求 MCP 的 base URL 解析；
  配置 `pull_file.url.public_base_url` 后返回绝对 URL
- 与 `fs.read_file` 的关系：read_file 返回 UTF-8 文本内容；pull_file 面向
  二进制/大文件/图片，走 HTTP 流式下载，不经过 JSON-RPC 响应体

### 输入参数
- `path`（必填，`minLength: 1`）：复用 `resolvePathArg`，强制 allowed roots
- `max_bytes`（可选，`minimum: 1`）：本次调用的进一步收紧上限；不能超过配置
  `pull_file.max_bytes`，二者取较小值（无放宽能力）

### 配置项
```yaml
pull_file:
  enabled: true              # 总开关，默认 true
  allowed_extensions: []     # 空 = 不限制；如 [".png",".jpg",".bmp"]；大小写不敏感
  max_bytes: 10485760        # 单文件上限，默认 10 MiB
  url:
    ttl_sec: 300             # URL 有效期，默认 300 秒
    max_downloads: 0         # 0 = 不限制次数；>0 = 次数上限
    public_base_url: ""      # 选填，反代/公网部署时返回绝对 URL
```

### 关键步骤（签发）
1. 解析 `path`（allowed roots / unsafe 语义与现有 fs 工具一致）
2. 校验 `pull_file.enabled`
3. 校验文件存在、非目录、不超过 `min(调用方 max_bytes, 配置 max_bytes)`
4. 校验扩展名白名单（空 = 放行）
5. 用进程内随机密钥对 `version || exp || nonce || path` 做 HMAC-SHA256 签名
6. 记录内存表 `{remaining: max_downloads, exp}`，返回 URL + 元数据

### 关键步骤（下载端点 `GET /file/<token>`）
1. 校验：版本 / HMAC 签名 / 未过期 / 次数未用尽
2. 重新走 allowed roots 校验（TOCTOU 防护）
3. `Stat` 确认文件仍存在、非目录、仍在白名单与大小上限内
4. 按扩展名设置 `Content-Type`（兜底 `application/octet-stream`）
5. `Content-Disposition: inline; filename="<basename>"`
6. `http.ServeContent` 流式发送（自动支持 Range / HEAD）

### 返回结构
```json
{
  "summary": "issued pull link",
  "path": "/abs/progress.png",
  "filename": "progress.png",
  "bytes": 12345,
  "mime_type": "image/png",
  "url": "/file/<token>",
  "url_kind": "relative",
  "host_hint": "127.0.0.1:8080",
  "expires_in_sec": 300,
  "max_downloads": 0
}
```
- `url_kind=absolute` 当且仅当配置了 `public_base_url`
- `host_hint` 来自请求 `Host` 头，仅作参考，不作为 URL 解析依据

### 副作用
- 只读：不写服务端磁盘、不发资源更新通知
- 每次下载写入独立审计事件 `fs.pull_file.download`（含 remote_addr、path、
  成功/失败、耗时、`served N bytes` 摘要），不记录文件内容
- token 内嵌文件路径且签名绑定；持有 URL 即具备该文件的可下载权限

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
7. `git.add` / `git.restore` / `git.diff` 的 `paths` 必须是 repo-relative，不能是绝对路径，也不能用 `..` 越界，不能以 `-` 伪装成选项，也不能以 `:` 使用 Git pathspec magic（例如 `:/`）
8. `git.add` / `git.restore` 的 `paths` 运行时必须非空，schema 同步声明 `minItems: 1`；`git.diff` 的 `paths` 可省略或为空，表示不做路径过滤

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

仅允许显式 repo-relative paths；拒绝绝对路径、`..` 越界、选项形式路径和以 `:` 开头的 Git pathspec magic。

## 4.4 `git.log`
固定模式：

```text
git log --oneline -<limit>
```

`limit` 省略时默认 20；显式提供时必须是正整数，`limit > 200` cap 到 200。

## 4.5 `git.add`
仅允许显式 repo-relative paths；拒绝绝对路径、`..` 越界、选项形式路径和以 `:` 开头的 Git pathspec magic。
`paths` 是必填非空数组，schema 声明 `minItems: 1`。

## 4.6 `git.restore`
仅允许显式 repo-relative paths；拒绝绝对路径、`..` 越界、选项形式路径和以 `:` 开头的 Git pathspec magic。
`paths` 是必填非空数组，schema 声明 `minItems: 1`。

## 4.7 `git.commit`
要求：

- `message` 非空
- schema 声明 `message.minLength: 1`

执行：

```text
git commit -m <message>
```

## 4.8 `git.branch`
只读列分支。

## 4.9 `git.switch`
要求：

- branch 非空
- schema 声明 `branch.minLength: 1`
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
1. 若显式提供 `command`，先校验其为 JSON string 且非空；`unsafe_allow_all=false` 时直接拒绝 raw command，不回退到 preset
2. 未走 raw command 时读取并校验非空 JSON string `preset`
3. 从配置中查找 `ExecPresets[preset]`
4. 若不存在，直接拒绝
5. `workdir` 必须是 JSON string 且非空
6. 若相对路径，则相对于 `StartupDirectory`
7. 要求 `workdir` 落在 allowed roots 内
8. 校验 `args`
9. 合成 argv：
   - `FixedArgs`
   - 用户额外参数
   - 若无 target，则为部分 preset 自动补 `./...`
10. `timeout_override_sec` 若提供必须是正整数；只允许**缩短**，不允许放大超过 preset 默认超时
11. `exec.CommandContext`
12. stdout/stderr 截断后写审计

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
- `command_timeout_sec` / `MCP_COMMAND_TIMEOUT_SEC` 会刷新未显式设置 timeout 的 preset/template 默认超时；每个 preset 仍可用自己的 `timeout_sec` 覆盖
- `command` / `workdir` schema 声明 `minLength: 1`，以表达运行时非空字符串要求；`preset` 因与 raw command 分支互斥，仍由运行时做非空校验
- `preset` / `command` 调用参数若显式提供必须是 JSON string，类型错误不会静默切换执行分支
- preset 模式下 `args` 必须是字符串数组；非数组或空字符串成员会被拒绝
- `unsafe_allow_all=true` 的 raw command 模式下，`args` 仍必须是字符串数组，但允许空字符串成员以保留真实 argv
- `timeout_override_sec` schema 声明 `minimum: 1`，且调用层仍会拒绝类型错误/小数/非正数/越界
- raw/env 与 preset env 的 key 必须非空且不能包含 `=`
- YAML preset `command` 中的裸命令名按 `PATH` 查找；带路径分隔符的相对命令路径按配置文件目录解析
- YAML preset `FixedArgs` / `AllowedArgs` 可配置为空列表以表达“不追加固定参数”或“不允许额外 flag”，但列表项本身不能为空或纯空白
- 以 `-` 开头的参数必须匹配 `AllowedArgs`
- 非 flag 参数默认必须是 workdir 内的本地 target，并会在真实执行路径中解析符号链接后校验仍位于 workdir 内
- 仅 `go_get` 第一版允许受控模块参数（如 `example.com/mod@v1.2.3`）
- 非 `go_get` preset 会拒绝 `github.com/org/mod` 这类模块/导入路径形式的位置参数
- 裸相对 target 会在执行前规范化为 `./...` / `./pkg` 形式，避免 `net/http`、`std`、`...` 被 Go 当作工作目录外的导入路径或全局模式
- `go_test` / `go_generate` / `go_build` / `go_vet` 在没有显式 target 时会自动补 `./...`
- `go_mod_download` 会直接在 `workdir` 中执行 `go mod download`
- `go_mod_tidy` 不会自动补 target，而是直接在 `workdir` 中执行 `go mod tidy`
- `go_work_sync` 不会自动补 target，而是直接在 `workdir` 中执行 `go work sync`
- `timeout_override_sec` 只接受正整数，且只允许缩短默认超时
- 带值 Go flags（如 `-run` / `-skip` / `-tags` / `-go` / `-compat`）支持 `-flag value` 和 `-flag=value`，且 flag 值不会计作显式 target
- 禁止绝对路径、URL、`..` 越界路径，以及通过符号链接解析到 workdir 外的本地 target
- 对 `-o=...`、`-o ...`、`-coverprofile=...`、`-coverprofile ...` 这类输出路径参数也会解析符号链接后再次校验，不能借路径参数写到 workdir 外
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
- `confirm`（模板要求确认时必填 true）

### 关键步骤
1. 读取 `template`
2. `template` 与 `workdir` 必须是 JSON string 且非空
3. 从配置中查找 `CommandTemplates[template]`
4. 若不存在则拒绝
5. 解析 `workdir`：常规模式要求其落在 allowed roots 内；`unsafe_allow_all=true` 时允许任意已存在目录
6. 若模板配置了固定 `env`，则附加到子进程环境中
7. 若模板配置了 `allowed_workdirs`，则 `workdir` 还必须命中这些范围之一；该限制不会被 `unsafe_allow_all=true` 绕过
8. 若提供 `confirm`，必须是 JSON boolean；非布尔值直接校验失败，不执行模板
9. 若模板 `requires_confirmation=true`，则调用方必须显式传 `confirm=true` 才允许执行；该错误会带 `confirmation_required=true` 供 `/debug/statez` 精确统计 `confirmation_blocked`
10. 模板风险语义（`category` / `destructive` / `requires_confirmation`）会随 schema、result、statez 一起暴露
11. 模板提供固定 argv，客户端不再额外传 args
12. `timeout_override_sec` 若提供必须是正整数，且只允许缩短
13. 执行并审计

### 边界说明
- 模板 `Command` 不能为空，所有 argv 片段都不能为空
- YAML 模板 `command` 的首个 argv：裸命令名按 `PATH` 查找，带路径分隔符的相对命令路径按配置文件目录解析；后续 argv 不做路径解析
- YAML 配置里的模板 `allowed_workdirs` 会在加载时按配置文件目录解析成规范化绝对路径
- 模板 env key 必须非空且不能包含 `=`
- `template` / `workdir` schema 声明 `minLength: 1`，以表达运行时非空字符串要求
- `timeout_override_sec` schema 声明 `minimum: 1`，且只会缩短模板默认 timeout
- async SSE stream 投递路径与普通 JSON 路径都会计入模板调用指标

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
- 所有 Go 导航 `path` 必须是非空 JSON string；显式非字符串会在 package load 前返回 validation error
- `go.find_definition.line` / `column` schema 声明 `minimum: 1`；调用层仍会重复校验正整数与位置越界
- `go.find_definition` 若跳到 `allowed_roots` 外，只返回位置，不放开读取边界

### 5.2.2 `go.list_symbols`

#### 作用
返回单个 Go 文件中的顶层结构索引，供 agent 做 outline。

#### 输入参数
- `path`

#### 关键步骤
1. 校验并解析 `path`
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
- `path` 必须是非空 JSON string；非字符串直接校验失败
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
1. 校验并解析 `path`
2. 校验 `line` / `column` 是整数且为正数
3. 用 `go/packages` 加载所属 package，并要求拿到 type info
4. 把 `line + column` 转成 `token.Pos`
5. 在目标文件 AST 中找到对应 identifier
6. 优先处理 selector / method selection
7. 用 `types.Info` 取 `Uses` / `Defs` / `Selections`
8. 只接受第一版支持的对象范围：
   - package-level `func`
   - `method`
   - package-level `type`
   - package-level `var`
   - package-level `const`
   - imported package symbols
9. 返回定义位置 + 最小符号摘要

#### 边界说明
- `path` 必须是非空 JSON string；非字符串直接校验失败
- `line` / `column` 必须是 JSON integer；类型错误或小数返回 `validation_failed`，非正数或越界位置返回 `position_out_of_bounds`
- 目标位置必须落在 identifier 的半开区间 `[Pos, End)` 内；紧跟在标识符后的 `(`、`.` 或空白不再被误判为该 identifier
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

`params.name` 必须是非空 JSON string；显式非字符串会在 registry lookup / 工具执行前按 `-32602` invalid params 拒绝，而不是被静默当作缺失名称。

`params.arguments` 可省略或为 `null`，此时按空对象传给工具；一旦显式提供，必须是 JSON object。array / string / boolean 等非 object 会在工具执行前按 `-32602` invalid params 拒绝，避免错误参数被静默降级成 `{}` 后触发默认工具行为。

执行成功后：
- 返回 `mcp.Result`
- 记录审计
- 若属于文件修改型工具，则进一步触发 `resources/updated`

### `prompts/get`
Prompt 参数入口与工具调用保持同样的 object 容器语义：

- `params.name` 必须是非空 JSON string；显式非字符串会在 prompt lookup 前按 `-32602` invalid params 拒绝
- `params.arguments` 可省略或为 `null`，此时按空对象传给 prompt
- 一旦显式提供，必须是 JSON object
- array / string / boolean 等非 object 会在 prompt schema 字段校验前按 `-32602` invalid params 拒绝，避免错误参数被静默降级成 `{}` 后报告误导性的缺失字段
- `prompts/list` 暴露的 `_meta.inputSchema` 会给运行时要求非空的必填字符串参数声明 `minLength: 1`：`safe_file_edit.task` / `path` 与 `go_dev_loop.goal` / `workdir`

### `completion/complete`
Completion 参数入口先校验对象容器，再校验字段：

- `params.ref` 与 `params.argument` 必须显式提供且必须是 JSON object
- 可选 `params.context` 省略或 `null` 时按空对象处理；显式提供时必须是 JSON object
- 可选 `params.context.arguments` 省略或 `null` 时按空对象处理；显式提供时必须是 JSON object
- `ref.type`、对应的 `ref.name` / `ref.uri`、`argument.name` 与 `argument.value` 必须是 JSON string；`argument.value` 允许空字符串作为空前缀补全
- `go_dev_loop.test_target` 补全会读取 `context.arguments.workdir` 作为目录基准；该字段省略或为 `null` 表示未提供，显式非 `null` 时必须是 JSON string
- `safe_file_edit.path` 与资源模板 `path` 等路径类补全按字面 prefix 过滤，不会 trim 空格或制表符

### SSE / Streamable HTTP 注意点
当前兼容策略：

- `POST /mcp` 请求正文必须解码为**单个** JSON-RPC 对象；第一个对象后的第二个 JSON 值或尾随垃圾统一按 `-32700` parse error 拒绝
- 顶层 JSON array（batch）或 scalar 是合法 JSON 但不是本服务支持的单对象请求，统一按 `-32600` invalid request 拒绝，响应使用 `id:null`
- JSON-RPC `jsonrpc` 字段必须提供且必须是 string `"2.0"`；缺失、非 string 或其他版本统一按 `-32600` invalid request 拒绝，并保留请求中可解析出的 `id`
- JSON-RPC `method` 必须是 string 且非空；非 string、缺失或空字符串统一按 `-32600` invalid request 拒绝，并保留请求中可解析出的 `id`
- JSON-RPC `params` 在本服务中只支持省略、`null` 或 object；array / string / boolean 等统一按 `-32602` invalid params 拒绝，并保留请求中可解析出的 `id`；object 内数字使用 `json.Number` 保留原始 token，整数工具参数会接受数值上等于整数的 JSON number（如 `42.0`、`1e3`），但不能先经 `float64` 舍入
- 非 `notifications/*` 方法必须提供 JSON-RPC `id`；缺失统一按 `-32600` invalid request 拒绝，响应使用 `id:null`
- JSON-RPC `id` 只接受 string、number 或 null；number id 会按原始 JSON number 精确保留，不能经由 `float64` 造成大整数舍入；object / array / boolean 会按 `-32600` invalid request 拒绝，响应使用 `id:null`
- JSON 与单次 SSE 的 `initialize.params.protocolVersion` 必须是非空 string；缺失或非 string 按 `-32602` invalid params 拒绝，合法但不在支持列表中的版本按 `-32002` unsupported protocol version 拒绝
- `initialize.params.clientInfo` 可省略或为 `null`，按空对象记录；一旦显式提供，必须是 JSON object，非 object 按 `-32602` invalid params 拒绝且不会创建 session
- JSON-RPC response 总是显式包含 `id` 字段；无法从请求中取得 id 的错误响应使用 `id:null`
- mixed `Accept: application/json, text/event-stream` 时优先 JSON，但 `q=0` 的 media range 视为不可接受
- 纯 SSE 请求才返回单次 SSE
- 长连接 stream 里所有 JSON-RPC 消息统一用：

```text
event: message
```

这样更兼容官方 Streamable HTTP 客户端。

内部实现约束：

- `streamHub` 在 publish / publishNotification 时保持读锁遍历当前 stream 集合，避免和 `unregister` / `closeSession` 并发时触发 `send on closed channel`
- 每个 stream 当前使用可配置的 queue；默认 **128** 条 envelope 缓冲，减少 burst 异步请求下退化成单次 SSE 的概率
- async POST 的单次 SSE fallback 必须复用本次已构造的 JSON-RPC response，不能重新进入 handler 执行工具，否则队列满时可能重复触发文件写入等副作用
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
   - 也不要把 `q=0` 的 `application/json` 当成可接受 JSON，否则客户端明确禁用 JSON 时仍会收到 JSON

3. **resource update 通知传播**
   - 不仅是具体文件，还包括父目录 URI

4. **git/exec 白名单**
   - 新增能力时不要引入任意命令透传

5. **notification 语义**
   - `notifications/*` 当前是 `202 + empty body`
   - 改动前必须确认与现有客户端兼容性
