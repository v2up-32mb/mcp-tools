# exec.run_template Confirmation Gating Plan

## 背景
`exec.run_template` 已经具备风险语义字段，但当前这些字段只用于展示和观测，不会影响实际执行。这样会导致 `requires_confirmation` 只有说明价值，没有执行约束。

## 目标
新增模板执行门禁：

- 如果模板 `requires_confirmation=true`
- 且客户端未显式传 `confirm=true`
- 服务端拒绝执行，并返回结构化错误结果

同时将阻断行为写入 `/debug/statez` 计数。

## 非目标
- 不做复杂的 pending approval / token / callback 流
- 不改 transport 协议
- 不改变现有非高风险模板的执行路径

## 设计
### 新参数
`exec.run_template` 增加：
- `confirm`（boolean, optional）

### 默认策略
- `go_clean_testcache`
  - `requires_confirmation: true`

### 观测
`template_metrics` 新增：
- `confirmation_blocked`
- `confirmation_blocked_per_template`

## 验证标准
- 未确认时高风险模板拒绝执行
- `confirm=true` 时允许执行
- statez 出现 confirmation-blocked 计数
- 全量测试通过
