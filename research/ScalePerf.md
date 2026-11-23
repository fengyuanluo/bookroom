# Scale & Performance @ 20k Books — Evidence Report (2026-11-23)

## Method & scope
No local BookRoom repo (greenfield). Findings from (a) official docs/benchmarks cited inline, (b) public issue reports with concrete numbers, (c) **local microbenchmarks I ran** on this box (Python 3.13, SQLite **3.46.1**, synthetic 20k-row DBs; Go not installed so no Go-specific measurement). All local numbers explicitly tagged **[measured:local]**; estimates **[INFERENCE]**. Web claims carry URLs.

---

## 1. Numbers table (operation → latency/size)

### 1a. Local 20k-book SQLite benchmark [measured:local]
Synthetic schema: `books` (id/title/author/description/lang/series/pub_year/size/added_at/rating/pages), `progress` (20k rows), `book_tags` (~49k rows), `tags` (50), FTS5 external-content indexes. WAL, `synchronous=NORMAL`, `cache_size=-65536` (64 MiB). Warm runs, mean of 20.

| Operation | Result | Source/notes |
|---|---|---|
| Insert 20k book rows (short metadata) | 0.17 s total | [measured:local] |
| Insert 20k rows with ~1 KiB Chinese description | 0.71 s total | [measured:local] |
| Build FTS5 trigram index (title+author+series, short fields) | 1.11 s total (~0.055 ms/book) | [measured:local] |
| Build FTS5 trigram index (title+author+~1 KiB description) | **28.1 s total (~1.4 ms/book)** | [measured:local] |
| Build FTS5 unicode61 index (same 1 KiB desc) | 2.42 s (~0.12 ms/book) | [measured:local] |
| DB size: short metadata + trigram(title/author/series) + progress + tags | **5.1 MiB main + 5.2 MiB WAL** | [measured:local] |
| DB size: 20k × 1 KiB desc + trigram + unicode61 + meta trigram (all three indexes) | **283 MiB** | [measured:local]; books data alone ~19.5 MiB, trigram full ~170 MiB, unicode61 ~100 MiB, meta trigram ~3 MiB |
| List: filter lang+year, sort by added_at, LIMIT 10/50/100 OFFSET 10000 | **5.8–7.2 ms** | [measured:local]; indexed filter + index-backed sort |
| List: sort by rating, no filter, deep OFFSET 10000 | **26–31 ms** | [measured:local]; full sort of 20k rows; avoid deep OFFSET and unindexed sorts |
| `LIKE '%秋水%'` on short titles | ~4–5 ms | [measured:local] |
| `LIKE '%天地%'` on 1 KiB description (20k rows) | **~173 ms** | [measured:local] |
| `LIKE '%天地玄%'` on 1 KiB description | ~210 ms | [measured:local] |
| FTS5 trigram 3-char CJK MATCH (full desc index) | **0.07 ms** | [measured:local] |
| FTS5 trigram 3-char CJK MATCH (title/author only) | 0.026 ms | [measured:local] |
| FTS5 trigram **2-char** MATCH | **0 results (silent failure)** | [measured:local]; matches official doc (below) |
| FTS5 trigram MATCH + join + sort + LIMIT 50 | 0.05 ms | [measured:local] |
| Tag facet counts (50 tags × ~49k links, GROUP BY) | **6.3 ms** | [measured:local]; add materialized counters if called on every page |
| Progress single-row UPDATE (WAL) | 0.003–0.02 ms | [measured:local] |

**Realistic DB size forecast at 20k** [INFERENCE from local measurements + external]: metadata-only ~5 MiB; with trigram indexing **titles/authors/summaries (~1 KiB each) ~170–210 MiB**; progress/tags/reading-lists <10 MiB. If you index only title+author+tags (not full descriptions), expect **<15 MiB**. If you index full book text (EPUB body), size explodes to multi-GB — don't.

### 1b. External scale evidence

| Source | Scale | Number | Relevance |
|---|---|---|---|
| [Kavita issue #1845](https://github.com/Kareadita/Kavita/issues/1845) | 45k EPUBs, SQLite ~60 MB | per-book ingest 0.2 s @ #1 → 6 s @ #20k → 12 s @ #34k; ~570 MiB RSS | linear scan slowdown; maintainer: first-scan cost of maintaining people/genres/tags in memory; subsequent scans skip work |
| [Kavita v0.8.9 release](https://github.com/Kareadita/Kavita/discussions/4356) (2026-01-14) | 141k files / 50k files | 14 days → 3 h (112×); 10 days → 4 h (60×); 3.5 h for 96k archives with 32 threads | multithreaded scanner + async cover generation + metadata/DB work off scan loop |
| [Kavita issue #2391](https://github.com/Kareadita/Kavita/issues/2391) | 10k+ items | per-series update 1463 ms logged; rescan of 13,543 series = 8938 ms after folder restructure | folder layout & incremental detection dominate, not DB size |
| [Komga discussion #1863](https://github.com/gotson/komga/discussions/1863) (2025-02) | 14k files on Synology Btrfs | full (non-deep) scan >30 min; maintainer: 5k files scans in 1 s | filesystem (Btrfs/Docker bind mount) and per-directory stat costs matter; maintainer explicitly rejects recursive inotify (watch limits, per-folder watches) |
| [Komga issue #607](https://github.com/gotson/komga/issues/607) | unspecified | high CPU during scan traced to `HashBook` tasks | full-file hashing is deliberately enabled for trash detection; can be disabled |
| [Komga scan docs](https://github.com/gotson/komga-website/blob/master/website/docs/guides/scan-analysis-refresh.md) | — | normal scan uses **parent-folder last-modified time** to decide which books to compare; "deep scan" compares all | good model for incremental scan |
| [Calibre power-search FAQ](https://askubuntu.com/questions/77218/does-calibre-support-full-text-indexing-search) | — | default Calibre search is metadata only; full-text needs plugin/index | our FTS design must be explicit about scope |

---

## 2. Scan performance (stat + parse 20k EPUBs)

**[measured:local]** synthetic 2000 EPUB-sized ZIPs (~200 KB cover + OPF), warm page cache:
- `stat` only: **0.004 ms/book**
- `zip open + read container.xml + read/parse content.opf` via Python `zipfile` + `ElementTree`: **0.234 ms/book** → **~4.7 s for 20k books, single-threaded** on this box. Go `archive/zip` should be same order (central-directory read + small deflate) [INFERENCE; Go unavailable locally].
- Real OPFs with more XML + multiple rootfiles: budget **0.3–1 ms/book** [INFERENCE].

**What actually dominates** — not the OPF parse:
- Cover extraction + decode (see §4): ~50–200 ms/book if decoded/resized during scan → 17–67 min at 20k single-threaded. Kavita's v0.8.9 fix was precisely to move cover generation to an async background thread ([release notes](https://github.com/Kareadita/Kavita/discussions/4356)).
- Word-count/full-text extraction of whole EPUB body (Kavita #1845: likely culprit in linear slowdown).
- Full-file hashing (Komga #607: HashBook tasks pegged CPU); if you need dedupe by hash, do it lazily/incremental, not for every file on every scan.
- DB writes: one transaction per book in WAL costs fsync amortization; batch commits (e.g. 100–500 books/tx).

**Recommended design** [INFERENCE grounded in above]:
1. Walk dirs with `filepath.WalkDir`, use **directory mtime** to skip unchanged subtrees (Komga model). For each candidate book compare `(mtime, size)` to stored row; skip if identical.
2. Parse OPF only for changed books. Store `content_hash` (xxhash3 or SHA-256) computed **only** when mtime/size changed (dedupe, detects in-place edits with restored mtime; optional full-hash pass weekly).
3. Worker pool sized to `min(NumCPU, 8)` for parse; but **serialize DB writes through one goroutine/channel** (SQLite single writer). Use a single write transaction per batch.
4. Queue cover generation + FTS reindex separately at low priority; Kavita needed 32 threads for 96k archives, implying ~0.13 s/archive/thread including everything.
5. Do not use recursive filesystem watchers (Komga maintainer's reasoning in #1863: per-directory watch count limits, OS variance). Trigger scan via explicit API call after downloads instead.

---

## 3. FTS5 vs LIKE vs external search @ 20k

Official FTS5 trigram docs ([sqlite.org/fts5.html §4.3.4](https://www.sqlite.org/fts5.html#the_trigram_tokenizer)):
- Treats every contiguous sequence of **3 characters** as a token; supports substring MATCH and indexed `LIKE`/`GLOB`.
- **Substrings <3 unicode chars return nothing** (lines 847–850 of local doc copy); patterns with no 3-char literal run fall back to linear scan.
- `detail=none/column`: full-text queries **cannot use tokens longer than 3 chars**; LIKE/GLOB still work, index ~half the size.
- Built-in trigram options: `case_sensitive`, `remove_diacritics` (lines 824–843). FTS5 token max 32768 bytes (lines 860–873).

**[measured:local]** confirms: 2-char MATCH → 0 results; 3-char MATCH → 0.03–0.07 ms; LIKE on 1 KiB desc → 170–210 ms (2500–3000× slower).

External corroboration:
- [Andrew Mara, 2024-01-08](https://andrewmara.com/blog/faster-sqlite-like-queries-using-fts5-trigram-indexes): 18.2M short strings, unindexed LIKE 1.75 s → trigram LIKE 8–30 ms (~100×); index grew DB 1.3 GiB → 3.7 GiB (~2.4× overhead), `detail='none'` → 2.8 GiB (~1.5× overhead) with same query speed.
- [CJK FTS5 benchmark, 2026-11-16](https://raytien.me/writing/cjk-fts-bench/) (11 tokenizer setups, 100k passages per language): `unicode61` nDCG@10 **0.008** on Chinese (treats whole Han run as one token); built-in `trigram` finds only **59.5%** of exact substrings (short queries fail) and ranks 0.251 in Chinese; `LIKE` patch finds 100% but scans whole table; best setup is **app-level overlapping bigrams + first character of each run**, nDCG top in zh/ja/ko, 98.4% substring recall; index only +6–26% over plain bigrams. Median latency in Korean: 8 ms (bigrams), 38 ms (bigrams+first char), 96 ms (bigrams+first+last char).

**Verdict**: At 20k, **FTS5 is enough** for metadata search (title/author/series/tags) — sub-millisecond. For Chinese:
- Use `trigram` for ≥3-char substring queries, with **LIKE fallback for 1–2 char queries** (accept ~170–210 ms at 20k × 1 KiB, or restrict fallback to title/author only → ~5 ms).
- If you want proper relevance ranking for Chinese/Japanese/Korean, store an **app-level "bigrams + first char" token column** in a separate FTS5 `unicode61` table (per the CJK benchmark) — ~2× index of trigram but far better ranking. This is the single most important CJK decision.
- External search engine (Meilisearch/Typesense): **not worth it at 20k** — adds a second process, contradicts single-binary goal [INFERENCE].

---

## 4. Cover/thumbnail serving

**[measured:local]** (Pillow 13.0.0, synthetic 2400×3200 noisy JPEG, 2.47 MB):
- decode only: **59.2 ms**
- decode + draft + resize to 400×600 + re-encode JPEG q75: **47.1 ms** (JPEG `draft()` downscales during decode, faster than full decode)
At 20k covers: on-demand decode of a cold 2 MB cover adds ~50–60 ms to a request; generating all covers up front is ~17–20 CPU-minutes single-threaded [measured:local extrapolation].

**Design** [INFERENCE]:
- Extract cover **once at scan/analysis time** (Kavita had to move this off the scan hot path: [#4356](https://github.com/Kareadita/Kavita/discussions/4356)); store as file in `covers/<hash-prefix>/<bookid>-{small,large}.webp|jpg` (e.g. 400×600 and 800×1200).
- Serve via `http.ServeContent`/`ServeFile` with `ETag` (content hash), `Cache-Control: public, max-age=31536000, immutable` for hash-addressed paths, and `Last-Modified`; conditional requests return 304 with no decode.
- On-the-fly only as fallback for cache miss, with singleflight to avoid stampede.
- Disk cost: 20k × 2 thumbs × ~30–80 KB = **~1–3 GiB** [INFERENCE]; cheap at this scale.
- For OPDS 2.0, include both large and small image links (spec example uses 1400×2000 and 700×400 — [spec §2.3](https://specs.opds.io/opds-2.0)).

---

## 5. Gin vs stdlib at this scale

[Official Gin benchmarks, 2026-03-15, Gin v1.12.0 / Go 1.25.8, Apple M4 Pro](https://gin-gonic.com/en/docs/benchmarks/):
- GitHub API 203-route routing: Gin **9,944 ns/op, 0 allocs**; stdlib `HttpServeMux` in static-route set: **18,172 ns/op** (Gin 5,528 ns). Single-param route: Gin **23.31 ns/op, 0 allocs**.
- These are router-only microbenchmarks. At 20k books your API handler does 5–30 ms of SQLite/JSON work; router overhead is **<0.002%** of a request [INFERENCE]. Framework choice is **not measurable** in this workload. Choose Gin for middleware/validation ergonomics; keep JSON encoding and DB access as the optimization targets.

---

## 6. SQLite concurrency (WAL single writer)

[sqlite.org/wal.html §2.2](https://www.sqlite.org/wal.html): readers do not block writers and vice versa, but **there is only one WAL file → only one writer at a time**. Checkpoint can run with readers but must stop at the oldest reader's end mark; a long-running read tx can starve checkpointing (§6, lines 399–431 of local copy). Default autocheckpoint threshold: 1000 pages; default is 1–2% slower for read-mostly workloads but WAL avoids most fsyncs.

[Concurrent WAL benchmark, 2026-09-10](https://solangle.com/sqlite-wal-concurrency-benchmarks.html) (Go 1.23, mattn/go-sqlite3 v1.14.52, SQLite 3.46, 500k rows, NVMe, 4 cores):
- 1 reader: 941k ops/30s (0.010 ms avg); 64 readers: 1.76M ops/30s (0.363 ms avg) — reads scale.
- 1 writer: 110k ops/30s; 2 writers: 276k but **max 6.4 s**; 4 writers: max **15 s** checkpoint/lock stalls; 64 writers: 2.9 ms avg, 9.2 s max — write throughput does not scale.
- 8r/8w WAL: 1.16M ops, 0.137 ms avg, 346 ms max; **same workload in rollback/delete mode: 2,313 ops (≈500× collapse)**.
- `busy_timeout` 100→5000 ms changed failure behavior, not throughput (0.149→0.150 ms avg).

**Recommended pragmas** (per connection, on open; set via DSN for Go drivers where supported):
```
journal_mode=WAL;           -- mandatory, set once (persistent)
synchronous=NORMAL;         -- WAL-safe, lose last tx only on power loss (pragma docs lines 1702-1719)
busy_timeout=5000;          -- error shaping, not speed (docs lines 353-368)
cache_size=-65536;          -- 64 MiB; default is only -2000 = 2 MiB (docs lines 374-387)
mmap_size=274877906944;     -- 256 GiB cap; OS maps only what's read (docs lines 1257-1290)
foreign_keys=ON;            -- integrity for tags/progress
wal_autocheckpoint=1000;    -- default; consider background TRUNCATE checkpoint if scan creates long readers
```
Note: `cache_size`/`mmap_size`/`busy_timeout` are **per-connection** and must be set on every pooled connection; use `db.SetMaxOpenConns` deliberately (e.g. 4–8), and set `MaxIdleConns` ≥ that to avoid repeated pragma setup. Batch scan writes into single transactions (100–500 books) so N scan workers funnel through **one writer goroutine**; never let a UI request hold a read tx open while scanning.

---

## 7. Memory: 20k book rows in RAM cache?

**[measured:local]** raw row payload: 20k × (title+author+tag/progress fields, ~500 B) ≈ **10 MiB**; with 1 KiB descriptions ≈ 20 MiB; Go struct overhead maybe 2–5× → **20–100 MiB** [INFERENCE]. Compare: SQLite page cache with `cache_size=-65536` = 64 MiB and already keeps hot pages.

**Verdict**: caching all book rows in Go maps is **not worth it** — the SQLite page cache already does it with less code and no invalidation bugs. Real memory hogs:
1. **Image decode**: a single 2400×3200 RGB bitmap is ~23 MiB raw [measured:local: w×h×3]; a 20k-image in-memory cache is impossible. Keep covers on disk.
2. **FTS trigram index build**: full-description index peaked at ~170 MiB on disk; build can use significant temp memory.
3. **Unbounded `rows` materialization**: always paginate at the SQL level (LIMIT/OFFSET or keyset), never `SELECT *` 20k rows into a slice.
4. **Metadata scraper concurrency**: cap HTTP client connections and reuse buffers.

Kavita #1845's ~570 MiB RSS at 45k books was attributed by the maintainer to holding people/genres/tags in memory — avoid global dedupe maps by using DB unique constraints + `INSERT ... ON CONFLICT` [INFERENCE from issue + standard practice].

---

## 8. OPDS feed pagination @ 20k & PSE

OPDS 2.0 ([specs.opds.io/opds-2.0 §4](https://specs.opds.io/opds-2.0)) defines pagination in `metadata`: `numberOfItems`, `itemsPerPage`, `currentPage`, plus `links` relations `first/previous/next/last`. Spec example uses **itemsPerPage: 50**. OPDS 1.2 uses Atom `<feed>` with `<link rel="next/prev/first/last">` and OpenSearch `totalResults/startIndex/itemsPerPage`; convention is also ~20–50 entries ([spec](https://specs.opds.io/opds-1.2)).
- **Recommendation**: page size **50** (100 max for well-behaved clients); 20k / 50 = 400 pages. Use **keyset pagination** (cursor on `id`/`added_at`+id) instead of OFFSET for deep pages; keep `numberOfItems` via `COUNT(*)` cached or approximate. Facet counts via `numberOfItems` properties ([spec §2.4](https://specs.opds.io/opds-2.0)).
- Kavita v0.8.9 fixed "recently added not having pagination" and "recently updated not using pagination, causing dashboard to become unresponsive" — direct evidence paginating feeds matters at scale ([release notes](https://github.com/Kareadita/Kavita/discussions/4356)).
- **OPDS-PSE 1.2** ([spec 2023-03-05](https://github.com/anansi-project/opds-pse/blob/master/v1.2.md)): add `<link rel="http://vaemendis.net/opds-pse/stream" type="image/jpeg|gif|png" href="...{pageNumber}[&width={maxWidth}]" pse:count="N" [pse:lastRead="n"] [pse:lastReadDate="..."]/>`; pages numbered 0..N-1; MUST NOT split double pages; namespace `http://vaemendis.net/opds-pse/ns`. Known servers: Komga, Kavita, Codex, LANraragi; client Panels 2.14.2+. This is primarily for comics (CBZ/CBR) — implement for CBZ/CBR, not EPUB/PDF. For large PDFs, use HTTP Range for the file itself; PSE requires pre-rendered page images and is not a PDF streaming protocol [INFERENCE].

---

## 9. Ranked: what actually matters at 20k

1. **CJK search design** — trigram alone silently fails 1–2 char queries and ranks poorly; bigrams+first-char FTS table or LIKE fallback is the highest-impact correctness decision (§3).
2. **Scan architecture** — mtime/dir-mtime incremental skip, async cover extraction, batched single-writer commits; naive full scans with cover decode = hours (§2).
3. **Cover cache + HTTP caching** — 50–60 ms per cold 2 MB decode dominates per-request latency; pre-generate thumbs and use ETags (§4).
4. **WAL pragmas & writer serialization** — without WAL, concurrent load collapses ~500×; with 4 writers, 15 s stalls (§6).
5. **Query/index hygiene** — indexed filter+sort+keyset pagination: 6 ms vs 26–31 ms for unindexed/deep sort; tag facets 6 ms (§1).
6. **FTS scope/size** — index title/author/summary only (~5–15 MiB), not full book text (~170 MiB+ at 20k) (§1, §3).
7. **OPDS pagination + PSE** — 50/page and cursor-based deep pages; PSE for CBZ/CBR only (§8).
8. **RAM caching** — skip; SQLite page cache suffices, real cost is images (§7).
9. **Gin vs stdlib** — irrelevant at this scale (<0.002% of request time) (§5).

## Open questions / caveats
- No Go toolchain here: all local benchmarks used Python sqlite3 3.46.1; Go drivers (mattn/go-sqlite3 vs modernc.org/sqlite vs ncruces/go-sqlite3) differ in default pragmas and FTS5/trigram availability — verify trigram compiled in (SQLite ≥3.34; trigram shipped 3.34.0, 2021-03-12) [INFERENCE].
- Benchmarks on synthetic data on this host; real-world metadata lengths, filesystem (network mounts!), and client concurrency will shift numbers. Komga #1863 shows 14k files taking >30 min on Btrfs/Docker bind mount despite similar hardware.
- The CJK benchmark's author also competes in it and documents corrections; its relative rankings are still the best public cross-language evidence, treat p-values 0.01–0.05 as hints.
