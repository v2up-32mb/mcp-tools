# exec.run_template Risk Semantics Plan

## 背景
`exec.run_template` 已经具备固定模板、固定 env、allowed_workdirs、statez 模板计数等能力，但还缺模板级风险语义。客户端当前只能知道“能不能执行”，还不知道“执行风险大不大”。

## 目标
为模板增加可消费的风险元信息：

- `category`
- `destructive`
- `requires_confirmation`

并把这些信息同步暴露到：
- 配置层
- `tools/list` 的 description / schema
- `/debug/statez`
- 文档与示例配置

## 非目标
- 本轮不实现真正的 confirmation 交互流
- 本轮不改变 exec.run 的安全边界
- 本轮不开放任意 shell 或客户端自定义 env

## 设计方向
### 配置层
在 `CommandTemplate` 中新增：
- `Category string`
- `Destructive bool`
- `RequiresConfirmation bool`

### discoverability
- 在 `exec.run_template` description 中追加模板风险摘要
- 在 schema 的 `_meta` 或 properties 描述中暴露模板元信息

### observability
`/debug/statez` 的 `config.command_templates` 中新增：
- `category`
- `destructive`
- `requires_confirmation`

## 默认模板建议
- `make_test`
  - `category: test`
  - `destructive: false`
  - `requires_confirmation: false`
- `make_build`
  - `category: build`
  - `destructive: false`
  - `requires_confirmation: false`
- `go_clean_testcache`
  - `category: cleanup`
  - `destructive: true`
  - `requires_confirmation: false`

## 验证标准
- 配置可解析
- `tools/list` 中能看到更明确的风险语义
- `/debug/statez` 返回模板风险元信息
- 全量测试通过
