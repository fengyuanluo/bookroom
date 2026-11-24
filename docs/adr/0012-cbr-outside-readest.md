# CBR 书只进书库，不进 Readest 书库

Readest 的格式表（`libs/document.ts` 的 `EXTS`）没有 CBR，foliate-js 也读不了 RAR。所以 CBR 书只出现在原生 API 和 OPDS 里，不投影进任何用户的 Readest 书记录；没有书记录就没有书单和阅读状态，`PUT /api/v1/books/{hash}/state` 对 CBR 返回 409，上传时附带的书单被忽略。KOReader 等 OPDS 阅读器照常从目录下载 CBR。

## 考虑过的方案

- 照常投影：Readest 会显示一本下载后打不开的书（文件名扩展名变成 `undefined`）。
- 入库时把 CBR 转成 CBZ：文件字节变了，书哈希和 KOReader 算出的对不上，违背 ADR 0002。

## 后果

- 客户端 fork 以后若支持 CBR，把 `library.ReadestCanOpen` 改为放行，再对现有用户补一次投影即可。
