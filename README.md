# notifyd — 可靠的对外通知投递服务

企业内部业务系统在关键事件发生时需要通知外部供应商的 HTTP API。本服务把「调用外部
API」从业务系统里剥离出来：业务系统提交一次通知请求拿到 `202` 即可遗忘，之后由本
服务负责**尽可能可靠地**送达——容忍供应商瞬时故障、自身进程重启和短时外部中断。

本仓库采用 **SDD（Spec-Driven Development）** 流程开发：先写 spec 并经人工评审，
再拆任务、实现、测试。完整设计契约见 [specs/spec.md](specs/spec.md)，任务拆解见
[specs/tasks.md](specs/tasks.md)，AI 协作过程见 [AI-USAGE.md](AI-USAGE.md)。
commit 历史即开发过程：spec → 拆任务 → 脚手架 → 存储层 → API → 投递引擎 → 测试 → 文档。

## 对问题的理解

题目的核心不是「转发 HTTP 请求」，而是**投递责任的转移**：业务系统不关心供应商返回
值，只要求通知可靠送达。因此本服务的本质是一条带持久化的异步投递通道，关键问题依次是：

1. 什么时刻起服务对通知负责？——**先持久化、后返回 202**，从 202 起负责到底；
2. 失败了怎么办？——区分**可自愈的失败**（重试）与**不可自愈的失败**（快速死信）；
3. 供应商长期挂了怎么办？——重试预算耗尽进**死信**，恢复后**人工重放**，
   让长期故障成为显式、可运维的事件而不是无限膨胀的重试队列。

各供应商 URL / Header / Body 各异这一点，恰恰通过**不做格式适配**来解决：调用方
提交最终成型的请求，本服务对 payload 无感知——供应商格式再怎么变，本服务不用改。

## 快速开始

```bash
go build -o notifyd ./cmd/notifyd && ./notifyd
# 默认监听 :8080，SQLite 落盘 ./notifyd.db
```

提交一条通知（拿到 202 后调用方即可遗忘）：

```bash
curl -X POST http://localhost:8080/api/notifications -d '{
  "target_url": "https://vendor.example.com/hooks/registration",
  "headers": {"Authorization": "Bearer xxx", "Content-Type": "application/json"},
  "body": "{\"user_id\": 42}",
  "idempotency_key": "reg-42-2026-07-16"
}'
```

查询状态 / 重放死信：

```bash
curl http://localhost:8080/api/notifications/<id>
curl -X POST http://localhost:8080/api/notifications/<id>/redeliver
```

运行测试（单元 + 端到端，含 race detector）：

```bash
go test -race ./...
```

### 配置（环境变量，均有默认值）

| 变量 | 默认 | 说明 |
|------|------|------|
| `NOTIFYD_ADDR` | `:8080` | 监听地址 |
| `NOTIFYD_DB_PATH` | `notifyd.db` | SQLite 路径 |
| `NOTIFYD_WORKERS` | `8` | 并发投递 worker 数 |
| `NOTIFYD_POLL_INTERVAL` | `500ms` | 调度轮询间隔 |
| `NOTIFYD_ATTEMPT_TIMEOUT` | `10s` | 单次投递超时 |
| `NOTIFYD_RETRY_BASE` / `NOTIFYD_RETRY_CAP` | `5s` / `1h` | 指数退避基数 / 上限 |
| `NOTIFYD_MAX_ATTEMPTS` | `12` | 重试预算（约 7 小时跨度） |
| `NOTIFYD_VISIBILITY_TIMEOUT` | `60s` | 崩溃恢复可见性超时 |
| `NOTIFYD_MAX_BODY_BYTES` | `262144` | 提交 body 上限 |

## 整体架构

```
 业务系统 ──HTTP──► ┌────────────────────────────────────┐
                    │  单个 Go 二进制                     │
                    │  ┌──────────┐      ┌────────────┐  │      ┌────────────┐
                    │  │ HTTP API │─────►│  SQLite    │  │      │  供应商 A   │
                    │  └──────────┘ 落盘 │ (WAL 模式) │  │ ┌───►│  供应商 B   │
                    │                    └─────┬──────┘  │ │    │  供应商 C   │
                    │                          │ 轮询    │ │    └────────────┘
                    │                    ┌─────▼──────┐  │ │
                    │                    │ dispatcher │──┼─┘  超时 / 重试 /
                    │                    │ + workers  │  │    退避 / 死信
                    │                    └────────────┘  │
                    └────────────────────────────────────┘
```

- `internal/store`：SQLite 持久化队列。状态机 `pending → delivering → succeeded/dead`，
  认领用单条原子 `UPDATE ... RETURNING`，无双认领窗口。
- `internal/dispatch`：轮询认领 → worker 池并发投递 → 按结果分类推进状态机；
  另有后台回收循环处理认领超时（进程崩溃残留）。
- `internal/api`：提交（202/200/409/400）、状态查询、死信重放。

代码 ~600 行，测试 ~700 行；唯一第三方依赖是纯 Go 的 SQLite 驱动 `modernc.org/sqlite`
（免 cgo）。不引入 web 框架、uuid 库——标准库够用时不加依赖。

## 设计说明（作业必答题）

### 1. 系统边界

**解决**：先持久化后 ack、退避重试、at-least-once、死信与人工重放、幂等提交、
状态查询、崩溃恢复（spec §2.1）。

**明确不解决**（spec §2.2 逐条有理由）：

- **供应商格式适配**——调用方提交最终请求，保持本服务 payload 无感知，边界才稳定；
- **exactly-once**——纯 HTTP 上不可实现，透传 `X-Notification-Id` 让接收方可去重；
- **顺序投递**——与并发投递、重试调度冲突，题目场景不需要；
- **鉴权 / SSRF 防护 / 限流熔断 / 管理后台**——v1 可信内网 + 低流量下是过早复杂度，
  全部列入演进路径并写明触发条件。

### 2. 可靠性与失败处理

**投递语义：at-least-once**。at-most-once 会丢通知，exactly-once 做不到——
供应商处理成功但响应丢失时，除重试外别无选择，重复不可避免。诚实的做法是
承认重复并给接收方去重抓手（`X-Notification-Id` 头）。

**失败分类**（spec §5.2）：网络错误 / 超时 / 408 / 425 / 429 / 5xx 可重试；
其余 4xx 与 3xx 是配置错误，**一次即死信**——对着 bug 消耗重试预算只会推迟
人工发现问题的时间。

**长期不可用**：指数退避（5s 起、封顶 1h、共 12 次、±20% 抖动）自动吸收约 7 小时
内的故障；超出预算进入 `dead`，供应商恢复后经 `POST /redeliver` 人工重放。
进程崩溃的兜底：认领超过可见性超时（60s）的任务自动收回重投。

### 3. 取舍与演进

**最大取舍：SQLite 当持久化队列，不用消息中间件。** 理由：

1. 入队必须与 ack 同一事务——SQLite 在进程内同时给到持久化与事务性，运维面为零；
2. 「任意延迟的重试调度」在 SQL 里是一列 `next_attempt_at`，在多数 broker 里
   反而要折腾延迟队列 / 死信交换机；
3. 题目明确流量不大，引入 broker 是纯运维成本。

**不使用时的替代方案**：Postgres + `SELECT ... FOR UPDATE SKIP LOCKED`，同一套
设计直接多节点——这正是演进第一步。存储层已用接口隔离，替换只动认领查询。

**演进路径**（spec §8，按触发条件排序）：吞吐超单节点 → Postgres 多副本；
慢供应商拖垮他人 → 按目标并发限制与熔断；写入量极高 → Kafka 做摄入削峰
（DB 仍是状态存储）；出现半可信调用方 → 鉴权 + URL 白名单；再往后是指标、
批量重放、HMAC 签名。

## 时间盒说明

作业建议 ≤4 小时，实际投入约 3.5 小时（含 spec 评审与文档）。刻意没做的事、
以及"如果有更多时间会做什么"，与演进路径是同一份清单（spec §2.2 / §8）——
scope 克制本身是本次交付的一部分。若再多半天，优先级最高的三件事：
按目标 host 的并发上限（防慢供应商占满 worker 池）、Prometheus 指标
（队列深度 / 死信数 / 投递延迟）、批量重放 API。
