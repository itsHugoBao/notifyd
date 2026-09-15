# Sketch: readyz + queue stats

> Mode: Sketch（有界的运维可见性；不改动通知投递契约）
> Target repo: https://github.com/itsHugoBao/rc_hugobao
> Feature slug: `readyz-stats`
> Status: approved for implementation（Meetup 最小闭环演示）

## Problem

`GET /healthz` 只返回 200，不能证明 SQLite 可达。
运维也无法在不查库的情况下回答「有多少 pending / dead？」。

## Scope (in)

1. 保持 `GET /healthz` 为进程 liveness（不变：200 + 空 body 即可）。
2. 新增 `GET /readyz`：
   - store ping 成功：200 + JSON `{"status":"ready"}`
   - store ping 失败：503 + JSON `{"status":"not_ready","error":"..."}`
3. 新增 `GET /api/stats`：
   - 200 + 按状态计数的 JSON，例如
     `{"pending":0,"delivering":0,"succeeded":0,"dead":0}`
   - 只读；无鉴权（与 v1 内部服务同一信任模型）
4. 以最小方法扩展 `store.Store`（例如 `Ping`、`CountByStatus`）。
5. 为新 handler 与 store 方法补充单元测试。
6. 本 sketch 放入 PR 的 `specs/readyz-stats/sketch.md`。
7. 在 README 的 ops / endpoints 处做简短说明。

## Scope (out)

- Prometheus 指标、看板、鉴权、SSRF、批量重放
- 改动 POST/GET/redeliver 通知契约
- 超出 CountByStatus 所需的 schema 迁移（优先 COUNT 查询；不加新表）

## Definition of done

- `go test ./...` 通过（若现有测试已带 race，一并带上）
- 新端点有测试覆盖
- 对默认分支开 PR，并包含本 sketch 产物
- 不合并；合并交给人工

## Hypothesis (non-binding)

Store 接口已隔离 SQLite；`Ping` + `CountByStatus` 可以干净接入。实现前先核实。
