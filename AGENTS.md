# 🔧 AGENTS.md — app-task（MindBase 任务执行平台）开发规范

> **本文件是本仓库的唯一权威规范文档。** 所有开发者（含 AI 编程助手）在动任何代码前必须完整阅读。
> 使用与部署见 [README.md](README.md)；架构与实现细节见 [docs/design.md](docs/design.md)。

---

## 一、项目简介

app-task 是一个**独立部署的定时任务执行器**：接收任务注册 → DB 轮询调度 →
到点执行（HTTP 透传 / 内置 Lua 沙箱 / 内置 notify 通知）→ 结果留痕 → 失败重试 → 邮件通知。

核心链路：

```
任务注册 → MySQL(自有) → 轮询认领(dispatching) → 并行派发 → HTTP/Lua 执行 → finalize
                                     ↘ 失败重试 / 超时回收 / 邮件通知
```

- **单二进制双角色**：`at`（或 `at serve`）启动服务端；其余子命令是客户端
  （HTTP 操作运行中的实例）；
- **多实例集群**（无主对称）：所有调度状态收敛到自有 MySQL（ACID + 条件更新
  + fencing token），实例不互相通信；执行器副作用 at-least-once，**必须幂等**；
- **独立性**：不依赖主 app 代码；与其他系统的集成全部走 HTTP 与共享环境变量。

## 二、目录结构

```
main.go                    入口薄壳（//go:embed default.yaml，转交 internal/cli）
internal/
  cli/                     at 命令行：serve + 客户端命令（cobra；client.go 双凭据头）
  config/                  配置加载：default.yaml + ${VAR}/${VAR:默认} 占位符 + APPTASK__ env 覆盖
  db/                      MySQL 连接（DSN 组装/池/超时/TLS；db.Options）
  model/                   GORM 模型（schema 所有权归本服务；db.Migrate 自建）
  repo/                    数据访问：任务认领/fencing/邮件队列/脚本/API key/名册
  executor/                执行器：HTTP 透传（executor.go/http.go）+ 内置 Lua 沙箱（lua.go）
  service/                 调度器（scheduler.go）+ 邮件投递 worker + 任务服务
  cluster/                 节点名册：心跳/存活/死节点判定/加入转正
  router/                  Gin：控制台 SSR（web/templates）+ /api/* + API key 中间件
                           + 状态存储（memory/redis：会话/节流/限流/一次性展示）
  certgen/                 控制台 HTTPS 自签证书生成与轮换
  security/                AES-256-GCM（中心密钥库）
  logger/                  slog + GORM logger
bench/                     基准 + 并发/一致性不变量测试（独立于 internal 的黑盒包）
docs/                      cli.md / cluster.md（运维）/ design.md（技术设计）
web/                       控制台模板与静态资源（Bootstrap 3 + CodeMirror 5，go:embed）
default.yaml               默认配置（嵌入二进制；支持 ${VAR} 占位符）
```

## 三、模块调用链（修改前先确认位置）

```
HTTP 请求
  │
  ▼
router/（参数解析 + 鉴权 + 转发）
  │
  ▼
service/（调度器/任务服务/邮件 worker）        repo/（数据访问，唯一写库方）
  │                                              │
  ▼                                              ▼
executor/（HTTP 透传 / Lua 沙箱）           MySQL（自有实例，schema 自建）
```

**方向约束：**

- ✅ `router → service → repo → db`，`service → executor`；
- ✅ 同层允许：`service/scheduler → repo/*`、`router → repo`（仅只读查询）；
- ❌ `repo` 禁止依赖 `service`/`router`（反向调用）；
- ❌ `executor` 禁止访问 `db`/`repo`（执行器只拿 ctx，不碰存储）；
- ❌ 禁止跨层改动（问题在认领就只改认领，不顺手改调度/路由）。

**禁止行为：**

- ❌ 禁止在 `executor`（Lua 沙箱/HTTP）内访问数据库或引入业务概念；
- ❌ 禁止绕过认领（claim）直接派发任务——所有执行都必须先持有 fencing token；
- ❌ 禁止让任何状态转换绕过条件更新（`WHERE status=…`）；
- ❌ 禁止把密钥/连接串写进 YAML 字面量（用 `${VAR}` 占位符）或打进日志。

## 四、核心机制（改动前必须理解的四条不变量）

1. **认领排他**：一个任务只会被一个 worker/实例认领——`ClaimTasksBatch`/
   `ClaimTask` 都是条件更新（`WHERE status='pending'`），数据库原子裁决；
2. **fencing**：每次认领铸造新 `claim_token`；finalize/release 只在 token
   仍是当前认领时生效。陈旧持有者的结果被记
   `orphaned dispatch result discarded` 并丢弃；
3. **执行副作用 at-least-once**：回收（TTL/死节点）意味着执行器可能被触发
   两次——**执行器必须幂等**（HTTP 执行器带 `X-Task-Id` 供去重）；
4. **完成即提交**：finalize/task_log 同步阻塞到 MySQL 确认（持久性由
   InnoDB redo 担保，不自建应用层 WAL）。状态脱离 MySQL 的重构才需要重新
   评估持久化方案。

违反任一条即为破坏一致性——review 会逐条核对。

## 五、开发规范

### Go（全仓）

- 所有函数有类型注解式注释惯例：导出函数必须有 doc comment（英文）；
- 错误处理：`fmt.Errorf("…: %w", err)` 包装；调度器日志统一 `[SCHEDULER]`
  前缀 + 结构化字段（slog）；
- 状态枚举一律常量（如 `repo.StatusDispatching`），禁止散落魔法字符串；
- 新增配置项：`default.yaml` + `internal/config` 结构体 + env 覆盖**三处同步**，
  并写注释说明防什么（如 read_timeout 防 hung DB 冻结 worker）；
- 新增表/列：只改 `internal/model` + `db.Migrate()`——**schema 归本服务所有，
  只 CREATE，不手写 ALTER**（改列类型/删列需在 PR 中声明影响）。

### 控制台（web/）

- 服务端渲染 html/template，**模板结构改动必须数 if/range/end 配对**
  （template.Must 会在启动期 panic）；
- 交互增强用原生 JS（app.js 的 modal/dropdown 约定：`data-modal-open`/
  `data-modal-close`/`data-toggle`），不引 jQuery/前端框架；
- 编辑器/样式沿用 Bootstrap 3 + CodeMirror 5 主题层（app.css 为定制层）；
- 所有用户输入走 html/template 自动转义；新增自由文本字段必须过字符集校验
  （参考 node_id 的 `nodeIDRe`——它流向日志/owner/重定向）。

### 安全（新增端点/字段时的 checklist）

- [ ] 端点挂在正确的门后（登录门 / `requireAdmin()` / API key 中间件）；
- [ ] 自由文本字段的字符集/长度校验（它将流向日志、HTML、URL、owner）；
- [ ] 不回显密钥/连接串（一次性展示除外，且一次性消费）；
- [ ] 数值参数有上下限（防溢出/防撑爆，参考 weight 1–1000）；
- [ ] 变更状态的操作必须条件更新 + fencing（不得裸 UPDATE）。

## 六、测试与基准

```bash
go test ./...                                  # 全量单测/集成（内存 SQLite，无外部依赖）
go test -run 'TestConcurrent|TestFenced|TestDeadNode' -v ./bench/   # 并发/一致性不变量
go test -bench . -benchmem -run '^$' ./bench/  # 基准（内存 SQLite，绝对值非生产数）
```

**MySQL 集成测试**（bench/mysql_integration_test.go，设 DSN 才跑，否则跳过）：
内存 SQLite 覆盖不了 InnoDB 行锁/SKIP LOCKED、严格模式（NO_ZERO_DATE——
Error 1292 零日期 bug 就是 SQLite 测不出来的）和 DSN loc=UTC 时间管线，
改认领/回收/fencing/时间相关代码后必须跑：

```bash
docker compose -f docker-compose.test.yml up -d --wait
APPTASK_TEST_MYSQL_DSN='mysql://app_task:app-task@127.0.0.1:13306/app_task_test' \
  go test ./bench/ -run 'TestMySQL' -v -count=1
docker compose -f docker-compose.test.yml down
```

- **新写并发/一致性代码必须同步写不变量测试**（参考 bench/concurrency_test.go
  的 I1–I6 编号法：认领排他/栅栏/排空完整/邮件唯一/cron 原子/接管隔离）；
  涉及 MySQL 特有语义的（锁/严格模式/时间序列化）同步补 TestMySQL* 集成版；
- **PR 前必须本地跑过新测试**——CI 是 Linux，本地 Windows 的 `-race` 不可用，
  靠测试纪律兜底（bench 并发测试曾三次在 CI 抓出本地没跑的问题）；
- 基准结果（内存 SQLite）不作为生产指标，仅用于路径对比与回归。

## 七、CI/CD 与发布

- `ci.yml`（PR/main）：gofmt → vet → test → build（四门全过才可合并）；
- `docker.yml`（main/tag）：发布镜像到 GHCR（`ghcr.io/atoncooper/app-task`）
  与 Docker Hub（`atoncooper/app-task`；需 secrets `DOCKERHUB_USERNAME`/
  `DOCKERHUB_TOKEN`，token 权限必须 Read & Write）；
- `release.yml`（tag `v*`）：GitHub Release + linux amd64/arm64 二进制 +
  checksums，版本号经 ldflags 注入；
- 发版流程：合并到 main 后 `git tag v0.x.0 && git push origin v0.x.0`。

## 八、提交规范

```
[模块] type: 简要说明（影响范围）

示例：
[scheduler] feat: 派发并行化（worker 池 + 认领前置）
[cli] fix: node ls 权重列与实际不符
[ui] style: 集群页密度调整
```

- type：`feat` / `fix` / `refactor` / `docs` / `test` / `chore` / `style`；
- 一个提交只动一个关注点；跨模块重构拆提交；
- PR 标题同首行格式；CI 全绿才可合并（merge commit）。

## 九、快速问题定位索引

| 问题 | 看哪里 |
|------|--------|
| 任务没被派发 / 派发了两次 | `service/scheduler.go`（tick/认领/finalize）+ `repo/task.go`（ClaimTasksBatch/回收） |
| 任务卡在 dispatching | 回收 TTL（`scheduler.dispatching_timeout_seconds`）与死节点判定（cluster 心跳） |
| 任务执行报错信息看不懂 | 先看 [docs/design.md](docs/design.md) §3 状态机与 §4 fencing；Lua 报错查 `internal/executor/lua.go` 的 ctx 实现 |
| 控制台页面空白/panic | 模板 `{{if}}/{{end}}` 配对（启动期 parse panic）；看启动日志首个 template 错误 |
| `at` 命令报 401/403 | 凭据链（flag > AT_TOKEN > APPTASK__WEBUI__TOKEN）；403=权限不足（admin 操作） |
| 配置不生效 | 占位符展开先于 env 覆盖；`${VAR}` 未设=空串；确认三处同步（yaml/config/env 名） |
| 邮件重复投递 / 卡 sending | `repo/email_queue.go`（claim/reclaim）+ `email_service.go`；多实例必须同 DB |
| 循环通知 / 渠道发送失败 | `service/notify.go`（渲染/幂等）+ `service/notify_channels.go`（钉钉/飞书/webhook 加签与 errcode）+ `repo/notify_channel.go`；钉钉/飞书 HTTP 200 也可能失败（看 body errcode/code） |
| 加入节点失败 | 准入模式（`cluster.admission`）、node_id 字符集、weight 上限（1–1000） |
| 内存/连接涨 | `rdbms.max_open_conns`（默认 25 ≥ workers）与 `conn_max_idle_time` |
