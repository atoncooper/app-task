# app-task 技术设计

> 本文面向想了解（或参与修改）app-task 内部实现的人。使用与部署见
> [README.md](../README.md)、[cluster.md](cluster.md)、[cli.md](cli.md)。

## 1. 架构总览：无主对称 + 单一权威存储

```
            ┌──────────────┐
            │  MySQL(自有) │  ← 唯一权威：任务/认领/fencing/名册 全在这
            └──┬───┬───┬──┘
       ┌──────┘   │   │└──────┐
   ┌───▼───┐  ┌───▼───┐ ┌──▼────┐
   │ node A │  │ node B │ │ node C │   ← 全部对称：每个实例跑完整 tick
   └───────┘  └───────┘ └───────┘
        │          │          │
        ▼          ▼          ▼
     executor_url（第三方执行器，HTTP，需幂等）
```

**刻意不做分布式共识/选主（Raft 等）**。理由：

- 调度状态的强一致权威已经存在（MySQL，ACID 事务）——互斥靠「条件更新 +
  fencing token」，不需要第二个协调层；
- Raft 只保证**日志复制**一致，从不保证**外部副作用**一致——上了 Raft，
  fencing 依然必须存在（被罢免的 leader 的迟到调用照样会发生）；
- 引入成本：raft 库 + WAL + 稳定成员列表 + ≥3 节点仲裁，与「单二进制、
  自带 MySQL、零额外组件」的形态冲突；
- 选主换来的只有「秒级 failover」和「follower 不空转」，而本调度器粒度是
  30s、空扫描 ~0.3 QPS——收益远小于成本。

**重新评估的触发条件**（满足其一再议）：调度状态搬出 MySQL（纯内存调度）、
failover 要求亚秒级、多区域 active-active 无法共享 MySQL。

## 2. 代码结构

```
main.go                  入口薄壳（嵌入 default.yaml，转交 internal/cli）
internal/cli/            at 命令行（serve + 客户端命令，cobra）
internal/config/         配置加载：default.yaml + ${VAR} 占位符 + APPTASK__ env 覆盖
internal/db/             MySQL 连接（DSN 组装/池参数）
internal/model/          GORM 模型（task/task_log/email_queue/script/.../cluster_node）
internal/repo/           数据访问（认领/finalize/名册/密钥/邮件队列）
internal/executor/       执行器：HTTP 透传 + 内置 Lua 沙箱
internal/service/        调度器 + 邮件投递 worker + 任务服务
internal/cluster/        节点名册：心跳/存活/死节点判定
internal/router/         Gin 路由：控制台(SSR)/api/* + API key 中间件 + 状态存储
internal/certgen/        控制台 HTTPS 自签证书生成与轮换
internal/security/       AES-256-GCM（中心密钥库）
internal/bench/…         bench/（仓库根）基准与并发/一致性测试
web/                     控制台模板与静态资源（Bootstrap 3 + CodeMirror 5，go:embed）
```

## 3. 任务生命周期与状态机

```
pending ──claim──► dispatching ──► completed        (同步 2xx)
                        │──► running ──► completed | failed   (异步 202+回调)
                        │──► pending            (失败且有重试额度，next_retry_at 退避)
                        └──► failed             (重试耗尽)
        ▲
        └── 回收：dispatching 超过 TTL(默认 120s) 或 所属节点被判死
```

每个 tick（默认 30s，实例间带随机抖动错峰）：

1. **cron 延展**：`ExtendCronTask` 单事务「条件占用 `cron_next_task_id`
   （预写 newID）+ INSERT 下一条」——并发延展绝不产生孤儿重复；
2. **死节点接管**：心跳失联节点的 `dispatching` 认领**立即**收回重派
   （不用等 120s TTL；fencing 保证卡顿未死节点的迟到结果按孤儿丢弃）；
3. **批量认领**：`ClaimTasksBatch` 在一个事务里读取候选
   （MySQL 上 `FOR UPDATE SKIP LOCKED`，并发实例读到互不相交的候选集）
   并整体翻转 `pending → dispatching`，每批铸造一个 fencing token；
   认领上限 = `min(batch_size, workers) × ceil(本机 weight ÷ 集群最大存活权重)`；
4. **并行派发**：每个已认领任务一个 goroutine，受两级闸门约束——
   全局池（`workers`）+ 按 executor_url 的并发闸（`per_url_limit`）；
   池/闸饱和 → **释放认领**（任务回 pending，下个 tick 再试，不算失败）；
5. **finalize**：全部带 fencing（`WHERE status='dispatching' AND
   claim_token=本批 token`）并清空认领字段；执行结果写 `task_log` 溯源；
6. **超时兜底**：`running` 超 10 分钟无回调 → failed（迟到的回调幂等返回
   当前状态，不复活任务）。

### 3.1 为什么「同时读到」不会导致「同时执行」

- 普通 SELECT 是 MVCC 快照读，**读本身无锁、无副作用**——它只产生候选名单；
- 候选要变成执行，必须先通过认领 UPDATE；而 **UPDATE 是当前读**：InnoDB
  拿行锁、并按最新已提交版本复查 `WHERE status='pending'`——第一台改写
  状态后，第二台的 UPDATE 匹配 0 行，认领失败，任务不在它的执行集合里；
- 一句话：**读是匿名的候选提名，写才是唯一 counts 的动作**；系统里没有
  任何路径是「读到什么就执行什么」而不在写入瞬间重新验证的。

## 4. fencing token（认领栅栏）

每次认领铸造新 `claim_token`（uuid）。所有 finalize/release 都必须携带
token 且匹配当前认领才生效：

- **防陈旧持有者覆盖**：实例卡顿超过 dispatching TTL → 认领被回收重派 →
  旧持有者晚到的 finalize 匹配 0 行被拒，结果记
  `orphaned dispatch result discarded`（WARN）并丢弃——**状态转换恰好一次**；
- 副作用层面无法完全消除重复（旧持有者的执行器调用可能已在途）——这就是
  **执行器必须幂等**契约的由来：dispatch 携带 `X-Task-Id` 供执行器去重，
  app-pay 超时委托用 payload version 守卫。

## 5. 完成即提交：持久化策略

- finalize/task_log 都是**同步逐条提交**（阻塞到 MySQL 确认），无缓冲无延迟；
- 持久性由 InnoDB 的 redo log（WAL）担保——**不自建应用层 WAL**：应用 WAL
  与 InnoDB redo 是两份独立日志，崩溃后需要双写对账，是负资产；
- 崩溃窗口矩阵：finalize 前崩 → 回收重派（不丢，可能重复）；提交后崩 →
  InnoDB 保证完整。最坏是 at-least-once，靠执行器幂等兜底；
- 异步回调丢失 → 10 分钟超时扫描兜底判 failed（有界延迟，非丢失）；
- 只有当调度状态脱离 MySQL 时才需要自建 WAL——当前 SCFQ 队列（预留 M4）
  自带 WAL，届时由该组件负责。

## 6. 权重分摊

- 配置：`scheduler.weight`（默认 1）；权重经心跳写入名册，**集群动态感知**；
- 公式：`本 tick 认领上限 = min(batch_size, workers) × ceil(weight ÷ 集群最大存活权重)`；
- 性质：全等权重=不缩放；最大权重节点宕机 → 分母自动缩小 → 存活节点回满；
- 边界：权重是**容量上限的相对缩放**（任务少时先到先得），不是严格公平
  队列——按流维度的严格公平是预留的 SCFQ 队列（`task.weight`，M4）的职责。

## 7. 邮件队列

- 执行器 POST 标准邮件到 `/internal/email/send` → `pending` 入库；
- 投递 worker 认领（`pending → sending`，条件更新 + token）→ Resend 发送 →
  `sent`；失败指数退避重试，超限 `failed`（控制台可手动重试）；
- `sending` 超 2 分钟回收重发（Resend 客户端超时 1 分钟 < 2 分钟窗口）；
- 多实例下认领互斥，不会重复投递；密钥值全程脱敏（日志/错误信息）。

## 8. 控制台状态存储（memory / redis）

控制台的会话、API 密钥一次性展示、失败节流、每 key 限流默认存**进程内存**
（单实例零依赖）。多实例共享一个控制台域名时切换 `webui.session_store:
redis`（`redis.url` 支持 rediss:// TLS，池/超时/重试可配）：

- 会话：SET/GET/DEL + TTL（过期即失效）；
- 一次性展示：GETDEL 原子单读；
- 节流/限流：INCR+EXPIRE 固定窗口；
- **故障语义**：节流/限流 fail-open（可用性优先，对齐主 app 的 Redis 语义）；
  会话读失败=重新登录；Redis 完全宕机不影响任务调度。

## 9. 加入流程与准入

- 预登记（`POST /api/cluster/join`，admin）：名册创建 `pending_join` 行
  （node_id/weight/invited_by 留痕；幂等刷新；active 冲突 409），返回加入
  指引（**RDBMS 连接串永不经 API 回显**——占位符 + 密钥渠道获取）；
- 新节点首次心跳自动 `pending_join → active`（joined_via=auto/cli/web）；
- 准入闸门：`cluster.admission: pre_approved` 时未预登记节点**拒绝启动**
  （fail-closed）；
- node_id 字符集 `^[A-Za-z0-9][A-Za-z0-9._-]{1,63}$`（流入日志/owner/重定向，
  防日志注入）；weight 钳制 1–1000（防加权算术溢出）；
- 名册可见性：控制台「集群」页与 `/api/cluster` 为 **admin 门**。

## 10. 一致性模型

- **总体策略**：刻意**不做分布式共识/分布式锁**——所有调度状态收敛到单一
  MySQL（ACID 事务），实例全部对称无状态，互斥靠「条件更新 + fencing」。
  好处：不存在脑裂面；代价：MySQL 是单点（宕机全集群暂停，不做多主）。
- **保证的性质**：
  - **状态机转换恰好一次**（exactly-once transitions）——认领/finalize/
    回收/回调/cron 延展全部条件更新；
  - **执行副作用 at-least-once**（绝不丢失、可能重复触发）——执行器必须
    幂等（`X-Task-Id` 去重；app-pay 委托用 payload version 守卫）；
  - **迟到成功丢弃**——running 超时判 failed 后到达的回调不复活任务。
- **显式取舍（可用性 > 一致性）**：限流/节流 Redis 故障时 fail-open；
  回收竞争窗口内允许重复触发（幂等兜底）。
- **时钟**：认领 TTL 用实例本地时钟，120s TTL 对秒级偏斜有约百倍裕度。

## 11. 连接与池配置

- **MySQL**：`max_open/max_idle`（池大小，默认 25/10，≥ worker 并发）、
  `conn_max_lifetime/idle_time`（防长连接老化与中间件静默断连）、
  `dial/read/write_timeout_seconds`（**驱动默认读/写无超时，hung DB 会冻结
  worker——必须强制有界**）、`tls`（true/skip-verify，DSN 参数注入）、
  `prepare_stmt`（预编译缓存）、`slow_threshold_ms`（慢查询 WARN）。DSN 由
  配置组装（DSN 自带 query 丢弃——配置权威）；
- **Redis**：`pool_size/min_idle_conns/超时/max_retries`（0 = go-redis 默认：
  池 10×CPU、dial 5s、读写 3s、重试 3）；
- **出站 HTTP**：`MaxIdleConnsPerHost` 匹配派发池宽；Lua `ctx.http_*` 与
  http 派发共用代理环境变量（NO_PROXY 覆盖内网服务名）。

## 12. 安全模型：伪造节点能不能加入？

把"伪造节点"分成两种威胁，控制手段完全不同：

### 12.1 威胁 A：没有 DB 凭据的伪造者 → 进不来

加入集群的每一个动作（心跳写名册、认领改任务）都是对自有 MySQL 的写入；
没有 DSN 连名册都摸不到。暴露面也已收紧：控制台/服务端口仅绑 127.0.0.1、
MySQL 不出容器网络、服务面有 API key 门。

### 12.2 威胁 B：持有 DB 凭据的伪造者 → app 层校验无效，靠下沉

持有 DSN 的攻击者直接写库即可（改任务状态、插名册行），此时：

- **app 层 join token 校验 = 安全剧场**：校验逻辑读的是它可写的同一个库；
- **fencing 也拦不住**：fencing 约束遵守协议的诚实节点，不约束直接写库者；
- 结论：认证必须发生在 app 进程之外——即"谁能连上数据库"。

### 12.3 三层防线的归位

| 层 | 手段 | 防什么 |
|----|------|--------|
| 治理层（app） | 预登记（admin）+ 留痕 + `pre_approved` 准入 | 误配置混入 / 无序扩容；审计"谁邀请的" |
| 网络层（IP） | MySQL 账号 host 限定 + 端口不出内网 | 连接级拒绝（握手都过不了） |
| 身份层（指纹） | MySQL `REQUIRE X509` + 每节点客户端证书 | 密码学节点身份：可吊销单节点、可审计来源 |

网络层加固示例：

```sql
CREATE USER 'app_task'@'172.18.0.%' IDENTIFIED BY '<dsn密码>';
GRANT ALL ON app_task.* TO 'app_task'@'172.18.0.%';
-- 更强身份：ALTER USER ... REQUIRE X509;（app-task 侧 DSN 追加 &tls=true）
```

**既有防线**：最小权限（账号只授 `app_task` 一个库，凭据泄露污染范围限
调度域）；审计（名册 invited_by/joined_via + task.owner + join API 留痕）。

## 13. 测试与基准

- **单元/集成测试**：全量 `go test ./...`（内存 SQLite，无需外部依赖）；
- **并发/一致性不变量测试**：`bench/concurrency_test.go`（认领排他、栅栏
  finalize、混合负载排空、邮件认领唯一、cron 原子、死节点接管）；
- **基准**：`bench/`（批量认领/并行竞争/认领+finalize/邮件/黑盒排空吞吐），
  运行 `go test -bench . -benchmem -run '^$' ./bench/`——内存 SQLite，
  绝对值非生产数，用于路径对比与回归；
- **多实例验收**：`--profile ha` 双实例 → `at node ls` / `at ps` / kill 演练。

## 14. 配置参考

| 配置 | 默认 | 说明 |
|------|------|------|
| `scheduler.interval_seconds` | 30 | 轮询间隔 |
| `scheduler.workers` | 16 | 全局派发并发 |
| `scheduler.batch_size` | 50 | 每 tick 认领上限基数 |
| `scheduler.dispatching_timeout_seconds` | 120 | dispatching 认领回收时长 |
| `scheduler.per_url_limit` | 8 | 单 executor_url 并发闸 |
| `scheduler.weight` | 1 | 本节点派发份额 |
| `scheduler.instance_id` | hostname+rand | 节点身份（owner/名册共用） |
| `scheduler.max_shards` | 32 | 分片广播扇出上限（存活节点数可被任务 shard_total 覆写） |
| `notify.http_timeout_seconds` | 10 | notify 渠道出站超时 |
| `notify.rate_limit_per_min` | 18 | notify 渠道限速（钉钉硬上限 20/min） |
| `ai.*` | 见 §17 | base_url/model/api_key/timeout/max_tokens/send_payload_data；model 空 = AI 关闭 |
| `cluster.admission` | open | open \| pre_approved |
| `rdbms.*` | 25/10 池等 | 见 §11 |
| `redis.*` | 库默认 | 见 §11 |
| `webui.session_store` | memory | memory \| redis |
| `webui.session_ttl_minutes` | 720 | 登录会话有效期 |
| `lua.timeout_seconds` | 30 | 单脚本执行超时 |
| `email.provider` | resend | 邮件通道 |

## 15. 分片广播

任务标记 `shard=true` 后，触发不再是单次执行，而是**按当时的存活节点数分裂成 N 个子任务**并行执行；每个子任务的 payload/HTTP 头携带 `shard_index`（0 起）/`shard_total`，执行器按片处理数据（如 `id % shard_total = shard_index`）。

```
触发行(pending) ──认领──► 持有者分裂（单事务：fenced 转 running + 插入 N 个子任务）
                              │
        子任务 N 个(pending，立即到期) ──► 各节点经正常认领分散执行（SKIP LOCKED 天然分片）
                              │
        子任务各自 finalize ──► 最后到达终态者独占收尾父任务（NOT EXISTS 未终态子片）
```

- **I7 分裂恰一次**：认领排他保证只有恰一个节点分裂；分裂是单事务（fenced 父转
  running + 插全部子片），不存在半分裂态；`uk_shard(parent_task_id, shard_index)`
  唯一索引兜底——NULL parent 的普通任务不受 MySQL 唯一约束影响。
- **I8 父任务恰一次收尾**：条件更新让并发收尾恰有一个胜者；任一子片 failed →
  父 failed，全部 completed → 父 completed。
- 存活节点数 ≤1 退化为普通执行；与 async 执行器互斥（创建期拒绝）；running
  超时清扫豁免 `shard=true` 行（父任务由子片门控，不由回调门控）。
- 片数上限 `scheduler.max_shards`（默认 32），任务可用 `shard_total` 固定覆写。
- 子片是普通任务：TTL 回收/死节点接管/重试/节点追溯（task_log.node）全部适用。

## 16. 业务日历

cron 表达不了"节假日不发、调休周末要发"。业务日历在 **cron 物化时**（计算下一次
触发，含 `extendCronTasks` 与任务创建）逐日判定可触发性：

| 标记 | 语义 |
|------|------|
| `off`（休） | 该日整日跳过触发（法定节假日） |
| `work`（班） | 该日按**周一**参与星期匹配（调休补班周六让 `0 9 * * 1-5` 触发） |
| 未标注 | 按 cron 原义 |

- 生效规则一句话：**被标"班"的日期按周一参与星期匹配**。dom/month 始终以日期
  本身计算；dom 与 dow 同时受限时沿用标准 cron 的 OR 语义（与 cronexpr 一致）。
- 逐日推进上限 400 天，超窗报错（fail-loud，不会静默丢触发）。
- 日历变更只影响未来的物化；已 pending 的触发行照发。删除日历后，引用它的
  任务其 cron 物化会 fail-loud（日志可见），直到改指向——宁可停也不在错误
  的日子静默开跑。
- 一次性 trigger_time 任务不受日历影响（用户显式指定日期）。
- 实现：`service/bizcalendar.go` 自含 5 段 cron 字段解析（dom/month/dow 集合 +
  标准 OR 语义），物化侧一次日历加载 + 逐日匹配，无 N+1 查询。

## 17. AI 能力（LLM 网关 + 三个消费方）

AI 是**增强不是依赖**：`ai.model` / `ai.api_key` 任一未配置，全部 AI 功能降级、
平台能力零回退。网关（`service/llm.go`）只讲 OpenAI 兼容 chat completions
（DeepSeek/Qwen/Moonshot/Ollama/OneAPI 通用），api_key 在所有日志/错误路径 mask。

| 功能 | 触发 | AI 关闭时 |
|------|------|-----------|
| AI 执行日报（`task_type=digest`） | 用户 cron 编排（可配业务日历） | 确定性统计模板（成功/失败/重试/TopN），**日报永不缺席** |
| 失败告警 + 根因诊断 | 任务最终失败（重试耗尽），`alert_channel` 渠道推送 | 纯事实告警（无【AI 诊断】段） |
| AI 生成 Lua 脚本 | 编辑器「AI 生成」按钮 | 按钮仍显示但端点返回 503 |

### 17.1 digest：调度语义全复用

日报做成任务类型而非系统级定时器——时机归用户 cron、互斥归认领（多实例
不重复发）、投递归 notify 渠道、留痕归 task_log。统计窗口 24h（weekly=7d）：
成功/失败/重试计数 + 失败 TopN + 最慢 TopN（聚合查询无 N+1）；窗口外数据
绝不进 prompt（测试锁定）。

### 17.2 失败告警：异步 + 隐私默认关

最终失败（仅此一次，重试过程不发）→ 后台 goroutine 投递（panic 隔离，
不阻塞派发池）→ `alert_channel` 渠道；AI 配置时附 ≤150 字根因 + 修复建议。
**隐私**：RCA prompt 默认不含任务 payload（`ai.send_payload_data=true` 才发
——payload 可能含业务数据）；诊断文本随 task_log 留痕。

### 17.3 AI 生成 Lua：生成 ≠ 上传

端点只产出文本，保存/启用/试运行全部走现有人工路径。质量与安全三闸：
① System prompt 内嵌沙箱 API 契约（ctx.* 全签名 + 无 os/io + 幂等要求），
模型只能在给定 API 面内写代码；② 返回代码过**真实编译门**
（`executor.CompileForValidation`，与人工上传同一解析器），编译失败自动
喂回错误重试一次，仍失败把错误透给用户；③ admin 门禁 + 会话级限速
（5 次/分钟）防刷账单。沙箱 VM 本身禁 os/io/debug——AI 写了也跑不了
系统调用，这是"GLUE 演进"的安全底气。
