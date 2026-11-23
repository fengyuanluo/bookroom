# ProductBench — self-hosted book server comparisons (research, 2026-11-23)

Evidence: GitHub API metadata fetched 2026-11-23; source via shallow clones of default branches at HEAD same date (Komga `master`, Kavita default branch, BookLore `master`, Grimmory `develop`, BookOrbit `main`, Calibre-Web `master`). Paths below are repo-relative with line numbers where read directly. Star counts are snapshots, not durable claims. `[INFERENCE]` marks reasoning not directly evidenced.

## 1. Comparison table

| Product | Stack | DB | License | Stars | Scale evidence / complaints | Modeling shape |
|---|---|---|---|---|---|---|
| Komga (`gotson/komga`) | Kotlin + Spring Boot, Vue web UI; since 2019-08-08 | SQLite (3 files: `database.sqlite`, `tasks.sqlite`, `sessions.sqlite` — `komga/src/main/resources/application.yml:19,26,28`), Flyway migrations | MIT | 6,874 | 88k-book library deletion hit `SQLITE_TOOBIG` (issue #1848); "500 pdf files make SQL timeout" (#1303); "Chrome browser hang on large collection" (#86); PRAGMA optimize problems (#1761) | `LIBRARY` → `SERIES` → `BOOK` → `MEDIA/MEDIA_PAGE`; no edition entity; `SERIES_METADATA`, `BOOK_METADATA` with `*_LOCK` per field; `COLLECTION`+`COLLECTION_SERIES`, later `READLIST`+`READLIST_BOOK`, both series/book groupers; tags are metadata columns |
| Kavita (`Kareadita/Kavita`) | C#/.NET 10, EF Core 10 (`Kavita.Database/Kavita.Database.csproj:19`), Angular | SQLite (EF Core provider) | GPL-3.0 | 12,168 | "database is locked" scanning (#2985), "Database is Locked error appearing frequently" (#4982), progress wiping (#4905); `IUnitOfWork.cs:45-46` explicitly documents `busy_timeout` workaround for `SQLITE_BUSY_SNAPSHOT` | `Series` → `Volume` → `Chapter` → `MangaFile`; `SeriesMetadata`+tags/genres; `ReadingList`/`ReadingListItem` (chapter order) vs `AppUserCollection` (series, per-user); no real edition row (2026-07 migration adds `MangaBakaEditionId` only) |
| BookLore (`herraristotle/BookLore`) | Java Spring Boot + MariaDB, React/Vite UI; created 2025-03-13 | MariaDB (`booklore-api/src/main/resources/application.yaml:18`) | **None** (no LICENSE file in repo; API metadata license=null) | 131 | No explicit scale claims found; repo last pushed 2026-08-10 | `book` → `book_metadata` (series_name/number/total columns, no series table) → `author`/`category` mappings; `shelf`+`book_shelf_mapping` (per-user); tags via category; no edition row |
| Calibre-Web (`janeczku/calibre-web`) | Python Flask, reads external Calibre library | Calibre's SQLite `metadata.db` (user points app at folder; README) | GPL-3.0 | 18,588 | 150k+ books `/author` page 35–52s despite 0.37s SQL (#3699); "Very slow loading times" (#1969); Kobo sync large libraries (#1276); cache system (#2789) | Inherits Calibre schema (books → series via `books_series_link`, authors, tags, collections as virtual libraries); no editions (Calibre has none); reading progress lives in Calibre-Web's own app DB, not `metadata.db` |
| Grimmory (`grimmory-tools/grimmory`) | Java Spring Boot + Flyway, Angular; created 2026-03-12, clearly a fork/successor of BookLore (identical package/table names, `org.booklore.*`, DB name default `booklore`) | MariaDB (`backend/src/main/resources/application.yaml:96`) | AGPL-3.0 | 4,771 | "Cover/Thumbnail storage does not scale" (#520); v3.2.1 notes improved ingest; 225 open issues | Same V1 tables as BookLore, plus `book_file`/`user_book_file_progress`; `shelf`/`category` still; per-field lock columns and locked-field migrations V117; sidecar support V115 |
| BookOrbit (`bookorbit/bookorbit`) | TypeScript NestJS + Fastify + Drizzle, Vue 3 client, pnpm monorepo; created 2026-05-09 | PostgreSQL (`README.md` Quick Start creates `data/postgres`; Docker Compose) | AGPL-3.0 | 6,745 | "Bulk rename goes through every book" (#361), ComicVine timeouts; 654 open issues; README explicitly rejects NAS/network storage | `books` → `book_files`; `book_metadata` (1:1); `book_series` + `book_series_memberships` (M:N, migration 0021); `collections`/`collection_books`, `smart_scopes`; `tags`/`book_tags`/`genres`; `hardcover_edition_id` foreign ID only (0025) |

## 2. Series vs book vs edition; lists vs shelves vs tags

| Product | Series | Book | Edition | Reading list | Shelf/collection | Tags |
|---|---|---|---|---|---|---|
| Komga | First-class `SERIES`+`SERIES_METADATA` | `BOOK` under series, `BOOK_METADATA`; author is denormalized name/role rows | None; ISBN column added (`V20210308163522__book_isbn.sql`) | `READLIST`+`READLIST_BOOK` with `NUMBER` order; `ORDERED` flag (`V20230221170726`) | `COLLECTION`+`COLLECTION_SERIES` (series-level) | Metadata tags/aggregated tags; no separate tag table initially |
| Kavita | First-class `Series` with 5 name variants incl. `NormalizedName`, `SortName`, `LocalizedName` (`Kavita.Models/Entities/Series.cs:17-42`) | `Volume` → `Chapter` → `MangaFile`; a "book" is a Volume/Chapter pair, not a row | Not modeled; external edition IDs only | `ReadingList` of `ReadingListItem` (Order, explicit chapter) with tags + CBL sync metadata (`ReadingList.cs:12-49`) | `AppUserCollection` = per-user set of Series (`AppUserCollection.cs:7-36`) | `Tag`, `Genre`, `SeriesMetadataTag`, `CollectionTag`, `ReadingListTag` — distinct tables per target type |
| BookLore/Grimmory | Denormalized strings `series_name`, `series_number`, `series_total` on `book_metadata` | `book` + `book_metadata` 1:1 | None | No ordered reading list | `shelf` (per-user, name/icon/sort) + `book_shelf_mapping` (`V1__Create_Tables.sql:188-207`) | `category` + `book_metadata_category_mapping`; Grimmory adds "smart shelves" rule-based |
| Calibre-Web | Calibre `series` table via link | `books` | None | None | Calibre collection field / virtual libraries | Calibre tags |
| BookOrbit | `book_series` + M:N `book_series_memberships` with `series_index`, `display_order` (migration 0021) | `books` + `book_files` + 1:1 `book_metadata` | `hardcover_edition_id` only | `collections` with `display_order` doubles as reading list; `smart_scopes` for dynamic | Same `collections` (per-user, `sync_to_kobo`) | `tags`, `genres`, M:N mappings |

**Minimum needed set** for BookRoom (10–20k books): `books` (one row per logical work/edition actually on disk — do NOT split series/edition unless acquisitions need multiple editions of one work), `book_files` (format, path, hash, size, mtime), `series` + `book_series` optional M:N with `series_index` (cheap to add later; needed for 书单 of long Chinese webnovels), `authors` + `book_authors` (M:N with role), `tags` + `book_tags`, `users`, `reading_progress`, `reading_lists` + `reading_list_items` (ordered) — **merge "shelf" and "reading list" into one ordered per-user table with an `is_ordered` flag**, as Komga does; tags are not a list. Editions: store `edition_id`/`isbn` as metadata columns, no separate table until a concrete duplicate-edition use case appears. `[INFERENCE]` — most products here survive without an edition entity; BookOrbit's `hardcover_edition_id` is provider bookkeeping, not a domain model.

## 3. Reading progress models

| Product | Granularity | Multi-device |
|---|---|---|
| Komga | `READ_PROGRESS(BOOK_ID, USER_ID)` PK, `PAGE`, `COMPLETED`, plus gzipped EPUB `locator` JSON (migration `V20240422132621__fix_read_progress_locators.kt:16-50`) | One current position per user/book; KOReader sync maps to it via partial-MD5 hashing (`KoreaderHasher.kt:11-35`) |
| Kavita | `AppUserProgress` per user+Chapter: `PagesRead`, `BookScrollId`, `KoboSpan`, `TotalReads`, timestamps; `RowVersion` concurrency token added in 2021 (`20210817152226_ProgressConcurencyCheck.cs:10-20`) | Single current position per user/chapter; `ClientDevice`/`ClientDeviceHistory` track device identity (`ClientDevice.cs:7-40`) but not separate positions |
| BookLore | `user_book_progress` UNIQUE(user,book) with `pdf_progress`, `epub_progress` CFI string (`V1__Create_Tables.sql:220-231`); Grimmory V18 adds percent columns | One position per user/book (legacy), but Grimmory V100 adds `user_book_file_progress` UNIQUE(user, book_file) with `position_data`, `position_href`, `progress_percent`, `tts_position_cfi` — position is per **file/format** |
| Calibre-Web | Progress in app DB per user/book; Kobo reads/writes via `cps/kobo.py` | Single position; no per-device store |
| BookOrbit | `reading_progress` PK(book_file_id, user_id): `percentage` 0–100 CHECK, `cfi`, `page_number`, `position_seconds` (baseline 0000); separate `koreader_device_progress` rows per device_id with `progress`, `percentage` 0–1, `orphaned_hash` (migration 0002) | **Best explicit multi-device model**: canonical normalized progress + per-device raw positions/hashes for KOReader reconciliation |

Recommendation: canonical `progress(user_id, book_file_id, position_json, percentage, page_number, updated_at)` PK(user,file) + optional `device_progress(device_id, book_file_id, user_id, raw_position, hash, updated_at)` if KOReader two-way sync matters. Store both normalized percentage and an opaque locator (CFI/page) — Grimmory and BookOrbit converged on this.

## 4. Metadata scraping

- **Komga**: mostly *embedded/sidecar* metadata, not web scrapers — `EpubMetadataProvider`, `ComicInfoProvider`, `MylarSeriesProvider`, `LocalArtworkProvider` (`komga/src/main/kotlin/org/gotson/komga/infrastructure/metadata/`); `MetadataAggregator.kt:9-25` rolls child book metadata up to series; sidecar table in `V20210609165742__sidecars.sql`. Per-field `*_LOCK` booleans in `BOOK_METADATA`/`SERIES_METADATA` implement manual override. No general provider chain/rate limiter.
- **Kavita**: external IDs on `Series`/`Volume` (AniList, MAL, Hardcover, ComicVine, MangaBaka etc. — `Volume.cs:64-70`), Kavita+ metadata matching, `ExternalSeriesMetadata` DbSet (`DataContext.cs:81`), `MetadataSettings`/`MetadataFieldMapping` tables (`:90-91`), cover lock flags. `[INFERENCE]` cloud metadata is partly Kavita+ paid; local scan is primary.
- **BookLore/Grimmory**: explicit provider map `BookParserConfig.java:15-36` — OpenLibrary, Amazon, GoodReads, Google, Hardcover, ComicVine, Douban, LubimyCzytac, RanobeDb, Audible, AppleBooks; async `metadata_fetch_jobs` + `metadata_fetch_proposals` with `metadata_json` for **human-approved** bulk fetch (V37), `all_fields_locked` and per-field locks; sidecar source setting V115 (`metadata_source='EMBEDDED'` default). Douban added via migration V150 — confirms Douban is a recognized Chinese fallback.
- **Calibre-Web**: plugin-based `cps/metadata_provider/{google,douban,amazon,comicvine,scholar,lubimyczytac}.py`; writes back into Calibre `metadata.db`; README recommends desktop Calibre for conversion.
- **BookOrbit**: built-in + external sidecar provider containers (Douban/FantLab/J-Novel Club/Libris/LubimyCzytac/MangaBaka), configurable per-field priority rules (`docs/METADATA_PROVIDERS.md:12-83`), provider keys/IDs/ratings, `locked_fields text[]` and `last_metadata_fetch_at` on `book_metadata` (baseline 0000). Field-rule chain is the closest match to BookRoom's provider chain; per-provider containers are a clean pattern for the local xiaoshuohub API.
- `[INFERENCE]` Rate limiting: none of these repos advertise shared rate-limiter code for metadata providers; treat caching, backoff, and a fetch queue as BookRoom's own responsibility (Grimmory's `metadata_fetch_jobs`, BookOrbit's `book_metadata_fetch_queue` are the closest precedents).

## 5. Recurring issue complaints

| Theme | Evidence |
|---|---|
| SQLite write contention / locked DB | Kavita #2985, #4982; Kavita source `IUnitOfWork.cs:45-46` documents busy_timeout workaround |
| Large-library UI/query slowness | Komga #1303 (500 PDFs SQL timeout), #86 (large collection hang); Calibre-Web #3699 (150k books, 35–52s author page), #1969; Grimmory #520 (cover storage does not scale); BookOrbit #361 (bulk rename full scan) |
| Progress sync correctness | Kavita #4905 (progress wiped), progress-save 400 errors; Komga locator repair migration V20240422132621 |
| Scale ceiling with SQLite + covers | Komga #1848 (88k books, SQLITE_TOOBIG on delete); Calibre-Web #1276 (Kobo sync large libraries) |

BookRoom implication: keep SQLite but use WAL + `busy_timeout`, single writer, bounded batches for scans/deletes, external cover files (not BLOBs), and indexes on `(library_id, updated_at)`, `(user_id, book_id)`, full-text via FTS5 if needed. `[INFERENCE]` from the failure modes above.

## 6. Calibre content server API

There **is** an undocumented JSON AJAX API in Calibre core, worth knowing but not a stable interop target:
- `src/calibre/srv/ajax.py:179` `/ajax/book/{book_id}/{library_id=None}`; `:225` `/ajax/books/{library_id=None}`; `:285` `/ajax/categories/...`; `:508` `/ajax/books_in/{category}/{item}/...`; `:615` `/ajax/search/...`; `:645` `/ajax/library-info` (all `@endpoint(..., postprocess=json)`), with `book_to_json()` at `:84` and tests in `src/calibre/srv/tests/ajax.py:90-450`.
- No official API documentation; MobileRead thread asks exactly this and receives no official spec (https://www.mobileread.com/forums/showthread.php?t=351799, accessed 2026-11-23). Calibre's manual documents OPDS, not `/ajax/` (https://manual.calibre-ebook.com/server.html).
- Calibre-Web (the Flask app) only exposes small `/ajax/*` helper routes (`cps/admin.py:425,437`, `cps/editbooks.py:217`, `cps/web.py:210`) and `/get_*_json` autocomplete routes (`cps/web.py:262-286`), not a general book API.
- **Verdict**: OPDS 1.2/2.0 is the only sane documented interop; optionally mimic `/ajax/books`-style JSON for convenience, but never depend on it.

## 7. Grimmory & BookOrbit verification

Both exist and are active as of 2026-11-23:
- **Grimmory**: https://github.com/grimmory-tools/grimmory — self-hosted library for ebooks/comics/audiobooks, Java Spring Boot (package `org.booklore.*`, MariaDB, Flyway V1–V149+), Angular client, AGPL-3.0, 4,771 stars, created 2026-03-12, v3.2.1 release notes mention bidirectional KOReader sync, public shelves, OPDS, ingest/perf fixes (https://github.com/grimmory-tools/grimmory/releases). Effectively a fast-moving fork/continuation of BookLore's codebase.
- **BookOrbit**: https://github.com/bookorbit/bookorbit — "Your Reading Space", TypeScript/NestJS+Fastify server, Vue client, Drizzle migrations 0000–0059+, PostgreSQL, AGPL-3.0, 6,745 stars, created 2026-05-09, KOReader/Kobo sync, external provider containers, explicit OpenAPI-ish Swagger decorators via `@nestjs/swagger` (no checked-in OpenAPI YAML; generated at runtime).

## 8. Bottom line for BookRoom

Adopt: one `books` table + `book_files`; per-field lock flags (Komga/Grimmory); single ordered table for lists/shelves (Komga); canonical progress + device raw positions (BookOrbit); async fetch queue with review proposals + provider priority rules (Grimmory + BookOrbit); SQLite WAL/batched writes from day one; OPDS 1.2+2.0 and kosync as first-class协议 surfaces, OpenAPI schema generated from Gin handlers to stay agent-friendly. Avoid: metadata BLOBs in DB, unpaginated list endpoints, unbounded bulk scans, edition tables without demand.

## Executive summary (5 lines)
1. Six comparators verified with live GitHub metadata (2026-11-23): Komga 6.9k★ MIT Kotlin/SQLite; Kavita 12.2k★ GPL C#/SQLite; Calibre-Web 18.6k★ GPL Python/Calibre SQLite; BookLore 131★ unlicensed Java/MariaDB; Grimmory 4.8k★ AGPL Java/MariaDB; BookOrbit 6.7k★ AGPL TS/PostgreSQL.
2. Grimmory is a BookLore-derived codebase; BookOrbit is the newest and most feature-complete multi-device design.
3. Minimum viable data model: books + book_files + series(memberships) + authors/tags + users + ordered per-user lists + canonical progress (optional device positions); no edition table.
4. Repeated failure modes across projects: SQLite write locks, large-library query/UI stalls, cover storage growth, progress-sync races — design around them.
5. Calibre's `/ajax/*` JSON exists but is undocumented; OPDS 1.2/2.0 is the only sane interop target.
