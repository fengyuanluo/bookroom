# 客户端兼容性与接入

BookRoom 实现了四个外部契约。本文写清每个契约以哪个版本为准、怎么接入、和原版有哪些有意的差异。

## Readest 云契约

基准：Readest v0.12.20（`apps/readest-app/src/pages/api/sync.ts`、`pages/api/sync/replicas.ts`、`pages/api/sync/replica-keys.ts`、`pages/api/storage/*`、`libs/replicaSyncServer.ts`、`libs/crdt.ts` 与 Supabase 迁移 `004/005/008/010/019`）。合并逻辑逐函数移植到 `internal/readest/merge.go`，函数名保持一致以便对照。

| 端点 | 状态 | 说明 |
|---|---|---|
| `GET /api/sync` | 完全兼容 | `since` 毫秒、`type`、`book`、`meta_hash`、`limit`；空的首次拉取同样带一条已删除的占位书（Readest 依赖它规避初始同步竞态）；统计行附 `updated_at_ms`/`deleted_at_ms` |
| `POST /api/sync` | 完全兼容 | 书、配置、批注、`statBooks`、`statPages`；返回权威行 |
| `GET/POST /api/sync/replicas` | 兼容 | 推送、单类拉取、`cursors` 批量拉取；错误码 `AUTH/VALIDATION/UNKNOWN_KIND/SCHEMA_TOO_NEW/CLOCK_SKEW/SERVER`；HLC 时钟偏差 ±60 s |
| `GET/POST/DELETE /api/sync/replica-keys` | 兼容 | 仅 `pbkdf2-600k-sha256`；遗忘时同时删除依赖这些盐的加密字段 |
| `GET/POST /api/sync/vocabulary` | 完全兼容 | 生词本：`since_ts`/`since_id` 键集分页（`updated_at` 服务端毫秒戳）；推送按 Readest `mergeEntries` 合并（`modified_at` 新者赢状态/提示/墓碑，例句并集，删除抹掉之前的例句）；格式错误的行跳过并在 `rejected` 里返回；每次最多 100 行 |
| `POST /api/storage/upload` | 兼容 | 返回签名 PUT URL（`/api/storage/objects/<token>`，30 分钟） |
| `GET/POST /api/storage/download` | 兼容 | 包括 Readest 对未编码 `&`、`+` 的容错，以及"按书哈希 + 扩展名"的回退匹配 |
| `GET /api/storage/list`、`DELETE /api/storage/delete` | 兼容 | 见下文差异 |

有意的差异：

- **书文件进共享书库。** `Readest/Books/<哈希>/<名>.<书的扩展名>` 上传完成时服务端校验内容的 partialMD5 与路径里的哈希一致，然后入库（ADR 0013）；下载这类键直接指向书库文件，任何用户都能下载书库里的书。删除这类键等于从书库删书，仅管理员可做。`cover.png` 若书库里的书还没有封面就被采用为书库封面（Readest 为 PDF 渲染的封面就是这样来的）。
- **不计配额。** `usage` 为实际占用，`quota` 固定为 2⁵⁰。
- **`temp`/`media` 上传** 存在用户私有区，返回 72 小时有效的签名下载地址，而不是公共 CDN。
- **没有统计冷归档。** 所有翻页事件都在库里，响应格式不变。
- **副本字段只校验信封。** 每个字段必须是 `{v,t,s}` 或密文信封，字段数 ≤64、总大小 ≤64 KiB、清单文件名合法；各类副本内部字段的形状不校验（由客户端负责），每用户行数上限不强制。
- **鉴权失败** 与 Readest 一样返回 403 `{"error":"Not authenticated"}`（副本接口为 401 `code: AUTH`）。
- **未实现：** CrossPoint 设备码绑定（`/api/crosspoint/*`，改用原生 KOSync + 设备密钥）、分享链接、邮件投递、Readest Voice、翻译配额路由、支付（ADR 0009 中删除的功能）。
- **CBR 不出现在 Readest 书库里**（ADR 0012）。

### 接入 Readest fork

- API 基址：`NEXT_PUBLIC_API_BASE_URL=https://书库地址`（Readest 自动加 `/api`）。
- 登录：把 Supabase 登录换成 `POST /api/v1/auth/login`（或沿用 GoTrue 形状的 `POST /auth/v1/token?grant_type=password`），令牌放进现有的 `getAccessToken()`；用户 ID 取响应里的 `user.id`，存储键前缀是它。
- 签名 URL 由 `BOOKROOM_PUBLIC_URL`（未设置时取请求的 Host 与 `X-Forwarded-*`）拼成，客户端必须能访问这个地址。
- Web 版的跨域代理改为调用 BookRoom，并带上 `Authorization: Bearer`：
  - OPDS：`/api/opds/proxy?url=…`，上游凭据放 `auth` 参数或 `X-Upstream-Authorization` 头；`<img>` 里用 `&access_token=`。
  - KOSync / BookOrbit / Grimmory：请求体与 Readest 原路由相同。
  - Hardcover：Hardcover 令牌改放 `X-Upstream-Authorization`（`Authorization` 已用于 BookRoom）。
  - Notion：与原路由相同，`x-notion-token` 头。

## readest.koplugin

插件用 GoTrue 接口登录，再调用同一套 `/api/sync` 与 `/api/storage`，进度、批注、阅读统计都能同步。

1. 设置项 `supabase_url` 改成书库地址（插件会访问 `<地址>/auth/v1/token`；`supabase_anon_key` 随便填，服务端忽略 `apikey` 头）。
2. `readest-sync-api.json` 里的 `base_url` 改成 `<书库地址>/api`。
3. 用 BookRoom 用户名（或邮箱）和密码登录。

"用代码登录"（`requestDeviceCode`/`pollDeviceToken`）未实现，用密码登录即可。插件自己生成的路径里有双斜杠（`/auth/v1//token`），服务端已兼容。

## KOSync（KOReader、CrossPoint 等）

基准：`koreader/koreader-sync-server` 与 `pid1/kosync-conformance` SPEC v1。七个端点中实现了 `healthcheck`、`users/create`（固定返回 402 / 2005，注册关闭）、`users/auth`、`PUT syncs/progress`、`GET syncs/progress/:document`。

- 服务器：`<书库地址>/kosync`。
- 用户名：任意（建议填 BookRoom 用户名）；密码：在 BookRoom 生成的**设备密钥**。KOReader 发送的是密码的 MD5，服务端只存它的 SHA-256。
- 文档识别：推荐 Binary（partialMD5）模式，直接对上书库的书哈希；Filename 模式也能用，服务端按原始文件名的 MD5 关联到书库里的书。
- 与 Readest 互通：EPUB 的 XPointer 会写进该用户的 Readest 书配置；`GET` 返回 KOSync 与 Readest 两边较新的位置（来自 Readest 时 `device` 为 `Readest`）。PDF、CBZ 的页码只在 KOSync 内部同步。
- 兼容细节：未知文档返回 `200 {}`；`timestamp` 为服务端秒；`progress` 原样以字符串存取；不要求 `Accept` 头；`document` 不能含 `:`、`/` 和空白。
- CrossPoint：把校验方式改为 Binary；它的 OPDS 客户端只认 EPUB。

## OPDS 1.2

- 地址：`<书库地址>/opds`，HTTP Basic，用户名 + 账号密码或设备密钥（推荐设备密钥，便于单独吊销）。
- 根目录：最近添加、按书名、在读、书单（逐级导航，书单内先列子书单再列书）、标签、OpenSearch 搜索。
- 每页 25 条，带 `first/last/previous/next` 与 `opensearch:totalResults`。
- 获取链接 `http://opds-spec.org/acquisition` 指向原文件（支持 Range），封面同时给出 `image` 与 `image/thumbnail`。
- TXT 以 `text/plain` 提供；未实现 OPDS 2.0 与 OPDS-PSE（ADR 0001）。
