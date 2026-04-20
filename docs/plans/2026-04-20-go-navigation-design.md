# Go 导航能力设计方案（P0）

日期：2026-04-20  
状态：validated design  
目标：为 `mcp-tools` 增加第一版 Go-only 符号/导航能力，优先补齐 coding agent 的“读懂项目与定位修改点”能力。

---

## 1. 目标与背景

当前 `mcp-tools` 已具备：

- `fs.read_file` / `fs.search_text` / `fs.replace_text` / `fs.edit_lines`
- `fs.apply_unified_diff` 作为复杂修改原语
- `exec.run` / `exec.run_template`
- 比较完整的 MCP JSON / SSE / Streamable HTTP 接入能力

但它仍明显缺少 ClaudeCode / OpenCode 风格 coding agent 体验中的关键一层：

> agent 不只是“能读文件、能改文件”，还应该能**快速知道文件里有什么、光标指到的符号是谁、定义在哪里**。

如果没有这层能力，agent 的典型 workflow 仍然是：

- 读整文件
- 用 grep / search 反复猜位置
- 再修改

这会导致：

- 定位成本高
- 理解质量不稳定
- 复杂项目里很难形成高成功率闭环

因此，P0 第一优先级不是继续堆更多执行命令，也不是立刻放开 `exec.run`，而是优先补齐：

- 符号列表（outline）
- 定义跳转（definition）

---

## 2. 第一版范围

### 2.1 语言范围

第一版明确：

- **Go-only**

原因：

- 当前仓库自身就是 Go 项目
- 现有 `exec.run` preset 也明显偏 Go workflow
- Go-only 能最快形成“理解 → 修改 → 验证”的完整飞轮

第一版不做：

- 多语言统一接口
- tree-sitter / LSP 风格通用导航层
- `gopls` daemon 依赖

### 2.2 第一批工具

第一版新增两个工具：

- `go.list_symbols`
- `go.find_definition`

### 2.3 第一版非目标

明确不做：

- `references`
- workspace-wide symbols
- 按 symbol 名直接查询 definition
- 局部变量 / label 等更细粒度目标
- 多语言扩展
- 外部定义文件的直接读取放开

---

## 3. 对外工具形态

### 3.1 `go.list_symbols`

#### 输入

- `path`：目标 Go 文件路径

#### 输出

返回单文件 symbols 结构，建议至少包括：

- `path`
- `package`
- `symbols`

每个 symbol 最少包括：

- `name`
- `kind`
  - `func`
  - `method`
  - `type`
  - `var`
  - `const`
- `receiver`（仅 method）
- `exported`
- `start_line`
- `start_column`
- `end_line`
- `end_column`

#### 第一版范围限制

- **只按单文件 `path` 工作**
- 不做 package-wide 聚合
- 不做 workspace-wide symbols

这意味着它的定位是：

> 把单个 Go 文件抽象成 agent 可消费的结构索引。

### 3.2 `go.find_definition`

#### 输入

- `path`
- `line`
- `column`

即：

- 基于具体文件中的具体光标位置定位目标符号

#### 输出

建议至少包括：

- `query_path`
- `query_line`
- `query_column`
- `symbol_name`
- `symbol_kind`
- `package`
- `definition_path`
- `definition_line`
- `definition_column`
- `in_allowed_roots`
- 可选：`signature_summary`

#### 第一版目标范围

仅覆盖：

- package-level declarations
  - funcs
  - methods
  - types
  - vars
  - consts
- imported package symbols

不覆盖：

- 局部变量
- label
- 更细粒度本地作用域目标

---

## 4. 外部定义边界

`go.find_definition` 第一版允许遇到两种情况：

### 4.1 定义在 `allowed_roots` 内

返回：

- 定义位置
- `in_allowed_roots=true`

后续 agent 可以继续：

- `fs.read_file`
- `fs.apply_unified_diff`
- 其它工作流动作

### 4.2 定义在 `allowed_roots` 外

例如：

- 标准库
- module cache
- 外部依赖源码目录

第一版策略：

- **允许返回定义位置**
- 但返回：
  - `in_allowed_roots=false`

这意味着：

- agent 至少知道“定义是谁、定义在哪”
- 但不意味着自动放开外部文件读取边界

这样做的好处是：

- 体验上不像残废导航
- 安全边界又没有直接炸开

---

## 5. 内部实现架构

第一版建议新增一个 Go 分析层，例如：

- `internal/tools/golangx/`
- 或 `internal/analysis/goindex/`

### 5.1 `go/packages` 的职责

负责“加载真实 Go 世界”：

- 文件所属 package
- module / workspace / build tags 语义
- import graph 基本解析
- type/object 归属
- definition 跳转时的对象定位基础

也就是说：

> 和“这个标识符在 Go 语义上到底是谁”相关的事，交给 `go/packages`。

### 5.2 `go/ast` 的职责

负责“把结构抽出来”：

- 单文件 declarations 遍历
- symbol kind 提取
- 方法接收者提取
- 位置范围提取
- outline 结果整理

也就是说：

> 和“把单文件结构变成 agent 好消费的数据”相关的事，交给 `go/ast`。

### 5.3 推荐流程

#### `go.list_symbols`
1. 解析 `path`
2. 校验在 `allowed_roots` 内
3. 用 `go/packages` 加载所属 package
4. 锁定目标文件 AST
5. 遍历 declarations，抽取 symbols
6. 返回结构化 outline

#### `go.find_definition`
1. 解析 `path`
2. 校验在 `allowed_roots` 内
3. 用 `go/packages` 加载所属 package 与类型信息
4. 将 `line + column` 转为 token.Position / token.Pos
5. 定位 identifier
6. 从对象信息拿 definition 位置
7. 组装最小符号摘要
8. 判断 definition 是否仍在 `allowed_roots` 内

---

## 6. 错误模型

第一版两个工具都应该支持**结构化失败**。

### 6.1 `go.list_symbols` 常见失败原因

- `not_go_file`
- `package_load_failed`
- `parse_failed`
- `path_outside_allowed_roots`

失败返回至少包括：

- `reason`
- `path`
- `message`

### 6.2 `go.find_definition` 常见失败原因

- `not_go_file`
- `package_load_failed`
- `identifier_not_found`
- `definition_not_resolved`
- `position_out_of_bounds`
- `path_outside_allowed_roots`

失败返回至少包括：

- `reason`
- `query_path`
- `query_line`
- `query_column`
- `message`

### 6.3 Agent 恢复语义

- `position_out_of_bounds`
  - 重新读文件并重算坐标
- `identifier_not_found`
  - 说明当前光标没指到可定义符号
- `package_load_failed`
  - 说明项目 Go 语义加载本身有问题，应先处理依赖或构建环境
- `definition_not_resolved`
  - 说明目标暂时不在第一版支持范围内

---

## 7. 测试矩阵

### 7.1 `go.list_symbols`

至少覆盖：

- 单文件列出 func / type / method / var / const
- method receiver 正确
- exported 标记正确
- 非 Go 文件拒绝
- 路径不在 `allowed_roots` 内拒绝

### 7.2 `go.find_definition`

至少覆盖：

- 跳到同文件定义
- 跳到同 package 其他文件定义
- 跳到 import 的外部定义，并返回 `in_allowed_roots=false`
- 光标越界报错
- 点到非符号位置报错
- 非 Go 文件拒绝

### 7.3 第一版质量底线

- 找不到定义时必须结构化失败，不能只返回模糊字符串
- 不允许 silently fallback 到 grep 猜结果
- 结果必须可用于后续：
  - `fs.read_file`
  - `fs.apply_unified_diff`
  - 未来 `references`

---

## 8. 为什么 P0 要先做这个

当前工具集已经有：

- 文件读写
- 精确文本替换
- patch 修改
- Go 命令执行

但在 coding agent 工作流里，真正缺的一层是：

> **知道“我现在看的这个文件有什么”，以及“我指到的这个东西是谁”。**

如果先补这层，agent 的 workflow 会从：

- 读整文件 → 猜位置 → 改

升级成：

- 看 symbols → 找 definition → 再读关键文件 → 再改

这会直接提高：

- 理解速度
- 修改定位准确率
- 复杂任务成功率

也因此，它应当排在“更开放的 exec.run 解限”之前。

---

## 9. P1（后续）预告：受控 exec.run 解限

当前已经明确：

- `exec.run` 解限有价值
- 但应作为**第二优先级**
- 且不能直接开放成任意 shell

后续设计重点包括：

- 是扩展现有 `exec.run`，还是新增受控 command 模式
- 如何做路径内写 / 外部缓存写 / 其它外部写 的影响分层
- 如何做 argv / cwd / env / policy 命中的完整审计

这部分不在当前 P0 导航能力第一版范围内。

---

## 10. 推荐下一步

下一步应从 validated design 切到 implementation planning，并按以下顺序实现：

### Phase A
- 建立 Go 分析层（`go/packages` + `go/ast`）

### Phase B
- 实现 `go.list_symbols`
- 先把单文件 outline 做稳

### Phase C
- 实现 `go.find_definition`
- 覆盖同文件 / 同 package / 外部定义定位

### Phase D
- 补结构化错误返回
- 补 README / AGENTS / TOOLS-DEFINE / tests

