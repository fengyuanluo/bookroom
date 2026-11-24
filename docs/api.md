# API 指南

完整契约在 [`api/openapi.yaml`](../api/openapi.yaml)，运行中的服务也在 `GET /openapi.json` 提供同一份。本文讲约定、鉴权和常见用法，末尾是全部端点索引（由规格生成）。

## 五个协议族

| 协议族 | 前缀 | 鉴权 | 错误格式 | 给谁用 |
|---|---|---|---|---|
| 原生 API | `/api/v1` | `Authorization: Bearer <access token>` | RFC 9457 `application/problem+json` | Agent、脚本、客户端 fork 的 BookRoom 专属界面 |
| Readest 云契约 | `/api/sync` `/api/storage` | Bearer | `{"error": "…"}`（副本接口另带 `code`） | Readest 数据层、`readest.koplugin` |
| GoTrue 子集 | `/auth/v1` | 密码 / 刷新令牌 | `{"error", "error_description"}` | `readest.koplugin` 登录 |
| KOSync | `/kosync` | `x-auth-user` + `x-auth-key`（设备密钥的 MD5） | `{"code", "message"}` | KOReader、CrossPoint 等 |
| OPDS 1.2 | `/opds` | HTTP Basic（账号密码或设备密钥） | 纯文本 + `WWW-Authenticate` | 任何 OPDS 阅读器 |
| 跨域代理 | `/api/opds/proxy` `/api/kosync` … | Bearer 或 `?access_token=` | `{"error", "details"}` | Readest Web 版 |

Readest 兼容端点保持 Readest 自己的线格式（snake_case、ISO 时间串、`null` 照 PostgREST 输出），因为未修改的 Readest 代码在调用它们；细节和差异见 [compatibility.md](compatibility.md)。

## 原生 API 约定

- **书的身份**：`hash`，文件内容的 partialMD5，32 位小写十六进制，与 Readest、KOReader 计算的值相同。同一作品的 EPUB 和 PDF 是两本书。
- **书单**：`state.shelf` 是 `/` 分隔的路径；一本书在每个用户那里至多属于一个书单。书单之外的维度用 `tags`。
- **分页**：列表返回 `nextCursor`，原样作为下一次的 `cursor`；没有 `nextCursor` 即最后一页。游标与排序方式绑定，换排序要从头开始。`total` 是匹配总数。
- **锁定**：任何手工写入的元数据字段（`PATCH /api/v1/books/{hash}` 或应用候选）自动锁定，后台刮削不再改它；`PUT …/metadata/locks` 可解锁。
- **校验**：请求先按规格校验（类型、枚举、长度、模式、必填），失败返回 400，`detail` 指出字段。
- **时间**：JSON 里是 RFC 3339 UTC。

### 错误

```json
{
  "type": "https://bookroom.dev/problems/not-found",
  "title": "Not Found",
  "status": 404,
  "detail": "no such book",
  "instance": "/api/v1/books/0123…"
}
```

`type` 的末段是稳定代码：`bad-request`、`invalid-request`（规格校验失败）、`unauthorized`、`forbidden`、`not-found`、`conflict`、`too-large`、`unsupported-format`、`upstream`（元数据源失败）、`unavailable`、`internal`。

## 鉴权

```bash
# 登录：返回 accessToken（默认 1 小时）与 refreshToken（默认 30 天，用一次即作废）
curl -s localhost:8080/api/v1/auth/login -H 'content-type: application/json' \
  -d '{"username":"admin","password":"…"}'

# 续期
curl -s localhost:8080/api/v1/auth/refresh -H 'content-type: application/json' -d '{"refreshToken":"…"}'

# 设备密钥：给 KOSync / OPDS 客户端当密码，只显示一次，可单独吊销
curl -s localhost:8080/api/v1/devices -H "Authorization: Bearer $TOKEN" \
  -H 'content-type: application/json' -d '{"name":"Kobo Libra"}'
```

用户名不区分大小写，邮箱也可用于登录。管理员专属操作（规格里标 `bearerAuth: [admin]`）：账号管理、从书库删书、修改共享元数据。

## 常见用法

```bash
H="Authorization: Bearer $TOKEN"

# 上传（同一文件重复上传返回 200 和已有的书），顺带放进书单
curl -s localhost:8080/api/v1/books -H "$H" -F file=@三体.epub -F shelf=小说/科幻

# 搜索：书名、作者、丛书名的子串，单个汉字也能命中
curl -s 'localhost:8080/api/v1/books?q=体&sort=title&order=asc&limit=20' -H "$H"

# 书单内（含子书单）
curl -s 'localhost:8080/api/v1/books?shelf=小说&recursive=true' -H "$H"

# 设阅读状态 / 移出书单（null 清除，缺省字段不变）
curl -s -X PUT localhost:8080/api/v1/books/$HASH/state -H "$H" \
  -H 'content-type: application/json' -d '{"readingStatus":"finished","shelf":null}'

# 刮削：先看候选，再应用（所选字段写入并锁定）
curl -s -X POST localhost:8080/api/v1/books/$HASH/metadata/search -H "$H" -H 'content-type: application/json' -d '{}'
curl -s -X POST localhost:8080/api/v1/books/$HASH/metadata/apply -H "$H" -H 'content-type: application/json' \
  -d '{"provider":"openlibrary","providerId":"isbn:9780765382030","fields":["description","cover"]}'

# 阅读时长：本月汇总、按天、按书
curl -s 'localhost:8080/api/v1/stats/summary?from=2026-11-01&tz=Asia/Shanghai' -H "$H"
curl -s 'localhost:8080/api/v1/stats/daily?tz=Asia/Shanghai' -H "$H"
curl -s 'localhost:8080/api/v1/stats/books?limit=10' -H "$H"
```

## 给 Agent 的提示

- 读 `/openapi.json`；每个操作都有稳定的 `operationId`、`summary` 和 `description`，可直接映射成工具。
- 先 `GET /api/v1/metadata/providers` 看哪些元数据源可用（`enabled`、`reason`、`pausedUntil`）。
- 批量整理书单用 `PUT /api/v1/books/{hash}/state` 和 `POST /api/v1/shelves/rename`；它们写的就是 Readest 的分组，客户端下次同步即可见。
- 不要调用 Readest 兼容端点做整理工作：它们的合并规则依赖客户端时钟，原生端点会替你盖好时间戳。

## 端点索引

### system

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/healthz` | `getHealth` | — | Liveness probe |
| GET | `/openapi.json` | `getOpenAPI` | — | This document as JSON |

### auth

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| POST | `/api/v1/auth/login` | `login` | — | Exchange username and password for a token pair |
| POST | `/api/v1/auth/refresh` | `refreshToken` | — | Rotate a refresh token |
| POST | `/api/v1/auth/logout` | `logout` | — | Revoke a refresh token |
| GET | `/api/v1/me` | `getMe` | bearerAuth | The authenticated user |
| PUT | `/api/v1/me/password` | `changePassword` | bearerAuth | Change the current user's password |

### users

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/api/v1/users` | `listUsers` | bearerAuth[admin] | List accounts |
| POST | `/api/v1/users` | `createUser` | bearerAuth[admin] | Create an account |
| DELETE | `/api/v1/users/{userId}` | `deleteUser` | bearerAuth[admin] | Delete an account and all of its sync data |
| PUT | `/api/v1/users/{userId}/password` | `resetUserPassword` | bearerAuth[admin] | Set another account's password |

### devices

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/api/v1/devices` | `listDevices` | bearerAuth | List the current user's device keys |
| POST | `/api/v1/devices` | `createDevice` | bearerAuth | Issue a device key |
| DELETE | `/api/v1/devices/{deviceId}` | `deleteDevice` | bearerAuth | Revoke a device key |

### books

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/api/v1/books` | `listBooks` | bearerAuth | List and search the library |
| POST | `/api/v1/books` | `uploadBook` | bearerAuth | Ingest a book file |
| GET | `/api/v1/books/{hash}` | `getBook` | bearerAuth | One book with the current user's state |
| PATCH | `/api/v1/books/{hash}` | `updateBookMetadata` | bearerAuth[admin] | Edit library metadata |
| DELETE | `/api/v1/books/{hash}` | `deleteBook` | bearerAuth[admin] | Remove a book from the library |
| GET | `/api/v1/books/{hash}/file` | `downloadBook` | bearerAuth | Download the book file |
| GET | `/api/v1/books/{hash}/cover` | `getBookCover` | bearerAuth | The cover image |
| PUT | `/api/v1/books/{hash}/state` | `updateBookState` | bearerAuth | Set the current user's shelf or reading status of a book |

### shelves

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/api/v1/shelves` | `listShelves` | bearerAuth | The current user's shelf tree, flattened |
| POST | `/api/v1/shelves/rename` | `renameShelf` | bearerAuth | Rename or move a shelf with all sub-shelves |
| GET | `/api/v1/tags` | `listTags` | bearerAuth | Library tags with book counts |

### metadata

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| PUT | `/api/v1/books/{hash}/metadata/locks` | `setMetadataLocks` | bearerAuth[admin] | Replace the set of locked metadata fields |
| POST | `/api/v1/books/{hash}/metadata/search` | `searchBookMetadata` | bearerAuth | Search metadata sources for candidates |
| POST | `/api/v1/books/{hash}/metadata/apply` | `applyBookMetadata` | bearerAuth[admin] | Apply a candidate to the book |
| POST | `/api/v1/books/{hash}/metadata/refresh` | `refreshBookMetadata` | bearerAuth | Queue automatic gap filling |
| GET | `/api/v1/metadata/providers` | `listMetadataProviders` | bearerAuth | Configured metadata sources |

### stats

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/api/v1/stats/summary` | `getStatsSummary` | bearerAuth | Reading time totals and streaks |
| GET | `/api/v1/stats/daily` | `getStatsDaily` | bearerAuth | Reading time per calendar day |
| GET | `/api/v1/stats/books` | `getStatsBooks` | bearerAuth | Reading time per book |

### readest-sync

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/api/sync` | `readestPull` | bearerAuth | Pull rows changed since a timestamp |
| POST | `/api/sync` | `readestPush` | bearerAuth | Push local changes and get the authoritative rows back |
| GET | `/api/sync/replicas` | `readestPullReplicas` | bearerAuth | Pull replica rows of one kind after an HLC cursor |
| POST | `/api/sync/replicas` | `readestPostReplicas` | bearerAuth | Push replica rows, or batch-pull several kinds |
| GET | `/api/sync/replica-keys` | `readestListReplicaKeys` | bearerAuth | List the user's encryption salts |
| POST | `/api/sync/replica-keys` | `readestCreateReplicaKey` | bearerAuth | Create an encryption salt |
| DELETE | `/api/sync/replica-keys` | `readestForgetReplicaKeys` | bearerAuth | Forget every salt and the encrypted fields that depend on them |

### readest-storage

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| POST | `/api/storage/upload` | `storageUploadUrl` | bearerAuth | Get a signed URL to PUT a file to |
| GET | `/api/storage/download` | `storageDownloadUrl` | bearerAuth | Get a signed download URL for one file |
| POST | `/api/storage/download` | `storageDownloadUrls` | bearerAuth | Get signed download URLs for many files |
| GET | `/api/storage/list` | `storageList` | bearerAuth | List the user's files |
| DELETE | `/api/storage/delete` | `storageDelete` | bearerAuth | Delete one file |
| PUT | `/api/storage/objects/{token}` | `storagePutObject` | — | Upload target of a signed URL |
| GET | `/api/storage/objects/{token}` | `storageGetObject` | — | Download target of a signed URL |

### gotrue

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| POST | `/auth/v1/token` | `gotrueToken` | — | Password or refresh-token grant |
| POST | `/auth/v1/logout` | `gotrueLogout` | bearerAuth | Revoke every refresh token of the user |
| GET | `/auth/v1/user` | `gotrueUser` | bearerAuth | The authenticated user in GoTrue shape |

### kosync

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/kosync/healthcheck` | `kosyncHealthcheck` | — | KOSync health check |
| POST | `/kosync/users/create` | `kosyncCreateUser` | — | Registration (always disabled) |
| GET | `/kosync/users/auth` | `kosyncAuth` | kosyncUser+kosyncKey | Check credentials |
| PUT | `/kosync/syncs/progress` | `kosyncPutProgress` | kosyncUser+kosyncKey | Store the reading position of a document |
| GET | `/kosync/syncs/progress/{document}` | `kosyncGetProgress` | kosyncUser+kosyncKey | The newest position of a document |

### opds

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/opds` | `opdsRoot` | basicAuth | Root navigation feed |
| GET | `/opds/books` | `opdsBooks` | basicAuth | Acquisition feed of library books |
| GET | `/opds/shelves` | `opdsShelf` | basicAuth | One shelf level |
| GET | `/opds/tags` | `opdsTags` | basicAuth | Navigation feed of tags |
| GET | `/opds/search` | `opdsSearch` | basicAuth | Search results as an acquisition feed |
| GET | `/opds/opensearch.xml` | `opdsOpenSearch` | basicAuth | OpenSearch description document |
| GET | `/opds/books/{hash}/file` | `opdsDownload` | basicAuth | Acquisition link target |
| GET | `/opds/books/{hash}/cover` | `opdsCover` | basicAuth | Image and thumbnail link target |

### proxy

| 方法 | 路径 | operationId | 鉴权 | 说明 |
|---|---|---|---|---|
| GET | `/api/opds/proxy` | `proxyOpds` | bearerAuth / accessTokenQuery | Fetch an external OPDS feed, cover or book |
| HEAD | `/api/opds/proxy` | `proxyOpdsHead` | bearerAuth / accessTokenQuery | HEAD variant of the OPDS proxy |
| POST | `/api/kosync` | `proxyKosync` | bearerAuth | Forward a KOSync call to an external server |
| POST | `/api/bookorbit` | `proxyBookorbit` | bearerAuth | Forward a BookOrbit KOReader-plugin call |
| POST | `/api/grimmory` | `proxyGrimmory` | bearerAuth | Forward a Grimmory API call |
| POST | `/api/hardcover/graphql` | `proxyHardcover` | bearerAuth | Forward a Hardcover GraphQL request |
| POST | `/api/notion/{collection}` | `proxyNotion1Post` | bearerAuth | Notion `POST /pages` |
| GET | `/api/notion/{collection}/{id}` | `proxyNotion2Get` | bearerAuth | Notion `GET /users/me`, `/data_sources/{id}`, `/databases/{id}`, `/pages/{id}` |
| PATCH | `/api/notion/{collection}/{id}` | `proxyNotion2Patch` | bearerAuth | Notion `PATCH /pages/{id}` |
| DELETE | `/api/notion/{collection}/{id}` | `proxyNotion2Delete` | bearerAuth | Notion `DELETE /blocks/{id}` |
| GET | `/api/notion/{collection}/{id}/{action}` | `proxyNotion3Get` | bearerAuth | Notion `GET /blocks/{id}/children` |
| POST | `/api/notion/{collection}/{id}/{action}` | `proxyNotion3Post` | bearerAuth | Notion `POST /data_sources/{id}/query` |
| PATCH | `/api/notion/{collection}/{id}/{action}` | `proxyNotion3Patch` | bearerAuth | Notion `PATCH /blocks/{id}/children` |
