# BookRoom DB portability: SQLite now, PostgreSQL later?

Scope: Go + Gin, 10–20k books, single-user first. Research only (2026-11-23). Primary sources; `[INFERENCE]` marks extrapolation.

## 1. Verdict (details below)

**Ship SQLite-only; keep SQL dialect-clean behind a repository port; add Postgres only on a measured trigger.** Dual-DB is not free: maintainers who did it report doubled CI, `if engine?` branches, and "lowest common denominator" features (GitLab, Home Assistant, Foreman). None of the comparable book/media servers actually support *both* SQLite and Postgres natively except general-purpose infra (Gitea, Vaultwarden, Linkding). Your workload's real ceiling is writer *topology* (background scan + request-path progress writes), not row count.

**Decision rule (ranked):**

| Rank | Option | When |
|---|---|---|
| 1 | SQLite + WAL, dialect-clean port layer, one sqlc block | Default. 1 process, ≤20k books, ≤~10 concurrent users |
| 2 | SQLite + optional Postgres adapter (two sqlc blocks, two migration trees) | Only when an external customer/deployment explicitly needs Postgres, or trigger below fires |
| 3 | Postgres-only | Only if multi-process/multi-host writers or PG-only features (pgvector, CJK FTS quality) become core requirements |

**Triggers to move to option 2/3 (any one):** (a) ≥2 concurrently writing processes/hosts (scanner sidecar, CLI import, replicas); (b) sustained `SQLITE_BUSY`/`BUSY_SNAPSHOT` after `busy_timeout=5000` + short `BEGIN IMMEDIATE` writes; (c) p99 write latency > SLO from checkpoint stalls; (d) NFS/network FS storage requirement; (e) FTS ranking must be identical across backends for CJK.

## 2. Comparable projects: DB support + pain evidence

| Project | Default / supported | Migration story | Pain / stance evidence |
|---|---|---|---|
| **Komga** | SQLite only (main + tasks + sessions DBs); maintainer rejected PG | **Flyway** SQL migrations + jOOQ codegen | #1269 "This is not in the plans"; #1327 "Not planned. Use wal mode or store the database on ssd." (issue cites 20k+ pending tasks, pool timeout); discussion #2086: "no substantial claims that it would enhance performance... trade off to manage a full fledged database". WAL default + local-FS check in 1.23.0 |
| **Kavita** | **SQLite only — "SQLite/MySQL/Postgres" claim is FALSE.** `develop` hardcodes `UseSqlite("Data source=config/kavita.db")`; no Npgsql/MySqlConnector packages | EF Core `MigrateAsync()` + custom manual-migration snapshot/backup infrastructure | #2719 maintainer: "There is no way to use a different db with kavita. It's coded only for SQLite." #2316 "some DB pains": users hit SQLITE_BUSY with heavy multithreading (komf API); fix = pooling, one conn/API, WAL, UoW. #4708 migration failure `no such table: ManualMigrationHistory` |
| **BookLore** | MariaDB/MySQL only upstream (Spring/JPA); SQLite+PG only via unofficial `booklore-multidb` wrapper | Spring **Flyway** (`flyway-mysql`) | Wrapper README: "The upstream project supports MariaDB only"; wrapper warns SQLite is single-writer. Demand signal, not upstream support |
| **Calibre-Web** | SQLite only (reads Calibre's `metadata.db`); MySQL only via separate Calibre setup | Custom SQLAlchemy `migrate_Database()` with column checks + `ALTER TABLE` | #1049 (wontfix): "All calibre databases are sqlite databases. I'll not program anything incompatible to calibre." A 2022 Alembic/Postgres offer was declined |
| **Immich** | PostgreSQL only + pgvector/VectorChord (PG ≥14,<20); pre-existing PG documented | TypeORM historically, now **Kysely** `FileMigrationProvider` | #562 closed not-planned: "we use some postgres-only features for things like search... multiple separate services (which would mean sqlite needing to use a global write lock)". PG-only migration bugs: #15592 hardcoded `public.assets`, #20167 duplicate PKs. Docs: ≥6GB RAM, ≥2GB for PG |
| **Jellyfin** | SQLite only in server build; PG via third-party plugin. EF Core conversion shipped in 10.11 (2025) | EF Core per-provider migration sets | Maintainer in #15383: "We will not ship any database providers except for SQLite in the server build." Official post: "database is locked across all OSes/drive speeds", caused by "thousands of parallel write requests"; fixed with app-level optimistic/pessimistic `ReaderWriterLockSlim` interceptors; 10.11 migration took hours on some libraries |
| **Gitea** | SQLite default; PG ≥12, MySQL ≥8, MariaDB ≥10.4, MSSQL ≥2012 SP4 | `gitea migrate` idempotent Go/XORM migrations + `doctor recreate-table`; `gitea dump --database ...` cross-dialect export | #31820 "Database conversion is a nightmare": switching SQLite→PG "constant stream of bugs"; pgloader "KABOOM!"; #26862 "no way to migrate away from sqlite to a bespoke database solution"; #10816 `Sync2: Unknown col created_unix`. Forgejo (fork) dropped MSSQL and recommends SQLite |
| **Miniflux** | PostgreSQL only (v1 PHP era had SQLite+PG; v2 deliberately PG-only) | Embedded Go migration funcs + `schema_version` in a transaction | #1925 open since 2023; PR #3051 maintainer's SQLite build failed: `current_version=0 latest_version=99 driver=sqlite3 [Migration v1] near "(": syntax error`; PR #3783: "Support for multiple databases will only lead to incompatibilities and further maintenance burden. The migrations changes alone terrify me." Maintainer: "I'm not against that. However, I'm concerned about the number of changes required" |
| **Linkding** | SQLite default; PostgreSQL opt-in (`LD_DB_ENGINE=postgres`), Django ORM | Django `manage.py migrate` | #432 made search case-insensitive on PG; #1500 (2026) SQLite planner stats: 2,000 URL misses 2.493s → 0.031s after `ANALYZE`; proposer notes "PostgreSQL keeps its existing lookup query" — tuning is dialect-specific |
| **Vaultwarden** | SQLite default; MySQL/MariaDB + PG via `DATABASE_URL`, build-time feature flags | Diesel embedded migrations per dialect | #87: "we're using `replace_into` a lot. It appears, that this might not be supported with Postgres" (PG later merged in #621); #7133 pgloader maps `INTEGER`→`BIGINT` but Diesel expects `i32` → silent login failure; #7061 misconfigured URL silently created empty SQLite DB → data loss; cross-DB migration "unsupported" |

## 3. First-person "dual-DB was costly" quotes

- **GitLab** (Yorick Peterse, #51173): "we need to introduce branches in the code to support both PostgreSQL and MySQL"; official blog: "merge requests that were twice as complex... CI system would run our test suites twice... unable to truly take advantage of either... lowest common denominator." <https://gitlab.com/gitlab-org/gitlab-foss/-/work_items/51173>, <https://gitlab.com/gitlab-com/www-gitlab-com/-/merge_requests/24987.patch>
- **Home Assistant ADR-0018**: "a lot of special handling for the different databases... makes maintenance and implementing changes really hard... bugs being reported for the less common databases after a new release." <https://github.com/home-assistant/architecture/blob/master/adr/0018-supported-databases.md>
- **Foreman** (Lukas Zapletal, 2019): "Supporting two databases must not be that hard. Truth is, it's a challenge... carry extra bit of code in our codebase for life... Foreman 1.24 will be the last version with MySQL support." <https://theforeman.org/2019/09/dropping-support-for-mysql.html>
- **Password Pusher** (2025): dropped MySQL, kept SQLite default — "ongoing maintenance burden... often needs special treatment in database migrations." <https://the0x00.dev/password%20pusher/OSS-Password-Pusher-Dropping-MySQL/>
- **Paperless-ngx** maintainer: "we have toyed with the idea of moving to just one but it would obviously be a seriously breaking change." <https://github.com/paperless-ngx/paperless-ngx/discussions/10612#discussioncomment-14144298>
- **Jellyfin** (nielsvanvelzen, PR #15383): "We will not ship any database providers except for SQLite in the server build. Any other databases, like PostgreSQL, should be provided using plugins." <https://github.com/jellyfin/jellyfin/pull/15383>
- **Calibre-Web** (#1049, wontfix): "All calibre databases are sqlite databases. I'll not program anything incompatible to calibre." <https://github.com/janeczku/calibre-web/issues/1049>
- **Miniflux** PR #3783 contributor: "Support for multiple databases will only lead to incompatibilities and further maintenance burden. The migrations changes alone terrify me." <https://github.com/miniflux/v2/pull/3783> (closed stale, 2026-05)
- **Vaultwarden** #87: "we're using `replace_into` a lot. It appears, that this might not be supported with Postgres." <https://github.com/dani-garcia/vaultwarden/issues/87> — the exact SQL-pattern tax described in §4
- **Negative result:** no maintainer in the list said "adding Postgres to a SQLite app was a mistake." Pattern: projects *remove the secondary engine* or stay single-DB. Miniflux's move was SQLite+PG → PG-only; Forgejo moved *toward* SQLite. `[INFERENCE]` strongest signal is to not create a second backend speculatively.

## 4. Dialect-divergence table

| Feature | SQLite | Postgres | Abstraction cost |
|---|---|---|---|
| Placeholders | `?` (modernc also accepts `$N` — vendor claim, unverified) | `$1..$N` | Low via `sqlc.arg()` or two generated packages |
| `RETURNING` | 3.35+; top-level INSERT/UPDATE/DELETE only | Always; richer | Low if plain `RETURNING *`; High for CTE/aggregates |
| UPSERT | `ON CONFLICT(cols)`, `excluded.`; no `ON CONSTRAINT` | + `ON CONSTRAINT`, partial-index inference | Medium: single-target conflict only; sqlc SQLite parser gaps (#3440, #3834) |
| Auto-ID | `INTEGER PRIMARY KEY` = ROWID; `AUTOINCREMENT` only prevents reuse | `SERIAL`/`IDENTITY` | **Low if IDs generated in Go (ULID/UUID)**; don't leak rowid semantics |
| JSON | JSON as TEXT; `json_*` functions; internal JSONB 3.45+ incompatible with PG | native `jsonb`, `@>`, `?`, `#>` | **High**: marshal in Go, store TEXT/BLOB |
| Arrays | none | native `text[]`, `ANY()` | **High**: model as rows or JSON text |
| `ILIKE` | none; use `lower(x) LIKE lower(?)` / `COLLATE NOCASE` | native | Low: use `lower()` both sides |
| FTS | FTS5 `MATCH`, `bm25`, trigram tokenizer | tsvector/GIN, `ts_rank_cd`, pg_trgm/pg_bigm | **Very high**: separate port; scores incomparable |
| Types/strictness | dynamic affinity; STRICT tables 3.37+ | static | Medium: harmonize with sqlc `overrides` or Go-owned types |
| Timestamps | no `timestamptz` | `timestamptz`, session TZ conversion | Low **if** normalized to UTC integer/text in Go |

Primary sources: <https://www.sqlite.org/lang_returning.html>, <https://www.sqlite.org/lang_upsert.html>, <https://www.sqlite.org/autoinc.html>, <https://www.postgresql.org/docs/current/sql-insert.html>, <https://docs.sqlc.dev/en/v1.31.1/reference/config.html>, <https://bun.uptrace.dev/guide/drivers.html>.

## 5. Concrete Go approach

**sqlc supports multi-engine natively** — `version: "2"` config with multiple `sql:` blocks, each having its own `engine`/`schema`/`queries`/`gen`; official example generates 7 engines <https://github.com/sqlc-dev/sqlc/blob/main/examples/authors/sqlc.yaml>. But **it does not translate SQL**: no per-engine query override conditional; you duplicate the dialect-specific query files (or restrict to an intersection dialect). SQLite Go support is **Beta** in sqlc <https://docs.sqlc.dev/en/latest/reference/language-support.html>; parser gaps exist for SQLite `RETURNING ... ORDER BY`, multi-target upsert, casts. `emit_interface: true` gives a mockable `Querier`. Both engines need dual CI — sqlc's own docs note `sqlc vet` is essential.

Proven pattern (h-id, AGPL OIDC): two sqlc blocks → `repos/sqlite/db` + `repos/postgres/db`; domain interfaces in `repos/*.go`; adapters map rows and translate driver errors (`sqlite.Error` / `pgconn.PgError`) to shared sentinels; two migration FS trees. <https://github.com/juho05/h-id/blob/main/sqlc.yaml>, <https://github.com/juho05/h-id/blob/main/repos/database.go>. Alternative (GoVueKit): one PG-generated package run on modernc SQLite with `$N` placeholders, intersection SQL only <https://govuekit.dev/blog/sqlite-or-postgres>.

**Seam that keeps handlers/services identical:** narrow repository interfaces returning domain types (e.g. `BookStore`, `ProgressStore`, `SearchPort`), dependency-injected into services. Handlers/services never import `*sql.DB`, sqlc packages, or driver types. Adapters (one per engine) own all SQL and error translation. GORM/ent/bun/squirrel relocate rather than remove divergence (Dialector interfaces, per-dialect generated code, or maintenance-mode builders); with sqlc the SQL stays reviewable. `squirrel` and `sqlboiler` are in **maintenance mode**. Don't adopt an ORM solely for hypothetical portability.

## 6. FTS: sane behind a port, but ranking leaks

- SQLite FTS5 `unicode61` treats a whole Chinese run as **one token** — wrong for CJK. `trigram` (3.34+) gives substring matching but **<3-char queries never match** (e.g. 中文, 书). `bm25` is lower=better with fixed k1/b; `detail=none/column` shrinks index; external-content tables need triggers and can drift ("unintuitive and appear inconsistent"). <https://sqlite.org/fts5.html>
- Postgres `tsvector`+GIN (preferred index), `websearch_to_tsquery`, `ts_rank`/`ts_rank_cd` (higher=better, no global normalization: "impossible to produce a fair normalization"). `pg_trgm` ignores non-alphanumerics → weak CJK without locale/parser work; `pg_bigm` (2-gram, GIN-only) and `pg_cjk_parser` handle CJK better. <https://www.postgresql.org/docs/current/textsearch-controls.html>, <https://github.com/pgbigm/pg_bigm>, <https://github.com/huangjimmy/pg_cjk_parser>
- Real abstraction: **Autumn** exposes the same `search` methods over PG `tsvector`/`ts_rank_cd` and SQLite external-content FTS5/`bm25`, but explicitly picks different tokenizers/weights — semantics diverge. Gitea "solves" portability by using LIKE and **no ranking**. <https://github.com/autumn-foundation/autumn/blob/trunk-dev/docs/guide/full-text-search.md>, <https://github.com/go-gitea/gitea/issues/6648>
- **Verdict:** a `SearchPort` returning `([]BookID, []Snippet)` ordered ordinal (no numeric score) is sane. It leaks on CJK and 1–2 char queries: SQLite trigram needs a `LIKE` fallback; PG needs `pg_bigm`/`pg_cjk_parser`. Ranking *will* differ; promise "relevant order", not identical results.

## 7. SQLite concurrency ceiling

- One writer at a time even in WAL; `busy_timeout` is per-connection (mattn defaults 5000ms, modernc 0), and **does not help** read→write upgrade in deferred txns (`SQLITE_BUSY_SNAPSHOT`, immediate failure). <https://www.sqlite.org/wal.html>, <https://www.sqlite.org/rescode.html#busy_snapshot>, <https://tenthousandmeters.com/blog/sqlite-concurrent-writes-and-database-is-locked-errors>
- Benchmarks (Go, WAL, synchronous=NORMAL, busy_timeout=5000): ~70–100k simple writes/s single-thread; flat ceiling from 1→64 writer threads, collapse/errors at ≥128–256; 15.1s max latency at 4 writers from checkpoint stalls; rollback-journal 8r/8w was ~500× slower than WAL. `busy_timeout` changes error shaping, not throughput. App-level mutex gives stable throughput well past 256 writers. <https://tenthousandmeters.com/blog/sqlite-concurrent-writes-and-database-is-locked-errors>, <https://solangle.com/sqlite-wal-concurrency-benchmarks.html>
- Jellyfin's own postmortem: a task-limit bug produced "thousands of parallel write requests" and "database is locked" on all OSes/drive types; fixed with app-level interceptors. Komga added WAL + a startup local-FS check; Kavita's fixes were pooling/short transactions. **These are exactly your scan + progress + stats workload.**
- `[INFERENCE]` 10–20k books and ≤10 users with **one writer process** is far below the ceiling. Failure comes from a second writer process, long scan transactions held open while writing progress, read-then-write upgrades, or network storage — not data volume.

## 8. Deployment cost vs. actual gain

| Cost | SQLite | Postgres |
|---|---|---|
| Services | 1 binary + 1 DB file | +1 container/daemon, healthchecks, version upgrades |
| Backup | copy file (`VACUUM INTO` / litestream) | `pg_dump`, PITR/WAL archiving, restore drills |
| RAM `[INFERENCE]` | ~tens of MB | PostgreSQL baseline ~100–200MB; Immich documents ≥2GB for its PG |
| Single-binary | preserved | lost |
| Multi-host / replicas | impossible | available |

Komga's maintainer explicitly rejected PG because of "the trade off to manage a full fledged database"; Forgejo recommends SQLite as "low maintenance overhead, easy to back up and restore." For a hobbyist the concrete gain is: concurrent writers across processes, off-host backups, and CJK-capable FTS — nothing else at 20k books.

## 9. SQLite-now, Postgres-later rule list (enforce in code review)

1. No SQL in handlers/services — only repository interfaces injecting domain types.
2. sqlc for all queries; store SQL files under `sql/queries/` with `sqlc.arg()` params; no dynamically concatenated SQL (builders/squirrel allowed only in adapters).
3. No SQLite-only syntax outside the SQLite adapter: no `rowid`, `AUTOINCREMENT`, `INSERT OR REPLACE`, `PRAGMA` in shared queries, `strftime`, `json_each`, FTS5 `MATCH`. Use plain `ON CONFLICT(col) DO UPDATE SET ... = excluded....`
4. Use `RETURNING *` only on plain single-row DML; IDs generated in Go (ULID/UUID), never dependent on autoincrement reuse or ordering.
5. Timestamps as UTC: store int64 Unix millis or RFC3339 UTC text; never `CURRENT_TIMESTAMP` semantics or local-time/TZ in the DB.
6. JSON marshalled in Go → TEXT/BLOB columns; no DB JSON operators, no arrays (child rows instead).
7. Case-insensitive search via `lower(col) LIKE lower(?)`, never `ILIKE` or `COLLATE NOCASE` in shared queries.
8. FTS only via `SearchPort`; index tables/tokenizers are adapter-owned; ranking consumed as ordinal.
9. Set WAL + `busy_timeout=5000` + `synchronous=NORMAL` on **every** connection; short explicit `BEGIN IMMEDIATE` write txns; materialize scan IDs before writing; serialize writes with an app mutex if >1 process is ever added.
10. Keep schema migrations in plain versioned SQL, dialect-neutral where possible (ADD COLUMN, new tables); avoid in-place ALTER/table rebuilds. Add a second migration tree only when the PG adapter lands.
11. Dual-engine CI from day one of option 2: `sqlc generate` + repository tests against both; pinned sqlc version (SQLite support is Beta).
12. Keep a `docs/adr/000X-db-portability.md` recording the triggers in §1 so the decision is revisit-able, not forgotten.

## 10. Ranked recommendation

1. **Now:** SQLite-only + rules 1–12. Cost: near zero. Preserves option value.
2. **At first dual-writer topology or measured lock errors:** add the PG sqlc block + adapter, keep SQLite for dev/tests, run both in CI. Cost: ~1–3 engineer-weeks `[INFERENCE]`, dominated by migration/FTS/error translation.
3. **Never speculatively** add a second engine "just in case" — the field evidence shows upkeep cost accumulates per query and per migration, and users rarely switch engines after initial install (Gitea #31820).
