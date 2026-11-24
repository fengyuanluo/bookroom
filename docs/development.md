# 开发

## 环境

- Go ≥ 1.26（oapi-codegen v2.9 生成的代码需要；`go.mod` 的 `go 1.26`）
- [sqlc](https://sqlc.dev) v1.31.1（`make generate` 用；oapi-codegen 作为 Go tool 依赖，无需单独安装）
- 无 cgo：SQLite 驱动是纯 Go 的 `modernc.org/sqlite`

```bash
make generate   # 规格与 SQL 改动后重新生成
make test       # 全部测试
make check      # CI 同款：vet + gofmt + 生成代码无漂移 + race 测试
make bench      # 两万本书的列表与同步基准
```

## 目录

```
api/openapi.yaml            接口契约（唯一真相源）
api/oapi-codegen.yaml       生成配置
internal/api/               生成的 Gin 路由、类型、strict server 接口（勿手改）
internal/httpapi/           实现 strict server：handlers_*.go、鉴权、错误渲染、流式响应
internal/store/migrations/  goose 迁移（同时是 sqlc 的 schema）
internal/store/queries/     sqlc 查询
internal/store/db/          生成的查询代码（勿手改）
internal/<服务>/             auth library readest kosync opds stats scrape proxy
internal/testutil/          生成测试用 EPUB/CBZ/PNG
cmd/bookroom/               CLI 与依赖装配
docs/                       本文档体系与 ADR
research/                   设计期调研
```

## 加一个端点

1. 在 `api/openapi.yaml` 写好路径、`operationId`、请求响应 schema、`security`（管理员专属用 `bearerAuth: [admin]`，公开用 `security: []`）。只做向后兼容的新增（CI 用 oasdiff 卡破坏性变更）。
2. `make generate`。编译会报 `*Server` 缺少新方法。
3. 在 `internal/httpapi/handlers_*.go` 实现它：取参数 → 调服务 → 返回生成的响应类型。调服务时传 `rc(ctx)`（请求自己的 context）：`*gin.Context` 会被池化复用，不能逃逸到 `database/sql` 或 HTTP 客户端里。当前用户用 `currentUser(ctx)`。
4. 业务放在服务包里；预期内的失败返回 `apperr`（如 `apperr.NotFound("…")`），HTTP 层会按协议族渲染成 problem+json、Readest、KOSync 或 OPDS 的格式。需要特定协议的错误体时用 `apperr.WithBody`。
5. 返回文件用 `fileResponse`（自带 Range、ETag、条件请求），返回上游原样内容用 `passthrough`。
6. 写集成测试（见下文）。

原生 API（`/api/v1/*`）的请求会先经过规格校验中间件，handler 拿到的参数已满足 schema 约束；其他协议族为了兼容客户端的宽松输入不做规格校验，handler 自己检查。

## 加一张表或一个查询

- 新迁移：`internal/store/migrations/0000N_描述.sql`，带 `-- +goose Up` / `-- +goose Down`。启动时自动执行，迁移文件嵌入二进制。
- 新查询：写进 `internal/store/queries/*.sql`（`-- name: X :one|:many|:exec|:execrows`），同一条语句里参数要么全用 `?`、要么全用 `sqlc.arg()`，混用会让 sqlc 生成错位的编号。
- 时间存 UTC 毫秒整数；不要用 `INSERT OR REPLACE`；SQLite 专有语法的现有例外见 ADR 0003，新增要同步记录。
- 写操作用 `store.Tx`（单写连接，`BEGIN IMMEDIATE`）或 `st.WQ`；读用 `st.Q` / `st.R`。
- 动态 SQL（列表过滤）集中在 `library/query.go`、`readest/storage.go`，一律参数化。

## 测试

| 层 | 位置 | 做法 |
|---|---|---|
| 纯逻辑 | `library/library_test.go`、`readest/merge_test.go`、`scrape/scrape_test.go`、`proxy/proxy_test.go` | partialMD5 金向量（含 1 GiB 稀疏文件的 12 段采样）、ISBN、文件名解析、Readest 合并规则、HLC 字段合并、元数据源解析（`httptest` 假上游）、SSRF 策略 |
| 端到端 | `internal/httpapi/*_test.go` | `newEnv` 起一个完整服务（临时数据目录、真 SQLite、真路由），用 HTTP 调用；外部服务用 `httptest` 假冒并通过 `envOptions` 注入 |
| CLI | `cmd/bookroom/main_test.go` | 用环境变量驱动 `run()` |
| 基准 | `library/bench_test.go`、`readest/bench_test.go` | 两万本书的列表、书单、同步拉取与推送 |

测试里 Argon2 参数调低（`auth.HashParams`），整套 race 测试约 25 秒。新测试应能捕捉用户可见的错误；不要测试生成代码或装配。

## 约定

- 代码注释与标识符用英文，文档与 ADR 用中文；术语以 `GLOSSARY.md` 为准。
- Go 1.26 风格：`new(expr)` 取值指针、`for range n`、`min`/`max`、`cmp.Or`。
- 生成代码随提交入库；`make check-generated` 保证它与规格和 SQL 一致。
- 改变架构的取舍写 ADR（`docs/adr/NNNN-slug.md`，一段话说清背景、决定、原因）。
