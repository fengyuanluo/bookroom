# GoStack — Go + Gin + SQLite server library research

**Research date: 2026-11-23. All versions below verified from pkg.go.dev (published timestamps Nov 2026 unless noted) or project docs; no Go toolchain on this box (`which go` → absent), nothing compiled or benchmarked locally. Performance numbers are third-party, cited.**

## 1. Chosen libraries (table)

| Concern | Module path | Latest version (date) | License | Why |
|---|---|---|---|---|
| HTTP framework | `github.com/gin-gonic/gin` | v1.12.0 (2026-02-28) | MIT | User constraint; mature router, huge middleware ecosystem. v1.12 needs Go 1.25+ ([oapi-codegen gin doc](https://github.com/oapi-codegen/oapi-codegen/blob/main/docs/gin-server.md)). |
| SQLite driver | `modernc.org/sqlite` | v1.63.0 (2026-11-22) | BSD-3-Clause | **Pure Go, zero cgo** ⇒ single static binary, trivial cross-compile, race detector. FTS5 compiled in by default (`-DSQLITE_ENABLE_FTS5` in generated `lib/sqlite_linux_amd64.go`, [source](https://github.com/modernc-org/sqlite/blob/master/lib/sqlite_linux_amd64.go)); no build tags needed. WAL + `_pragma=busy_timeout(N)` DSN supported. Newest bundled SQLite. |
| Query layer | `github.com/sqlc-dev/sqlc` (dev tool) + `database/sql` | v1.31.1 (2026-04-22) | MIT | "Least magic": typed functions generated directly from SQL; agent can read/edit `.sql` files. Official SQLite tutorial uses modernc driver ([docs](https://docs.sqlc.dev/en/latest/tutorials/getting-started-sqlite.html)). |
| Migrations | `github.com/pressly/goose/v3` | v3.30.1 (2026-11-12) | MIT | `embed.FS` support, SQL + Go migrations, out-of-order, usable as library at startup; pairs with sqlc ([goose+sqlc blog](https://pressly.github.io/goose/blog/2024/goose-sqlc)). |
| JWT | `github.com/golang-jwt/jwt/v5` | v5.3.1 (2026-01-28) | MIT | Standard; HS256/RS256, strict validation, `alg=none` rejected by default ([README](https://github.com/golang-jwt/jwt/blob/main/README.md)). |
| Password hash | `golang.org/x/crypto/argon2` (+`subtle`) | x/crypto v0.59.0 (2026-11-11) | BSD-3-Clause | Argon2id recommended; per-user 16B salt, store params. |
| CORS | `github.com/gin-contrib/cors` | v1.7.11 (2026-11-08) | MIT | Gin-native, allowlist origins for the forked client. |
| Request ID | `github.com/gin-contrib/requestid` | v1.0.10 (2026-11-08) | MIT | X-Request-ID propagation; pair with slog. |
| Structured logging | `github.com/gin-contrib/slog` | v1.2.5 (2026-11-08) | MIT | slog middleware, hides Authorization/Cookie by default, per-status levels, inject request_id ([README](https://github.com/gin-contrib/slog/blob/main/README.md)). Requires Go 1.26+.Alternative: hand-write 20-line middleware (zero dep). |
| Rate limit | `golang.org/x/time/rate` (stdlib-adjacent, in-process) | via x/time latest | BSD-3-Clause | Single-instance limiter by IP/token; no Redis. (`github.com/ulule/limiter/v3` v3.11.2 MIT is the heavier fallback; last release 2023.) |
| OpenAPI-first | `github.com/oapi-codegen/oapi-codegen/v2` | v2.9.0 (2026-11-21) | Apache-2.0 | Spec-first, has explicit `gin-server` generator producing `ServerInterface` + `RegisterHandlers(gin.IRouter, ...)` ([gin doc](https://github.com/oapi-codegen/oapi-codegen/blob/main/docs/gin-server.md)); generated code deliberately simple/verbose; also generates Go clients + strict server. Requires Go 1.26+ for tool *and* generated code. TS client: `openapi-typescript` + `openapi-fetch` in the Readest fork. |
| Task queue | In-process worker pool (`golang.org/x/sync/errgroup` + buffered channel; optional SQLite-backed jobs table for durability) | x/sync latest | BSD-3-Clause | Single-binary constraint; scraping is I/O-bound and restartable. No Redis dependency. |
| Config | `github.com/kelseyhightower/envconfig` | v1.4.0 (2019) | MIT | Struct tags → env vars, zero transitive deps; enough for ~15 knobs. |
| Observability | stdlib `net/http/pprof`, `expvar`, `log/slog` | Go 1.26+ | Go BSD | Bind pprof to **localhost-only separate listener**; expvar for counters. |
| EPUB metadata | `github.com/pirmd/epub` | v0.3.2 (2026-08-29) | BSD | EPUB2 + 3.2 OPF metadata (title/creator/identifiers/series), manifest + resource walking ([README](https://github.com/pirmd/epub/blob/master/README.md)). Use `mathieu-keller/epub-parser/v2` v2.1.6 (MIT) if richer typed OPF structs needed. |
| EPUB TOC / nav | `github.com/ArcadiaLin/go-epub` | v0.1.0 (2026) | check repo | NCX + EPUB3 nav, `FlattenTOC()`, chapters ([README](https://github.com/ArcadiaLin/go-epub/blob/main/README.md)); or hand-parse nav with `encoding/xml`. |
| Cover bytes (EPUB/CBZ) | stdlib `archive/zip` + `encoding/xml` | — | Go BSD | Find `manifest item[properties~='cover-image']`, read via zip; CBZ = first image by name sort. No extra dep. |
| CBR | `github.com/nwaples/rardecode/v2` | v2.4.6 (2026-11-04) | BSD-2-Clause | Pure-Go RAR reader, RAR3/5; no `unrar` cgo/licence issue ([LICENSE](https://github.com/nwaples/rardecode/blob/master/LICENSE)). |
| PDF metadata | `github.com/pdfcpu/pdfcpu` | v0.16.4 (2026-10-27) | Apache-2.0 | Pure Go, `pkg/api` (`api.Info`/`api.PDFInfo`), Go 1.26+ ([api docs](https://pkg.go.dev/github.com/pdfcpu/pdfcpu/pkg/api), [install](https://pdfcpu.io/getting_started/install_api/)). |
| TXT charset | `golang.org/x/text` `encoding` + `github.com/saintfish/chardet` | x/text v0.44.0 (2026-11-09); chardet pseudo v0.0.0-20230101081208-5e3ef4b5456d | BSD-3 / BSD-3 | Detect GB18030/Big5 then `transform.NewReader`; chardet is stale but dependency-free and handles Chinese heuristically. Fallback UTF-8 → GB18030 → Big5. |
| Thumbnails | `github.com/disintegration/imaging` | v1.6.2 (2019) | MIT | Pure Go resize (`imaging.Fit/Lanczos`), no cgo. **WebP encoding not supported by stdlib/imaging** — pair with `github.com/chai2010/webp` (cgo) or emit JPEG/PNG, or accept govips. |
| Optional fast WebP | `github.com/davidbyttow/govips/v2` | v2.20.0 (2026-10-16) | MIT | 4–8× faster, native WebP export, **requires cgo + libvips 8.14+** ([README](https://github.com/davidbyttow/govips/blob/master/README.md)) — violates zero-cgo; avoid unless profiling demands it. |

## 2. Rejected and why (table)

| Candidate | Reason |
|---|---|
| `github.com/mattn/go-sqlite3` v1.14.53 (MIT) | **cgo required** (`CGO_ENABLED=1` + gcc); cross-compiles silently produce a stub driver failing at first query ([benchmark/article](https://importstatic.com/databases/go-sqlite-driver-modernc-vs-mattn)); ~1.4–1.6× faster point lookups/bulk inserts but irrelevant at 10–20k books. Had high-severity bundled-SQLite CVEs fixed only in v1.14.53 ([advisory GHSA-mm5h-fpwx-37fx](https://github.com/mattn/go-sqlite3/security/advisories/GHSA-mm5h-fpwx-37fx)). |
| `zombiezen.com/go/sqlite` v1.4.2 (ISC) | Pure Go + FTS5 ([doc.go](https://github.com/zombiezen/go-sqlite/blob/main/doc.go)) but **not a `database/sql` driver**, incompatible with sqlc/database-sql; low-level C-style API. Last release 2025-05. |
| `gorm.io/gorm` v1.31.2 / `entgo.io/ent` v0.14.6 | ORM magic, hidden queries, harder for an agent to reason about/perf-tune; ent generates large code graph. |
| `github.com/jmoiron/sqlx` v1.4.0 | Useful but stringly-typed SQL, no compile-time column checking; sqlc strictly better. |
| `ariga.io/atlas` v1.4.0 (Apache-2.0) | Declarative schema diffing is powerful but overkill for single-DB server; more concepts/CLI surface than goose. |
| `github.com/golang-migrate/migrate/v4` v4.20.1 | Two files per version (up/down), more ceremony; embed supported but goose simpler. |
| `github.com/danielgtaylor/huma/v2` v2.41.0 | **Premise correction:** huma is router-agnostic (BYOR) and has an official `humagin` adapter ([docs](https://huma.rocks/features/bring-your-own-router/)) — so Gin-compatible. Still rejected: spec is *generated from Go structs* (code-first), inverts the requested OpenAPI-first contract and requires rewriting handlers to huma types. |
| `github.com/go-swagger/go-swagger` v0.36.8 (Apache-2.0) | Spec-first but heavyweight, Swagger 2.0 heritage, verbose generated server. |
| `github.com/appleboy/gin-jwt/v2` v2.10.3 | Batteries-included but opinionated cookie/store design; plain jwt/v5 middleware is ~30 LOC and easier to adapt to refresh tokens. |
| `github.com/hibiken/asynq` v0.26.0 (MIT) | Requires Redis — breaks single-binary constraint. |
| `github.com/spf13/viper` v1.21.0 (MIT) | Large dep graph (fsnotify, YAML/TOML…); envconfig suffices. |
| `github.com/unidoc/unipdf/v4` v4.11.0 | **AGPL or per-developer commercial license key required**, every call errors without key ([setup docs](https://docs.unidoc.io/unipdf/faq/setup/)). |
| `github.com/davidbyttow/govips/v2` | cgo + libvips 8.14+ runtime dependency; conflicts with zero-cgo/portable-binary goal. |
| `github.com/bbeevik/go-epub`-style writers | Write-oriented, not needed; use read-only parsers above. |

## 3. Answers to specific questions

**Gin middleware stack order:** recovery → slog → requestid → cors → JWT (on protected groups only) → rate limit → handlers. Use `gin.New()` (not `gin.Default()`) to control the chain.

**SQLite concurrency:** single writer, many readers. `db.SetMaxOpenConns(1)` for writes or a dedicated write connection; WAL + `busy_timeout=5000`; keep read transactions short; use `_pragma=foreign_keys(1)` and `_txlock=immediate` for read-modify-write to avoid `database is locked` ([analysis](https://importstatic.com/databases/sqlite-database-is-locked-go)). At 10–20k books FTS5 handles search; external SQLite file with WAL sharing is unnecessary if only one process.

**sqlc workflow (concrete):**
1. `go get -tool github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1` and `go get modernc.org/sqlite`.
2. `sqlc.yaml` v2: `sql: [{engine: sqlite, queries: query.sql, schema: schema.sql, gen: {go: {package: db, out: internal/db}}}]`.
3. Put **authoritative** DDL in `schema.sql` (keep goose migrations in sync — either generate schema from migrations or accept duplication; goose+sqlc integration blog above).
4. Named queries `-- name: GetBook :one` / `:many` / `:exec` with `?` placeholders.
5. `//go:generate go tool sqlc generate` → `db.go`, `models.go`, `query.sql.go`; commit generated code; `queries := db.New(sqlDB)`; call typed methods with `context.Context`.

**Covers on disk:** `covers/<sha256-of-book-id>/{large.jpg,medium.jpg,small.webp}` — or simpler `covers/<bookID>-<width>.webp`; write temp file + rename for atomicity; serve via `http.ServeContent` with `ETag: "<sha256+size>"` and honor `If-None-Match` → 304; `Cache-Control: public, max-age=31536000, immutable` for content-hashed names.

**Auth design:** access JWT 15 min (HS256 with 32-byte secret from env), refresh token 256-bit random stored hashed in SQLite with expiry/rotation + revocation on reuse; argon2id m=64MiB,t=3,p=2 (tune to ~100ms); reset tokens single-use hashed. Bind JWT claims to user ID; kosync uses its own Basic-Auth-derived token (see Kosync peer).

**OpenAPI layout:** hand-write `openapi.yaml` (3.1) as source of truth → oapi-codegen `gin-server` + `client` + `types` → Go; `openapi-typescript` + `openapi-fetch` → TS in Readest fork. Add ` spectral`/`oapi-codegen validate` to CI. Note: OAPI-generated structs don't automatically validate bodies — add `github.com/oapi-codegen/runtime` validation middleware or a validator.

**License flag:** all chosen deps are permissive (MIT/BSD/Apache-2.0). **No chosen dependency requires cgo**: modernc sqlite, imaging, stdlib zip/rar reader, pdfcpu are pure Go. The only cgo option (govips) was rejected; the only AGPL option (unipdf) was rejected.

## 4. Confidence / caveats

- `[INFERENCE]` `github.com/chai2010/webp` as WebP encoder pairing for imaging not verified this session; if WebP required, use govips behind a build tag or defer to client-side conversion.
- `[INFERENCE]` ArcadiaLin/go-epub license not confirmed from pkg.go.dev (no tagged release found); verify before adoption or hand-parse EPUB3 nav XML.
- Third-party benchmarks (importstatic Oct 2026, llimllib 2024/2025) are single-machine and illustrative, not a local measurement.
- Versions are point-in-time (2026-11-23); pin exact versions in `go.mod`.
