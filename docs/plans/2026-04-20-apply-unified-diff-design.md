# fs.apply_unified_diff 设计方案

日期：2026-04-20  
状态：validated design  
目标：补齐复杂修改原语，让 agent 在复杂代码编辑场景下优先使用 **标准 unified diff patch workflow**，降低 `fs.edit_lines` 在复杂改动中导致文件错乱的概率。

---

## 1. 背景与问题定义

当前 `mcp-tools` 已经具备：

- `fs.edit_lines`：适合小范围、低上下文、按行精修
- `fs.replace_text`：适合精确旧文本替换
- `fs.write_file`：适合整文件重写

但在真实 coding agent 使用场景中，复杂修改往往同时具备以下特点：

- 一个文件内多处改动
- 改动之间存在上下文关联
- 需要同时表达“保留哪些上下文”“删除哪些旧行”“新增哪些新行”
- 失败后希望 agent 能精确知道哪里冲突，而不是盲猜重试

继续让 agent 在这类任务里主要依赖 `fs.edit_lines`，风险很高：

1. 调用方需要自己计算行号区间
2. 多次局部编辑容易让后续区间漂移
3. 复杂修改缺少“旧内容校验 + 上下文校验”
4. 失败时难以自动恢复，容易导致文件被反复改乱

因此下一阶段的核心抓手不是再增加更多执行命令，而是补上一层更稳的复杂修改原语：

- `fs.apply_unified_diff`

---

## 2. 目标与非目标

### 2.1 目标

第一版 `fs.apply_unified_diff` 要做到：

- 接受**标准 unified diff 文本**作为输入
- 仅支持**单文件 patch**
- 支持**多 hunk**
- 严格命中；任一 hunk 不匹配则**整体失败**
- 失败时返回**结构化冲突详情**
- 成功时走现有：
  - allowed roots 校验
  - 原子写入
  - 审计
  - `notifications/resources/updated`

### 2.2 非目标

第一版明确**不做**：

- 多文件 patch
- fuzz / 宽松匹配
- transport 层交互式确认流
- AST/语法树结构化编辑
- 跨文件事务 patch 包

---

## 3. 对外工具 Contract

新增工具：

- `fs.apply_unified_diff`

### 3.1 输入参数

建议第一版 schema：

- `path`：目标文件路径（必填）
- `diff`：标准 unified diff 文本（必填）
- `expected_old_text`：可选，额外 optimistic concurrency 门禁
- `context_lines`：可选，仅用于返回摘要控制，不参与命中
- `dry_run`：可选，只验证 patch 是否可应用，不写盘

### 3.2 行为约束

- `path` 仍单独作为参数传入，继续走既有路径安全校验
- `diff` 中的 `--- / +++` 头必须与 `path` 一致
- 若 `diff` 表达多文件 patch，直接失败
- 所有 hunk 必须严格命中
- 任一 hunk 失败，整体失败，不允许部分应用

### 3.3 成功返回

建议至少返回：

- `applied: true`
- `hunks_applied`
- `bytes_written`
- `content_digest`
- 可选：`affected_line_ranges`

### 3.4 失败返回

建议结构化返回：

- `applied: false`
- `conflict.hunk_index`
- `conflict.reason`
- `conflict.expected_lines`
- `conflict.actual_lines`
- `conflict.target_start_line`

其中 `reason` 第一版最少支持：

- `header_mismatch`
- `context_mismatch`
- `delete_mismatch`
- `path_header_mismatch`
- `multi_file_diff_not_supported`
- `expected_old_text_mismatch`

---

## 4. 内部应用算法

第一版内部算法按如下顺序执行：

### 4.1 输入解析

1. 解析 `path`
2. 路径落入 `allowed_roots`
3. 解析 `diff` 为 unified diff 结构
4. 如果存在：
   - 多文件 patch
   - 非法 `@@` header
   - 缺失必要 diff header
   - header 路径与 `path` 不一致
   直接失败

### 4.2 读取文件快照

1. 读取当前文件内容
2. 按逻辑行切分，保留换行信息
3. 若传入 `expected_old_text`，先做前置校验
4. 若前置校验失败，则返回并发冲突类错误，不进入 hunk 应用

### 4.3 严格命中验证

逐个 hunk 在内存中模拟应用：

- 先根据 hunk header 计算目标行范围
- 逐行比对 context 行
- 逐行比对 delete 行
- add 行不参与旧内容命中，但参与最终结果生成

关键原则：

- 不是边校验边写文件
- 不是 hunk 过一个写一个
- 必须先在内存中证明**所有 hunk 都合法**
- 只要有一个 hunk 不匹配，就返回结构化冲突信息并停止

### 4.4 结果生成与写入

当所有 hunk 均验证通过后：

1. 在内存中拼接新的完整文件内容
2. 若 `dry_run=true`，直接返回可应用结果，不落盘
3. 否则调用既有 `atomicWrite`
4. 成功后触发审计与资源更新通知

---

## 5. 为什么它比 fs.edit_lines 更稳

`fs.edit_lines` 的长处是低上下文、轻量、适合精修；它的短板是：

- 复杂改动时，调用方需要自己维护行号区间
- 多次编辑后容易发生后续区间漂移
- 缺少足够强的上下文校验

`fs.apply_unified_diff` 的优势在于：

- 同时携带上下文、删除内容、新增内容
- 服务端可以判断“你要改的是不是这段真实内容”
- 失败可结构化诊断，而不是只返回模糊字符串
- 更适合 agent 生成 patch、失败后重读文件再重生 patch

本质上，它把“改文件”升级为：

> **先校验你想改的是不是当前文件中的那一段，再允许你改。**

这就是复杂修改稳定性的底层逻辑。

---

## 6. 错误模型与 Agent 恢复策略

`fs.apply_unified_diff` 的价值，不只在成功路径，更在失败路径。

### 6.1 错误模型

失败时不返回单一字符串，而返回结构化冲突对象，让 agent 能决定下一步：

- `path_header_mismatch`
  - patch header 路径与 `path` 不一致
- `multi_file_diff_not_supported`
  - patch 中包含多个文件
- `header_mismatch`
  - hunk header 声明的定位信息与当前文件状态不一致
- `context_mismatch`
  - 上下文行不匹配
- `delete_mismatch`
  - patch 想删除的旧行与当前文件不匹配
- `expected_old_text_mismatch`
  - 并发门禁失败

### 6.2 Agent 推荐恢复动作

- 若是 `path_header_mismatch` / `multi_file_diff_not_supported`
  - 不需要先读文件，直接重生 patch
- 若是 `context_mismatch` / `delete_mismatch` / `header_mismatch`
  - 先 `fs.read_file` 获取最新内容，再重生 patch
- 若是 `expected_old_text_mismatch`
  - 视为并发冲突，先重新理解文件当前状态，不要盲改

这样失败就不再是黑盒，而是下一轮动作的输入。

---

## 7. 测试矩阵

### 7.1 成功路径

- 单文件、单 hunk 成功
- 单文件、多 hunk 成功
- 仅新增行成功
- 仅删除行成功
- 替换行成功
- `dry_run=true` 时通过但不写盘
- 成功后触发 `notifications/resources/updated`

### 7.2 失败路径

- 多文件 diff 直接拒绝
- `--- / +++` header 与 `path` 不一致
- context 行不匹配
- delete 行不匹配
- hunk header 行号不匹配
- `expected_old_text` 校验失败
- 任一 hunk 失败时，文件内容保持不变

### 7.3 边界路径

- 文件尾换行变化
- 空文件 patch
- 只新增、不删除
- 只删除、不新增
- 文件中有空行的 patch
- patch 文本格式合法但语义不合法

---

## 8. 验收标准

若第一阶段达到以下 4 条，就算成功：

1. agent 在复杂修改时开始优先使用 `fs.apply_unified_diff`
2. patch 失败时能依赖结构化冲突自动进入“重读文件 → 重生 patch”
3. 文件不会因为部分 hunk 失败而被写坏
4. README / AGENTS / TOOLS-DEFINE / tool description / tests / audit 都完成同步闭环

---

## 9. 推荐实施顺序

### Phase A: Core Engine
- patch 解析
- 单文件校验
- 严格 hunk 命中
- 内存中模拟应用

### Phase B: Tool Surface
- MCP tool schema
- 结构化返回
- dry_run
- 审计字段

### Phase C: Integration
- `resources/updated`
- `tools/list` description
- README / AGENTS / TOOLS-DEFINE

### Phase D: Test Hardening
- 成功路径测试
- 冲突路径测试
- 不变性测试
- 资源通知测试

---

## 10. 为什么这是接近 ClaudeCode/OpenCode 的第一块关键积木

ClaudeCode/OpenCode 体验好的关键，不只是“会调工具”，而是：

- agent 有更稳定的复杂修改中间表示
- 失败后能快速收敛到下一步
- 修改结果更可审计、更可 review、更可回放

因此，`fs.apply_unified_diff` 不是一个普通增量工具，而是下一阶段 workflow 飞轮里的核心原语：

- 读文件
- 生成 patch
- apply patch
- 失败则结构化恢复
- 成功后再验证 / 执行 / 提交

当这个环节稳住，后面再补：

- 多文件 patch
- 更通用的代码工作流
- 符号级导航
- watcher / 诊断聚合

整个工具集才会真正开始逼近 ClaudeCode/OpenCode 那种“顺手”的体感。
