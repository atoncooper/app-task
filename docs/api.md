# Service API 手册（/tasks、/scripts、/internal）

> 本文档是服务面 HTTP API 的**单一事实来源**。任何改动 `/api/*` 之外服务面
> 路由、鉴权、请求/响应结构的 PR **必须同步更新本文档**（AGENTS.md PR 约定）。
> HTTP 分层架构（dto/auth/middleware/router）见 [design.md §2.1](design.md)。
> 控制台管理面 `/api/*`（webui 会话/主令牌鉴权，见全部数据）不在本文范围内。

## 1. 概览与部署拓扑

app-task 对外暴露两类 HTTP 面（同一端口、同一进程）：

| 面 | 路径前缀 | 鉴权 | 消费者 |
|----|---------|------|--------|
| 服务面 | `/tasks`、`/scripts*`、`/internal/*` | API key 中间件 | 第三方执行器、调度调用方 |
| 管理面 | `/console/*`、`/api/*` | webui 会话/主令牌 | 管理控制台、`at` CLI |

两种典型接入拓扑：

1. **APISIX 网关**（推荐生产）：网关做 key-auth 挡第一跳，并把终端用户
   uid 注入 `X-Uid` 头。app-task 自身的 key-auth 中间件仍是端口上的权威——
   网关被绕过时直连依旧要带有效 key。
2. **直连**：调用方直接访问 app-task 端口（默认 8001；TLS 开启时为 HTTPS，
   自签证书见控制台证书轮换）。必须带有效 API key。

传输安全：服务端 TLS 由 `server.tls` 配置（certgen 自签 + 自动轮换）；
生产建议网关终止 TLS 或给 app-task 挂真证书。所有服务面响应带
`Cache-Control: no-store`。

## 2. 认证

### 2.1 凭证头（三选一，等价）

```
X-API-Key: <key>
apikey: <key>
Authorization: Bearer <key>
```

key 在控制台「API 密钥」页生成（明文仅显示一次，服务端只存 SHA-256 摘要），
或由 `security.service_keys` 在启动时种入（bootstrap key，如 APISIX
consumer key）。密钥可吊销（立即 401）、可设过期时间。

### 2.2 scope 矩阵

每把 key 携带 scope CSV，中间件按路径组强制（403）：

| 路径组 | scope |
|--------|-------|
| `/tasks`（注册/详情/列表） | `tasks` |
| `/scripts`、`/scripts/logs` | `scripts` |
| `/internal/*`（回调/邮件/脚本运行） | `internal` |

空 CSV = 全部组（仅旧版/bootstrap 行）。scope 之外每把 key 还有
`rate_per_min` 每分钟请求上限（0 = 不限），超出返回 429 + `Retry-After: 60`。

### 2.3 uid 绑定（所有权模型）

任务按 `uid` 归属。两种信任模式：

- **绑定 key**（推荐第三方直连场景）：key 上绑定了 `uid > 0` 时，所有
  请求的 uid 一律以 key 为准——注册请求体里的 `uid` 不匹配直接 403，
  详情/列表忽略 `X-Uid` 头。持有 key 即只能看到/操作自己的任务。
- **未绑定 key**（网关注入模式，bootstrap key 默认如此）：信任调用方
  （网关）注入的 `X-Uid` 头；缺失/非法返回 401。**该模式下直连方拿到
  key 即可访问任意 uid 的数据**——只应发给完全信任的基础设施组件。

### 2.4 失败节流

鉴权失败按客户端 IP 计（与控制台登录同一节流器）：连续失败触发
429 + `Retry-After: 60`。服务端 `SetTrustedProxies(nil)`，伪造
`X-Forwarded-For` 无法绕过。

## 3. 通用约定

- 请求/响应均为 JSON（`Content-Type: application/json`）。
- 时间格式 **RFC3339 / ISO8601**（如 `2030-01-01T09:00:00+08:00`；
  `Z` 后缀接受）。DSN 侧 `loc=UTC`，落库统一 UTC。
- **payload 不透明**：调度器从不解释 `payload`，原样透传给执行器
  （上限 64 KB）。
- 幂等：执行器副作用 at-least-once——调度器派发 HTTP 任务时带
  `X-Task-Id` 头，第三方执行器应按它去重（超时回收/死节点接管会重放）。

### 3.1 错误模型

所有服务面错误统一信封（`detail` 为兼容字段，与旧版一致；新代码请按
`error.code` 分支）：

```json
{
  "detail": "invalid trigger_time (expect ISO8601)",
  "error": {"code": "invalid_trigger_time", "message": "invalid trigger_time (expect ISO8601)"}
}
```

| code | HTTP | 语义 |
|------|------|------|
| `unauthorized` | 401 | 缺 key / key 无效 / X-Uid 缺失或非法 |
| `forbidden` | 403 | scope 不足 / uid 与绑定 key 不符 / 非任务所有者 |
| `rate_limited` | 429 | 失败节流或每 key 限流（带 `Retry-After: 60`） |
| `payload_too_large` | 400 | 请求体超限 |
| `invalid_request` | 400 | JSON 绑定失败 / 缺必填字段 / 分页参数非法 |
| `invalid_trigger_time` | 400 | trigger_time 非 ISO8601 |
| `invalid_executor_url` | 400 | executor_url 协议/主机校验未过（见 §4.1） |
| `register_failed` | 400 | 调度层校验失败（cron 非法、shard_total 越界等） |
| `task_not_found` | 404 | 任务/回调目标不存在 |
| `script_not_found` | 404 | 脚本不存在 |
| `script_rejected` | 400 | 脚本上传被编译门/校验拒绝 |
| `enqueue_failed` | 400 | 邮件入队校验失败 |
| `internal_error` | 500 | 服务端内部错误 |

## 4. 端点

### 4.1 任务注册 — `POST /tasks/register`（scope: tasks）

**201 Created**，响应头 `Location: /tasks/{task_id}`：

```json
{"task_id": "uuid", "status": "pending"}
```

请求体：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `uid` | int | ✓ | 所有者 uid（绑定 key 时必须与之一致，否则 403） |
| `task_type` | string | | `http`（默认）/ `lua` / `notify` |
| `payload` | object | | 不透明参数，原样透传（≤64 KB） |
| `executor_url` | string | | http 型任务的第三方执行器地址，**必须是绝对 http/https URL**；`security.executor_block_private_hosts=true` 时拒绝回环/私网/链路本地目标（注册时 DNS 解析、fail-closed） |
| `async` | bool | | true = 执行器受理 202 后经 `/internal/task/{id}/complete` 回执终态 |
| `cron_expr` | string | | 5 段 cron；空 = 一次性任务 |
| `trigger_time` | string | 条件 | cron 为空时必填（首次触发时间） |
| `max_retry` | int | | 失败重试上限（默认 0） |
| `weight` | int | | 调度权重（默认 1） |
| `shard` | bool | | 分片广播模式（与 `async` 互斥，400） |
| `shard_total` | int | | 分片数，0 = 按存活节点数（≤1000） |
| `calendar_id` | string | | 业务日历名，cron 物化时生效 |
| `alert_channel` | string | | 最终失败告警的 notify 渠道名（需先建） |

```bash
curl -X POST http://127.0.0.1:8001/tasks/register \
  -H "X-API-Key: at_xxx" -H "Content-Type: application/json" \
  -d '{"uid":1,"task_type":"http","executor_url":"http://executor/api/job",
       "cron_expr":"0 9 * * *","max_retry":3,"payload":{"day":"mon"}}'
```

> **威胁模型注记**：注册 http 任务即授权 app-task 从服务端网络向
> `executor_url` 发起请求。`executor_block_private_hosts` 缓解对内网的
> SSRF 探测，但不覆盖 DNS 重绑定（注册时解析 ≠ 派发时解析）——强隔离
> 需求请把执行器放独立网段，或用出网代理白名单。

### 4.2 任务详情 — `GET /tasks/{task_id}`（scope: tasks）

**200**：任务全字段 + 最近 10 条执行日志（trigger_at/executor/status/
duration_ms/response/error）。非所有者 403，不存在 404。

### 4.3 任务列表 — `GET /tasks`（scope: tasks）

**200**：

```json
{"tasks": [{"task_id": "...", "task_type": "http", "status": "pending",
            "trigger_time": "...", "executor_url": "...", "async": false,
            "payload": {}}],
 "total": 42, "limit": 50, "offset": 0}
```

| 参数 | 默认 | 约束 |
|------|------|------|
| `limit` | 50 | 1–200，越界 400 |
| `offset` | 0 | ≥0 |
| `status` | 全部 | `pending` / `dispatching` / `running` / `completed` / `failed` 之一，否则 400 |

uid 来源见 §2.3（绑定 key 或 `X-Uid` 头）。

### 4.4 异步回执 — `POST /internal/task/{task_id}/complete`（scope: internal）

第三方异步执行器报告终态（`running` → `completed` | `failed`）：

```json
{"status": "completed", "result": "{\"ok\":true}", "error": ""}
```

`result` 为任意字符串（通常回传执行结果的 JSON 文本）。fencing：任务已被
回收/重派时回执按孤儿结果丢弃。**200** `{"task_id": "...", "status": "..."}`。

### 4.5 邮件投递 — `POST /internal/email/send`（scope: internal）

平台邮件能力：入队（≤256 KB）后由 worker 重试投递。

```json
{"to": ["a@x.com"], "cc": ["c@x.com"], "subject": "...",
 "html": "<div>...</div>", "reference_id": "task-xxx"}
```

`to`/`subject`/`html` 必填（subject ≤255，reference_id ≤64）。**200**
`{"email_id": "...", "status": "queued"}`。

### 4.6 脚本按需运行 — `POST /internal/script/run`（scope: internal）

同步运行已注册 Lua 脚本（与定时派发同一管线，落 script_run 留痕）：

```json
{"script_id": "...", "payload": {}}
```

**200** 返回 `run_id`/`script_id`/`version`/`status`/`error`/结果 JSON；
脚本不存在 **404**。

### 4.7 脚本管理（scope: scripts）

- `POST /scripts` — 上传即新版本（立即生效），过编译门
  （`executor.CompileForValidation`），拒绝则 400 `script_rejected`。请求体：
  `script_id`（可选，省略则服务端分配 UUID；携带即走更新/upsert 流程）、
  `name`（必填 ≤128）、`description`（≤512）、`source`（必填 Lua 源码）、
  `enabled`（可选布尔，缺省 true）、`operator`（可选 ≤64，缺省回退
  `X-Operator` 头 → 认证身份 → API key 名）。每次变更写审计日志
  （operator/source IP/request id/version/action）。
- `GET /scripts` — 全部脚本最新版本列表。
- `GET /scripts/logs?script_id=...&limit=50` — 单脚本变更审计
  （operator/source IP/version/action），limit 1–200。

## 5. 变更纪律

- 改服务面路由/鉴权/字段：**同一 PR 内更新本文档** + 同步 CLI（若涉及其
  使用的 `/api/*` 管理面则另见控制台文档）+ 补 router 层测试；
- 新增自由文本字段必须过字符集校验并核对流向（日志/HTML/URL/owner）；
- 状态变更类端点必须条件更新 + fencing（AGENTS.md §四）。
