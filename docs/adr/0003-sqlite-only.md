# 只用 SQLite

数据库只用 SQLite（WAL 模式），驱动选 `modernc.org/sqlite`（纯 Go，不需要 cgo）：发布物是单个二进制，备份就是复制一个文件。规模估算：2 万本书的元数据约 5 MiB，列表查询 6–7 ms（`research/ScalePerf.md`）。对标的 Komga、Kavita、BookLore、BookOrbit、Calibre-Web 都只支持一种数据库；做过双库的项目维护者反馈 CI 和迁移都要做两份，功能只能取两边的交集（`research/DBPortability.md`）。

## 考虑过的方案

- 同时支持 SQLite 和 PostgreSQL：sqlc 和迁移都要维护两套。
- 只用 PostgreSQL：多一个要运维的服务，换来的并发写能力这个规模用不上。

## 后果

- SQL 只用两种数据库都支持的写法：不用 `INSERT OR REPLACE`，不依赖 `rowid`，时间统一存 UTC 整数毫秒。将来真要迁移，成本只在驱动和迁移脚本上。
- 出现以下任一情况时，重新评估是否上 PostgreSQL：多个进程同时写库；数据库文件放在网络文件系统上；持续出现 `SQLITE_BUSY`。
