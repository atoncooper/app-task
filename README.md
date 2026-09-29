# app-task

**定时任务执行器** —— 把"到点要做的事"注册进来，到点自动执行，结果可查、失败自动重试、可发邮件通知。

- ⏰ **定时 / 一次性任务**：cron 表达式或指定触发时间
- 🔌 **两种执行方式**：调用你的 HTTP 接口，或执行内置的 Lua 脚本
- 🔁 **失败自动重试**：指数退避，次数可配
- 📜 **全程留痕**：每次执行的请求/响应/耗时/错误都可查
- 📧 **邮件通知**：任务可发邮件，投递失败自动重试
- 🖥 **管理控制台**：网页上建任务、写脚本、看日志、管集群
- ⌨️ **命令行 `at`**：常用的都能在命令行完成
- 🏗 **多实例集群**：流量大了或要求高可用时，直接加机器

---

## 快速开始

```bash
docker compose up -d --build
```

启动后打开控制台：**http://localhost:8001**

首次启动自动创建管理员：**admin / app-task-admin**（请立即在「账户」页改密码）。

> 只想要命令行？见 [docs/cli.md](docs/cli.md)。需要多台机器组集群？见 [docs/cluster.md](docs/cluster.md)。

## 控制台能做什么

| 页面 | 用途 |
|------|------|
| 仪表盘 | 任务/日志/邮件/节点总览 |
| 任务管理 | 新建任务（HTTP 或 Lua）、看状态、看执行历史 |
| 执行日志 | 所有执行的记录：触发时间、耗时、结果/错误 |
| Lua 脚本 | 在线写脚本、保存即生效、可试运行 |
| 邮件队列 | 平台代发的邮件，失败可一键重试 |
| 集群 | 节点列表、存活状态、加入新节点（多实例时） |
| API 密钥 | 给程序调用用的密钥（生成/吊销） |
| 密钥管理 | 存第三方服务的凭据，脚本里按名字取用，只写不读 |

## 怎么建一个任务

**方式一：控制台** —— 「任务管理 → 新建任务」，填触发时间或 cron，选 HTTP 或 Lua 类型即可。

**方式二：命令行**

```bash
# 每天 23 点跑一个 Lua 脚本
at task create --type lua --script-id <脚本ID> --cron "0 23 * * *"

# 一次性 HTTP 任务（到点 POST 你的接口）
at task create --executor-url https://你的接口/run --trigger-time "2026-10-01 09:00:00"
```

**HTTP 任务**：到点时 app-task 会把任务的 payload（JSON）POST 到你填的接口，
返回 2xx 视为成功。建议接口做幂等：极端情况下（机器断电等）同一条任务
可能被投递两次，用任务 ID（请求头 `X-Task-Id`）去重即可。

**Lua 任务**：脚本里用内置的 `ctx` 完成逻辑（发 HTTP、读密钥、记日志、
控制重试），写错不慌——保存即新版本，随时改。

## 怎么发邮件

在「密钥管理」页配好 Resend 的 API Key，任务的执行脚本里即可发信；
发过的邮件在「邮件队列」页可见，失败的会自动重试，也可手动重发。

## 配置

全部通过环境变量配置（可写在 `.env` 里）：

| 变量 | 说明 | 默认 |
|------|------|------|
| `APPTASK__RDBMS__URL` | MySQL 连接串（**必填**） | — |
| `APPTASK__EMAIL__API_KEY` | Resend API Key（不发邮件可不填） | — |
| `APPTASK__WEBUI__TOKEN` | 控制台 API 令牌（给 `at` 命令/程序用） | — |
| `APPTASK__WEBUI__SESSION_STORE` | `memory`（默认）或 `redis` | memory |
| `APPTASK__SCHEDULER__WEIGHT` | 本节点分任务的份额（集群用） | 1 |
| `APPTASK__SERVER__TLS__ENABLED` | 控制台启用 HTTPS | false |
| `APPTASK__TIMEZONE` | 业务时区 | Asia/Shanghai |

完整列表见 `default.yaml`（每个参数都有注释）。

## 多实例 / 集群

一台机器不够用或要求高可用时，直接加机器——新节点指向同一个数据库，
启动即自动加入，任务自动分摊，节点宕机其任务自动被其他节点接管。

- 第二台起：`docker compose --profile ha up -d app-task-ha`，或按控制台
  「集群」页的加入指引部署新节点
- 想让某台少干点活（比如还兼跑别的服务）：给它设低一点的
  `APPTASK__SCHEDULER__WEIGHT`

详见 [docs/cluster.md](docs/cluster.md)。

## 升级与数据

- 拉新镜像/新代码后 `docker compose up -d` 即可，数据库结构自动迁移，
  任务与历史不丢；
- 数据都在 MySQL 与命名卷里，备份这两样即可。

## 文档

- [docs/cli.md](docs/cli.md) —— `at` 命令行完整参考
- [docs/cluster.md](docs/cluster.md) —— 集群部署与运维指南
- [docs/design.md](docs/design.md) —— 技术设计与实现细节（想深入了解再看）
