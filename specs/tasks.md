# Tasks：实现任务拆解

> 由 [spec.md](spec.md) 派生。每个任务对应一个独立 commit，按序实现；
> 每个任务标注其覆盖的 spec 条款，实现与 review 时对照验收。

## 总览

| # | 任务 | 覆盖 spec | commit 类型 |
|---|------|-----------|------------|
| T1 | 项目脚手架 | §6、§7 | `scaffold` |
| T2 | 存储层（SQLite 队列） | §4、S1、S5、S7 | `feat` |
| T3 | HTTP API | §3、S6 | `feat` |
| T4 | 投递引擎（dispatcher + worker） | §5、S2、S3、S4 | `feat` |
| T5 | 集成测试 | §10 | `test` |
| T6 | README | — | `docs` |

## T1 项目脚手架

- `go.mod`（module `notifyd`）
- 目录结构：
  ```
  cmd/notifyd/main.go      # 组装 + 优雅停机
  internal/config/         # 环境变量配置（§7 全部参数 + 默认值）
  internal/store/          # T2
  internal/api/            # T3
  internal/dispatch/       # T4
  ```
- 依赖决策：**仅 `modernc.org/sqlite`**（纯 Go 驱动，免 cgo，交叉编译友好）。
  路由用 Go 1.22+ 标准库 `net/http` 方法路由，不引入 web 框架；
  id 用 `crypto/rand` 生成，不引入 uuid 库。
- `.gitignore`（二进制、`*.db*`）

**验收**：`go build ./...` 通过；进程能启动、监听、Ctrl-C 优雅退出。

## T2 存储层（SQLite 队列）

- schema 迁移（启动时执行，§4 单表 + `(status, next_attempt_at)` 索引），WAL 模式
- `Store` 接口与 SQLite 实现：
  - `Create`：插入 + 幂等键处理（同 key 同 payload → 返回已有记录；
    同 key 不同 payload → 冲突错误；§3.1）
  - `Get`：按 id 查询
  - `ClaimDue(limit)`：原子 `UPDATE ... RETURNING`，`pending → delivering`（§6 认领原子性）
  - `MarkSucceeded / MarkRetry(nextAt, lastErr) / MarkDead(lastErr)`
  - `ReclaimStale(visibilityTimeout)`：`delivering` 超时回收为 `pending`（§5.5）
  - `Redeliver`：`dead → pending`，重置预算（§3.3）
- 存储层单元测试（临时 DB 文件）：幂等键三种分支、claim 不重复认领、stale 回收

**验收**：`go test ./internal/store/` 通过；接口不泄漏 SQL 细节（为 §8 演进第 1 步留缝）。

## T3 HTTP API

- `POST /api/notifications`：校验（URL 合法且 http/https、method ∈ {POST,PUT,PATCH}、
  body ≤ 256 KiB）→ 202 / 200 / 409 / 400（§3.1）
- `GET /api/notifications/{id}`：200 / 404（§3.2）
- `POST /api/notifications/{id}/redeliver`：200 / 404 / 409（§3.3）
- 统一 JSON 错误格式；结构化日志（`log/slog`）

**验收**：`go test ./internal/api/`（httptest 驱动 handler）通过；响应码与 §3 逐条一致。

## T4 投递引擎

- `internal/dispatch`：
  - 退避计算：`min(base·2^(n-1), cap) ± 20% jitter`（§5.3），纯函数
  - 结果分类：2xx / 可重试（408、425、429、5xx、网络错误）/ 永久（其余 4xx、3xx）（§5.2），纯函数
  - 投递 client：10s 超时、不跟随重定向、注入 `X-Notification-Id`（§3.4）
  - dispatcher 循环：轮询 → `ClaimDue` → 分发 worker 池（默认 8）→ 按结果写回状态
  - 后台 stale 回收循环（§5.5）
  - 优雅停机：context 取消 → 停止认领 → 等在途完成（§6）
- 单元测试：退避（增长 / 封顶 / 抖动范围）、分类表驱动测试

**验收**：`go test ./internal/dispatch/` 通过；关掉进程再启动，未完成任务能继续投递。

## T5 集成测试（端到端）

`httptest` 模拟供应商，起完整服务栈（真实 store + dispatcher + api），覆盖 §10 场景：

1. 提交 → 供应商失败 2 次后成功 → `succeeded` 且 attempts=3
2. 供应商返回 400 → 立即 `dead`
3. 供应商持续 500 → 预算耗尽 → `dead`
4. `dead` → `POST /redeliver` → 恢复投递 → `succeeded`
5. 幂等键重放 → 同一 id；同 key 不同 payload → 409
6. 崩溃恢复：伪造 `delivering` + 过期 `claimed_at` → 被回收并成功投递
7. 供应商收到的请求：method / headers / body 原样 + `X-Notification-Id`

测试中重试参数调小（base 数十 ms）保证测试秒级完成。

**验收**：`go test ./...` 全绿，无 flaky（时间相关断言留余量）。

## T6 README

- `README.md`（即设计文档）：
  - 问题理解、整体架构（图）、快速开始（build / run / curl 示例）
  - 系统边界（指向 spec §2）、投递语义与长期故障（§5）、
    取舍与演进（§6.1、§8）
  - 中间件说明：为什么不用 MQ、替代方案（§6.1）
- `AI-USAGE.md`：
  - AI 在哪些环节提供帮助（需求分析、spec 起草、实现、测试）
  - **未采纳的 AI 建议清单**（MQ/Kafka、模板适配层、exactly-once、鉴权、
    熔断限流、管理后台……及不采纳理由）
  - 人工做出的关键决策（技术栈、SQLite-as-queue、4xx 快速死信、409 幂等冲突语义）
  - 工作流记录：SDD 流程 + Claude 实现 + Grok 交叉评审 + 人工仲裁

**验收**：README 能让一个没看过 spec 的工程师 10 分钟内跑起服务并理解设计。
