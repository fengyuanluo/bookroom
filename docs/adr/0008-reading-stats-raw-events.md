# 阅读统计存原始翻页事件，聚合在查询时计算

Readest（`services/statistics/*`、`ReadingStatsTracker.tsx`）和 KOReader（`readest.koplugin/readest_syncstats.lua`）记录的都是 KOReader 格式的翻页事件 `(书哈希, 页, 开始时间, 时长, 总页数)`，通过 `/api/sync` 的 `statBooks`/`statPages` 同步。服务端原样存储这些事件，以 `(用户, 书哈希, 页, 开始时间)` 为唯一键，重复上报时保留较长的时长（与两端客户端相同，见 `readest_syncstats.lua:77,129`）；拉取游标用 `updated_at_ms`（`statsSync.ts:96`）。每日时长、每本书总时长、连续阅读天数由 SQL 在查询时计算，通过 OpenAPI 提供给 Agent 和其他客户端使用；客户端自己的统计界面不变。

## 考虑过的方案

- 服务端自己切分阅读会话、设最短时长门槛：什么算阅读已经由客户端决定（翻页、空闲、切到后台时结束一段），服务端再定义一次会让各设备显示的数字对不上。
- 照搬官方的冷数据归档（`app/api/stats/compact`，旧事件压缩后存进 R2）：每阅读一小时约 60 条事件 `[INFERENCE]`，一年十万级行，SQLite 直接查即可。

## 后果

- 删书不删统计：`statBooks` 带 `deleted_at` 墓碑（`statsSync.ts:118`），历史阅读时长仍然保留。
