# TXT 原样存储，客户端打开时转换

书库里的 TXT 存原文件，书哈希就是这个 TXT 文件的 partialMD5，服务端、KOReader、Readest 三方算出的值相同。服务端可以直接导入整个目录的 TXT，书名和作者从文件名提取，规则照 Readest `utils/txt.ts` 里的 `extractTxtFilenameMetadata`（`《书名》作者：X.txt`、`【书名】… 作者：X.txt` 等）移植到 Go。客户端打开 TXT 时在本机用 `TxtToEpubConverter` 转成 EPUB 来显示；服务端不转换，也不渲染。

Readest 现在的做法是导入时先把 TXT 转成 EPUB，再对 EPUB 算哈希并存下 EPUB（`services/bookService.ts:617-620,742,919-923`）。它的阅读器本来就能直接打开原始 TXT，打开时临时转换（`libs/document.ts:623-627`，Android 的"用其他应用打开"走的就是这条路径），所以 fork 只需要改导入：对原文件算哈希，存原文件。

## 考虑过的方案

- 存转换后的 EPUB（Readest 现状）：客户端不用改，但原文丢失；服务端要导入 TXT，就得把整个转换器移植到 Go；书哈希会跟着转换器的输出变；KOReader 直接读 TXT 时算出的哈希也对不上。
- 服务端用 Go 实现转换：两套转换器对同一个 TXT 输出不同，同一本书会因为入库方式不同变成两本。

## 后果

- 书哈希不再依赖转换器，阅读位置却依赖：Readest 的进度和批注记的是转换后 EPUB 里的位置。改动 `utils/txt.ts` 的章节切分，等于改动所有 TXT 书的位置坐标，要按迁移来处理。章节切分变化后位置会偏移多少，还没验证。
- 每次打开都要重新转换，大文件耗时没测过。太慢的话，按"书哈希 + 转换器版本"把转换结果存进本机缓存。
- KOReader 用自己的引擎直接打开 TXT，它记录位置的方式和 Readest 的 EPUB 位置不同，两者之间的进度大概只能按百分比对齐（推断）。实现时要验证 `readest.koplugin` 收到对方位置后的表现。
- OPDS 以 `text/plain` 提供 TXT。CrossPoint 的 OPDS 客户端只认 EPUB，看不到 TXT 书。
- DOCX、RTF 不变，入库时仍转成 EPUB。
