# 客户端连接多个互相隔离的远程

客户端可以连接多个远程：BookRoom、网盘（WebDAV、GDrive、S3 等）、OPDS 书库（可搭配 KOSync）、Grimmory、BookOrbit 以及本机。一本书只属于一个远程，它的书单、进度、批注都只同步回所属远程；客户端只消费，不提供跨远程转移。首屏显示当前远程的书单，当前远程在设置中切换。客户端配置只同步到 BookRoom（ADR 0001）。

Readest 现在的做法正相反：只有一个本地书库，同时镜像到所有已启用的后端（`services/sync/cloudSyncProvider.ts:174`），从 OPDS 下载的书也会并入同一个书库（`services/opds/autoDownload.ts:144`）。这样同一本书的数据可能散落在几个后端，以哪个为准要每个功能单独决定。隔离之后不会出现跨远程冲突，各远程按自己的能力提供功能：

| 远程 | 书单 | 进度 | 批注 | 阅读统计 |
|---|---|---|---|---|
| BookRoom | 可编辑 | ✓ | ✓ | ✓ |
| 网盘 | 可编辑（`library.json` 中的 `groupId`） | ✓ | ✓ | 只存本机 |
| OPDS 书库 | 导航层级，只读 | 搭配了 KOSync 时 ✓ | 只存本机 | 只存本机 |
| Grimmory | 书库列表（`/api/v1/books`） | ✓ | ✓ | 推送到服务端 |
| BookOrbit | OPDS 导航层级 | ✓ | ✓ | 推送到服务端 |
| 本机 | 可编辑 | — | — | 只存本机 |

## 考虑过的方案

- BookRoom 专用客户端：功能最完整，但用不了已有的 Komga、Kavita 或网盘书库。
- 保持 Readest 现状（一个书库镜像到所有后端）：不用新写代码，但会有多个真相源。
- 支持"存到 BookRoom"这类跨远程转移：破坏隔离，不做。

## 后果

- 客户端要新增"所属远程"维度：书记录带上远程标识，同步按远程分发，首屏按当前远程过滤。这是 fork 里最大的一块新代码。
- 添加 OPDS 远程时可以顺带填一个 KOSync 地址（Komga 和 Kavita 同时提供两者），这本书的进度就走这个 KOSync。
- Readest 的 Grimmory（`services/grimmory/`）和 BookOrbit（`services/bookorbit/`）连接器原本是给本地书库里的书配对远程进度：Grimmory 按文件大小加 partialMD5 匹配（`bookMatch.ts`），BookOrbit 走它的 KOReader 插件 API。要改造成"从该远程浏览书、同步回该远程"。
- 只有 BookRoom 能同步客户端配置，也只有它能跨设备查看阅读统计；Grimmory 和 BookOrbit 只接收统计推送（`statsPush.ts`）。
- Web 版连 OPDS、KOSync、Grimmory、BookOrbit 要经 BookRoom 代理（ADR 0009），所以没登录 BookRoom 的 Web 版用不了这几类远程。
