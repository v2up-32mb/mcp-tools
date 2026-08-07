# fs.pull_file 设计文档

日期：2026-08-07
状态：已确认，进入实现

## 1. 目标

新增只读工具 `fs.pull_file`：客户端调用后，服务端校验目标文件并签发一个

**有时效、带次数限制、token 内嵌文件路径**的下载 URL，客户端自行 GET 下载。

典型场景：客户端需要查看服务器上的任务进度图片（如 `progress.png`），下载到本地后交给 LLM 识别。

## 2. 关键架构决策

- MCP 是 Client–Server 模型，server 无法写 client 本地磁盘。本工具**不返回文件内容**，
  只返回下载 URL，由 client 用 `curl`/浏览器直接下载保存。
- 传输采用和 S3 presigned URL 一致的模型：**持有 URL 即授权**，无二级凭证。
- 下载端点是服务自带的 HTTP 路由 `GET /file/<token>`，不属于 MCP 工具调用，
  但每次下载都会写入独立审计事件。
- URL 默认返回**相对路径** `/file/<token>`，由 client 用自身请求 MCP 的 base URL 解析
  （RFC 3986 相对解析）。不信任 `Host` 头作为 URL 主体；可配置
  `pull_file.url.public_base_url` 让 server 显式返回绝对 URL（反代/公网部署场景）。

## 3. 调用参数

```jsonc
{
  "path": "相对或绝对路径", // 必填，复用 resolvePathArg，强制 allowed roots
  "max_bytes": 1048576     // 可选，正整数；本次拉取上限，不能超过配置 max_bytes
}
```

被删除的候选参数：`encoding`（与 fs.read_file 重复）、`as_image`（URL 方案下 client
直接访问 URL 即可，无需 image 块）、分块 offset（YAGNI）。

## 4. 配置文件

```yaml
pull_file:
  enabled: true              # 总开关，默认 true
  allowed_extensions: []     # 空 = 不限制；如 [".png",".jpg",".jpeg",".bmp"]；大小写不敏感
  max_bytes: 10485760        # 默认 10 MiB
  url:
    ttl_sec: 300             # 默认 300 秒
    max_downloads: 0         # 默认 0 = 不限制次数；>0 = 次数上限（与 TTL 双条件，先到先失效）
    public_base_url: ""      # 选填；空 = 返回相对路径由 client 拼接
```

校验规则：
- `max_bytes`/`ttl_sec` 必须为正；`max_downloads` 非负。
- `allowed_extensions` 每项必须以 `.` 开头、不含 `/`、`\`、空白；统一转为小写。
- `public_base_url` 若配置必须是合法 `http(s)://` 绝对 URL，不允许 query/fragment，去尾部 `/`。

## 4. token 与下载端点

### token 结构（签发后 base64url，无明文路径）

```
payload = version(1B) || exp_unix(8B) || nonce(16B) || utf8(path)
sig     = HMAC-SHA256(serverSecret, payload)
token   = base64url(payload || sig)
```

- `serverSecret`：进程启动时随机 32 字节（`crypto/rand`），**不落盘**。
  → 服务重启后所有已签发 URL 天然失效，符合"短时效"语义。
- `exp` 双条件：token 内过期时间 + 内存表剩余下载次数。内存表 key=token，
  保存 `{remaining, exp}`，签发时惰性清扫过期项。
- `max_downloads=0` 表示不限制次数，仅受 TTL 约束。

### 下载时刻再校验（防 TOCTOU）

`Resolve(token)` 依次：
1. 校验版本、HMAC 签名、未过期、次数未用尽（用完即焚语义）。
2. 重新用 `security.ResolvePath(path, allowed_roots)` 校验（`unsafe_allow_all=true` 时放开，与现有工具一致）。
3. `os.Stat` 确认文件仍存在且非目录。
4. 重新校验扩展名白名单与 `max_bytes`（文件可能被替换）。

校验失败对客户端统一返回 403（不泄露原因）；文件不存在返回 404。

### 下载响应

- `Content-Type`：按扩展名标准库 `mime.TypeByExtension`，兜底 `application/octet-stream`。
- `Content-Disposition: inline; filename="<basename>"`（basename 纯文件名）。
- 通过 `http.ServeContent` 流式输出，自动支持 `Range` / `HEAD`。

## 6. 工具返回结构

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

- `public_base_url` 配置后：`url_kind=absolute`，`url=<base>/file/<token>`。
- `host_hint` 仅取请求 `Host` 头作为参考，不做解析依据。

## 7. 审计

- 工具调用本身由 MCP registry 审计（与现有 fs 工具一致）。
- 下载端点额外写独立事件：`tool=fs.pull_file.download`，记录 remote_addr、target_path、
  成功/失败、耗时、result_digest（`served N bytes` 或失败原因摘要），**不记录文件内容**。

## 8. 测试计划

- config：pull_file 默认值覆盖、非法扩展名 / 空 URL / 负 TTL / 负 max_downloads 被拒。
- pullfile 包：签发/解析往返、过期、次数耗尽、签名篡改、不存在的文件、白名单大小写、尺寸上限。
- fs 工具：schema（path 必填 minLength、max_bytes minimum=1）、相对/绝对 URL、禁用、白名单拒绝、超限。
- httpapi：`GET /file/<token>` 往返比对字节、403 无效/过期/消耗、404 文件缺失、HEAD、审计落盘行。

## 9. 文档同步

- `docs/plans/2026-08-07-fs-pull-file-design.md`（本文件）
- `README.md` / `TOOLS-DEFINE.md` / `AGENTS.md` / `mcp-tools.example.yaml` / `CHANGELOG.md`
- `task_plan.md` / `findings.md` / `progress.md`

## 10. 不做的事情（边界）

- 不返回文件内容（不做 base64）、不支持 URL 拉取、不做分块断点服务、不写客户端磁盘。
- client 落盘由调用方完成，服务端只签发下载 URL。
