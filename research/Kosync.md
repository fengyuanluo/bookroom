# KOReader kosync / OPDS research (evidence-backed)

Sources (cloned 2026-11-23): `github.com/koreader/koreader` HEAD `1294bb3d` (2026-11-21); `github.com/koreader/koreader-sync-server` HEAD `46ef6b84` (2026-09-23); `github.com/pid1/kosync-conformance` SPEC v1 (2026-09-21, CC0). The conformance SPEC is descriptive (not canon) but pins commits and reproduces measured results; cite it as `SPEC.md:<line>`.

## 1. Protocol — verbatim test contract

Defaults: base `https://sync.koreader.rocks:443/`, no path/version prefix (`plugins/kosync.koplugin/api.json:2`). Every request sends **`Accept: application/vnd.koreader.v1+json`** (KOSyncClient.lua:28-31); reference server *requires* it (412 code 101 if wrong/missing) — **be permissive** (SPEC.md:145-203). Bodies are JSON objects; no request `Content-Type` gate on reference server; responses are `application/json` (SPEC.md:205-258). Auth headers on all authenticated calls (KOSyncClient.lua:32-36):

```
x-auth-user: <username>          # nonempty, NO colon (Redis key guard)
x-auth-key:  <lowercase-hex MD5 of plaintext password>   # main.lua:16,568,606
```

`x-auth-key` is **opaque**: store/compare verbatim; do NOT re-hash (SPEC.md:281-309, :402-414); "registration-disabled"/"taken" = 402 with codes 2005/2002.

| Method | Path | Auth | Request body | Success response | Key errors |
|---|---|---|---|---|---|
| GET | `/healthcheck` | no | — | `200 {"state":"OK"}` | server-only |
| POST | `/users/create` | no | `{"username":"…","password":"<md5>"}` | `201 {"username":"…"}` | `402 {"code":2002,"message":"Username is already registered."}`, `403/2003`, `402/2005` |
| GET | `/users/auth` | yes | — | `200 {"authorized":"OK"}` | `401 {"code":2001,"message":"Unauthorized"}` |
| PUT | `/syncs/progress` | yes | `{"document":"<hex32>","progress":"<string>","percentage":0.42,"device":"Kobo","device_id":"…","metadata":{…}}` | `200 {"document":"<hex32>","timestamp":1790005119}` | `403/2004` bad document; `403/2003` missing fields; `401/2001` |
| GET | `/syncs/progress/:document` | yes | — | `200 {"document":…,"percentage":…,"progress":…,"device":…,"device_id":…,"timestamp":…}`; **unknown ⇒ `200 {}`** | `401/2001` |
| DELETE | `/users/me` | yes | — | `200 {"deleted":true}` | `401/2001`; `404/2006` absent (server-only) |
| PUT | `/users/password` | yes | `{"password":"<new md5>"}` | `200 {"updated":true}` | `401/2001`; `403/2003` (server-only) |

Routes: `koreader-sync-server/config/routes.lua:7-18`; bodies/errors: `app/controllers/1/syncs_controller.lua:108-296`, `config/errors.lua:15-24`; endpoints detail SPEC.md:355-790. Field rules (SPEC.md:882-1048): `progress` **always string** (client `tostring()`, KOSyncClient.lua:130; EPUB = CRe XPointer `/body/DocFragment[…]/text().123`; paged PDF/CBZ = page number string; main.lua:658-664,705-712). `percentage` JSON number 0–1; server does not range-check. `timestamp` = **server `os.time()` Unix seconds**, never sent by client; MUST be on GET. `device_id` optional on PUT, stored/returned only if sent. `metadata` (`filename`,`title`,`authors` strings; main.lua:694-703) accepted, ignored by official server; off by default. **Never emit 202** (client treats it as failure; KOSyncClient.lua:137). Non-401 errors cause client to retry from a local queue for 4 weeks (KOSyncQueue.lua:5-7,39-61; SPEC.md:853-878) — use 401 for permanent failures. Timeouts: progress 2s connect/5s total, auth 5/10s (KOSyncClient.lua:5-8).

## 2. Document identifier — reproducible MD5

Two methods, `CHECKSUM_METHOD = {BINARY=0, FILENAME=1}` (main.lua:46-49); default **BINARY** (main.lua:65). FILENAME = plain MD5 of basename (`main.lua:678-692`). BINARY = KOReader `util.partialMD5` (`frontend/util.lua:1094-1112`, verified verbatim in clone): open binary; `step=size=1024`; incremental MD5; loop `i=-1..10`: `file:seek("set", lshift(step, 2*i))`, read up to 1024 B, feed to hash, **break at first short/EOF read**; return lowercase hex. **First offset is 0, not 256**: LuaJIT masks shift count to 5 bits, `-2 & 31 = 30`, `1024<<30` overflows 32-bit to 0 (SPEC.md:1168-1221, measured with LuaJIT 2.1 + KOReader golden vectors). Twelve offsets: `0, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456, 1073741824` (SPEC.md:1223-1262); max 12 KiB read; final sample truncated, never padded; file length is NOT hashed (2048 B and 3000 B with equal first 2 KiB collide). Golden vectors (SPEC.md:1283-1330): `leaves.epub` (881331 B) → `59d481d168cca6267322f150c5f6a2a3`; `tall.pdf` (388039 B) → `41cce710f34e5ec21315e19c99821415`; synthetic P(2048), P(3000) → `d0e59a0c7b893c3b8d6a9bddbd64e631`; P(200000) → `1784b150454ef4cf6780ceb94af0386f`; 12-sample sparse-file vector → `266ff27c24919f4cec96f90f9b3231ca`. Plugin reads cached digest from sidecar `partial_md5_checksum` (main.lua:674-676), written at open by `frontend/apps/reader/readerui.lua:497-501`. Works for **any format** KOReader can open (PDF/CBZ/TXT/EPUB): algorithm is raw bytes, not EPUB-specific; format affects only `progress` semantics (XPointer vs page number). Caveat: appending highlights near a sample boundary (esp. PDF) changes digest.

## 3. Server model / conflicts

Per `(user, document)` exactly **one** position, LWW, no merge/history/per-device fan-out (SPEC.md:71-89). Redis hash key `user:<username>:document:<document>` with fields `percentage`, `progress`, `device`, `device_id`, `timestamp` (`syncs_controller.lua:4-10`, README's flat-key layout is stale: SPEC.md:1923-1944). Accounts: `user:<username>:key` → verbatim MD5 via `SETNX` (`:128-144`). Write re-checks auth atomically in a Lua script (`:55-73,244-292`). Client-side conflict resolution (main.lua:841-933): ignore self (`device`+`device_id`), ignore if rounded percentage equal or progress equal; **interactive pull applies remote unconditionally, no prompt**; automatic pull decides "remote newer" via `body.timestamp > self.last_page_turn_timestamp` (device `os.time()` at last page turn, init 0), falling back to `percentage > percentage` if server omits timestamp. Default `sync_forward=PROMPT`, `sync_backward=DISABLE` (main.lua:62-64) — omitting timestamp inverts policy (SPEC.md:1416-1435). Two unsynchronized clocks compared directly; first auto-pull after open always treats remote as newer. Client debounces 25 s (main.lua:52).

## 4. Server implementation / vendoring

`koreader-sync-server` is **Lua on OpenResty/nginx + Gin 0.2.0 framework + Redis** (README.md:5-7), **not Go**; HEAD 2026-09-23, **AGPL-3.0** (COPYING). Code: `app/controllers/1/syncs_controller.lua` (330 lines), `config/{routes,errors,application,nginx,settings}.lua`, `db/redis.lua` (pooled resty.redis, DB 1/2/3 per env, 1 s timeout), plus spec/benchmark scripts. TLS on 7200 (self-signed), plaintext 17200 for reverse proxy (nginx.conf:29-46, README.md:56-75). Scale notes: no published capacity numbers; design is O(1) Redis HSET/HMGET; benchmark scripts exist (`scripts/benchmark.{lua,sh}`) but no 10k–20k results. **[INFERENCE]** SQLite should substitute fine for Redis at 10–20k books (≈ users × documents rows, tiny payloads) — but verify with our own load test. Vendoring: AGPL-3.0 permits copying with attribution and requires AGPL for derivative network service; **do not copy source verbatim into a proprietary/closed server**; the CC0 conformance SPEC (SPEC.md, vectors) can be copied freely as our test contract; reimplement logic in Go is cleanest — protocol is ~7 endpoints and 1 Redis Lua script.

## 5. Extensions / annotations

No annotation/highlight support in kosync protocol: conformance SPEC mentions highlights only re: MD5 stability (SPEC.md:1165,1274); Readest KOSyncClient.ts:41-48 carries only document/progress/percentage/timestamp/device/device_id; it sends optional `metadata` (KOSyncClient.ts:279-302). Readest syncs annotations via **separate** services (Grimmory notes two-way CFI sync: `readest/apps/readest-app/src/services/grimmory/notesSync.ts:1-16`; KOReader has no wire format for them). "Full-plus" without breaking KOReader: keep kosync endpoints byte-compatible; store extra JSON keys (metadata, annotations, reading-time, tags/lists) in separate tables/fields; ignore unknown keys on GET for KOReader; use `metadata` object (already accepted-and-ignored) as a safe sidecar for filename/title/authors; expose our richer features through **new** endpoints (e.g. `/api/v1/...`) rather than adding semantics to `/syncs/*`. Extra response fields are ignored by KOReader (it reads only known keys, main.lua:851-933) and by Readest (typed interface but `...data` spread, KOSyncClient.ts:257). **[INFERENCE]** Annotation sync would need our forked clients on both ends plus a CFI/XPointer mapping layer; page-based PDF/CBZ positions have no shared anchor.

## 6. OPDS-PSE / OPDS auth / WebDAV

OPDS plugin present (`plugins/opds.koplugin/`): supports OPDS 1.x Atom (`catalog_type="application/atom+xml"`) and advertises OPDS 2.0 JSON in negotiation (`opds20_feed="application/opds+json, application/atom+xml;profile=opds-catalog, */*"`, opdsbrowser.lua:36,548), with JSON decode fallback (`:624-625`). Navigation/acquisition links matched by rel prefixes `http://opds-spec.org/acquisition*`, `borrow`, `subsection`, `facet`, opensearch search templates (opdsbrowser.lua:37-56,905-970); thumbnails/covers via `http://opds-spec.org/image[/thumbnail]`.

OPDS-PSE (paged streaming, https://vaemendis.net/opds-pse/) **is supported** (`opdspse.lua`, opdsbrowser.lua:17,42): link rel `http://vaemendis.net/opds-pse/stream`; needs a `<link>` attribute whose name **ends in `:count`** (total pages; namespace may be any prefix) — optional `*:lastRead` (1-based page) (opdsbrowser.lua:984-1006); server serves pages by URL template, PSE helper also has Kavita-specific `getLastPage` REST flow (opdspse.lua:17-80). OPDS catalogs with auth are standard: username/password stored per catalog (opdsbrowser.lua:79-80,340-357,443-444), sent via HTTP Basic same-origin only (`opdsclient.lua:20-24,55-63`); cookies supported (`cookiejar.lua`); 401/403 have dedicated user messages (opdsbrowser.lua:582-583). Cloud storage plugin (`plugins/cloudstorage.koplugin/`) provides **WebDAV** (providers/webdav.lua), Dropbox, FTP with upload/download and folder-sync (cloudstorage.lua:1041-1160) — separate from OPDS; an authenticated WebDAV endpoint is well-trodden ground for KOReader.

## 7. Compatibility risk list (for our Go implementation)

| Risk | Mitigation |
|---|---|
| Unknown document must be `200 {}`, not 404 (client shows sync error otherwise) | Explicit empty-object response |
| `password` field is already MD5; treating as plaintext breaks accounts | Store verbatim; no re-hash |
| 202 / wrong status → 4-week retry loop; 402/403 ambiguity | Exact code table; use 401 for permanent auth failure |
| `progress` numeric coercion destroys EPUB XPointer; `"56"` vs `56` breaks already-synced check | Store/return as string, byte-for-byte (escaped `\/` is fine) |
| Missing/fractional/millisecond timestamp breaks conflict policy | Server seconds, always on GET |
| `Accept` header: official requires it; third parties don't | Require-or-accept both; never gate on request `Content-Type` |
| Colon in username/document ⇒ key injection on Redis | Reject `:`; document `[A-Za-z0-9_]+` only (client always 32-hex) but widen GET/PUT consistently |
| `x-auth-key` case sensitivity; hardening (Argon2 etc.) breaks portability | Compare verbatim; keep hex case as sent |
| Digest mismatch: KOReader 12 samples offset 0 vs Kavita 11 samples/uppercase; Readest removed filename mode | Reproduce `util.partialMD5` exactly; golden vectors in contract; lowercase output |
| Filename vs binary checksum settings silently desync | Document choice in our client; validate with KOReader users |
| PDF/CBZ highlight appends near offsets change digest | Accept as upstream limitation |
| 5 s progress timeout; add no inline enrichment to PUT | Fast path; enrich async |
| Self-detection breaks with custom device names (upstream bug) | Echo device/device_id faithfully; cannot fix server-side |
| AGPL on sync server source; CC0 spec | Reimplement in Go, copy only CC0 test vectors/spec |
