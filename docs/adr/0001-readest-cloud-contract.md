# 服务端兼容 Readest 云契约，不自定义同步协议

服务端是 Readest 云的自托管替身加一个共享书库：实现 Readest 现有的 `/api/sync`（books、notes、configs、statBooks、statPages）、`/api/sync/replicas`（客户端配置与智能书架）、`/api/sync/vocabulary`（生词本）与 `/api/storage`（upload、download、list、delete）线格式，外加 OPDS 1.2 和原生 KOSync。Readest 已经有文件夹首屏、书籍详情、按需下载、缓存清理；KOReader 通过 `readest.koplugin` 走同一套端点（`apps/readest.koplugin/readest-sync-api.json`）。兼容这套契约后，客户端数据层不用改，KOReader 的进度、批注、统计同步也能直接用。没装插件的 KOReader 和 CrossPoint 这类自带 KOSync 客户端的设备，用原生 KOSync 同步进度。

## 考虑过的方案

- 自定义 REST：服务端模型更自由，但要重写客户端同步层，KOReader 也只剩进度同步。
- OPDS 2.0 与 OPDS-PSE：目标阅读器都支持 1.2，Readest 下载整本 CBZ，不实现。

## 后果

- 表结构跟着 Readest 线格式走：书以书哈希为键，冲突规则沿用 Readest 的整行 LWW 加字段级时钟（`readingStatusUpdatedAt`、`coverUpdatedAt`、`metadataUpdatedAt`、`groupUpdatedAt`），批注时间戳相同时以墓碑为准。
- 合并规则以 Readest v0.12.20 的 `pages/api/sync.ts` 为准；动手实现前要通读该文件（调研只读到导出的合并函数）。
- Readest 升级时如果改了契约，需要跟进；用 `readest-sync-api.json` 写契约测试来发现漂移。
- `/api/sync/replicas` 承载客户端配置同步：设置、字体、主题纹理、词典、自定义 TTS 和翻译器、OPDS 书库列表、智能书架（`services/sync/adapters/*`），按 HLC 做字段级 LWW。服务端逻辑照 `libs/replicaSyncServer.ts` 和 `libs/crdt.ts` 移植；客户端配置只同步到 BookRoom（见 ADR 0010）。
- 原生 KOSync 的进度和 `/api/sync` 写同一条书配置（`xpointer` 加 `progress`），Readest 打开书时会应用这个 xpointer。逻辑照 Readest 的 `app/api/crosspoint/syncs/progress/*` 移植，包括它的防回退规则：服务端还没有 xpointer 时，新设备上传的较小进度不能覆盖 Readest 已有的较大进度。
- KOSync 协议把密码的 MD5 当作 `x-auth-key` 发送。服务端不存用户密码的 MD5，而是像 Readest 的 CrossPoint 那样，给每台设备签发一个设备密钥填在 KOSync 密码处（OPDS 的 Basic 认证同样接受）：密钥本身就能识别用户，用户名只作显示，可单独吊销。注册接口关闭，账号由 BookRoom 管理。
- CrossPoint 默认按文件名匹配书，要改成 Binary（partialMD5）模式才能对上书哈希；它的 OPDS 客户端只认 `application/epub+zip`。
