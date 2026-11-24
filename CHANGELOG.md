# Changelog

## 0.1.0 — 未发布

首个版本。

- OpenAPI 3 契约（76 个操作）生成 Gin strict server；`/openapi.json` 自描述；RFC 9457 错误。
- 书库：partialMD5 内容寻址，EPUB/PDF/CBZ/CBR/TXT 等入库，内嵌元数据解析，封面，键集分页列表、书单、标签、阅读状态；多用户共享书库，书投影进每个用户的 Readest 书记录。
- Readest 云契约：`/api/sync`（移植 v0.12.20 合并规则）、`/api/sync/replicas`、`/api/sync/replica-keys`、`/api/storage/*`；GoTrue 子集供 `readest.koplugin` 登录。
- KOSync 协议，设备密钥登录，EPUB 位置与 Readest 互通。
- OPDS 1.2 目录。
- 阅读统计聚合：汇总、按天、按书、连续天数。
- 元数据刮削：Google Books、Open Library、Hardcover、小说汇、豆瓣（默认关闭）；精确匹配的后台补缺，配额暂停，字段锁定。
- Readest Web 版跨域代理（OPDS、KOSync、BookOrbit、Grimmory、Hardcover、Notion），拨号时 SSRF 防护。
- CLI：`serve`、`import`、`user add|passwd|list`、`backup`；Dockerfile；CI。
