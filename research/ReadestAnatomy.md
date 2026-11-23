# Readest Anatomy — fork research (github.com/readest/readest, main @ 2026-11-23)

## Executive summary (5 lines)
1. **No `readest-core`/`readest-shared`/`readest-rs` packages exist**: the monorepo is one huge Next.js 16 app (`apps/readest-app`, v0.12.20) plus native/plug-in apps; `packages/*` are mostly **uninitialized git submodules** (foliate-js, tauri, qcms…), so the assumed layout is wrong.
2. **Supabase is surprisingly well isolated on the client**: only ~60–65 non-test app source files touch it; the data path already talks to a generic HTTP API (`getAPIBaseUrl()`, `NEXT_PUBLIC_API_BASE_URL`) and local data is **SQLite (Turso WASM/OPFS), not Dexie/IndexedDB** — so a Go server can be a drop-in behind the existing endpoints.
3. **Readest already ships a self-hosted Docker stack** (`docker/`: Kong + GoTrue + PostgREST + MinIO, `SELF_HOSTED=true` unlocks premium features) and an in-progress "Supabase exit" migration plan in `.claude/memory/supabase-exit-host-migration.md` — strong evidence the HTTP surface is the real seam.
4. **Two sync protocols to mimic**: legacy `/api/sync` (books/book_configs/book_notes, `synced_at` cursor, field-level LWW) and newer `/api/sync/replicas` (HLC CRDT rows `{kind, replica_id, fields_jsonb, updated_at_ts…}`, `MAX_PUSH_BATCH=100`, 409 CLOCK_SKEW beyond 60 s); KOSync is a separate client-side protocol our server can implement natively.
5. **Fork cost**: full Supabase excision ≈ 60–65 app source files + ~25 tests + CI/Docker/workers/koplugin; **minimal viable fork ≈ 8–12 files** if we keep Readest's HTTP API shape and replace auth + sync transport behind `API_BASE_URL`.

---

## 1. Monorepo layout (packages, purpose, LOC)

Workspace: `pnpm-workspace.yaml:1-7` — `apps/*`, 2 Cloudflare workers, browser extension, `packages/foliate-js` only. Root scripts delegate everything to `@readest/readest-app` (`package.json:4-30`); pnpm 11.1.1, Biome, TypeScript 5.

| Path | What it is | Evidence | LOC |
|---|---|---|---|
| `apps/readest-app` | The entire product: Next.js 16.4.6 + React 19.2.9 app, both App Router (`src/app/api/*`) and Pages Router (`src/pages/api/sync.ts`), Tauri shell in `src-tauri/`. v0.12.20 | `apps/readest-app/package.json:1-6`, `docs/code-layout.md:1-60` | **~150–200k** TS/TSX [INFERENCE: 270+ listed source files, several 30–57 KB, e.g. `bookService.ts` 56.6 KB, `sync.ts` 53.9 KB] |
| `apps/readest.koplugin` | KOReader Lua plugin: sync auth, sync client, progress/annotation sync, local library, `readest_supabaseauth.lua`, `readest-sync-api.json` | dir listing `apps/readest.koplugin/` | ~30k Lua [INFERENCE] |
| `apps/readest-calibre-plugin`, `apps/readest-crosspoint-plugin` | Calibre & CrossPoint integrations | `apps/` listing | small [INFERENCE] |
| `packages/foliate-js` | **Uninitialized submodule** (`readest/foliate-js` fork), dir empty. Rendering engine, pinned via gitlink | `.gitmodules:1-3`, dir listing | n/a |
| `packages/tauri`, `packages/qcms`, `packages/js-mdict`, `packages/simplecc-wasm` | Submodules: Tauri fork, PDF color management, MDict reader, Chinese conversion WASM | `.gitmodules:4-6,14-18` | n/a |
| `packages/swift-rs`, `packages/tauri-runtime-cef-stub` | Rust-side host pieces (not submodules) | root listing | small [INFERENCE] |
| `apps/readest-app/workers/{send-email,iap-reconcile,auth-proxy}` | Cloudflare Workers; `auth-proxy` proxies Supabase password grants | `workers/auth-proxy/wrangler.toml:6-19`, `workers/auth-proxy/src/index.ts:1-40` | ~500 [INFERENCE] |

There is **no** `packages/readest-core`, `readest-shared`, `readest-rs` — all shared code lives in `apps/readest-app/src/{libs,services,types,utils}`.

## 2. Supabase coupling

Counts (case-insensitive `supabase|@supabase|SUPABASE`):
- **279 total matches repo-wide**, but most are `.claude/memory/` docs.
- **57 files** in `src/` import from `@/utils/supabase` (1 is a test) → **56 non-test source files**.
- **17 files** import `@supabase/*` directly (7 tests) → **10 non-test source files**.
- Overlapping distinct total: **~60–65 non-test app source files** [INFERENCE from the two lists].
- Extras: ~25 test files (`src/__tests__/**`), 3 CI workflows (`.github/workflows/{nightly,release,try-appimage}.yml`), `Dockerfile:35-36`, 2 workers, browser extension (`extensions/send-to-readest/src/content/auth-bridge.ts:2-57`), koplugin (`readest_supabaseauth.lua`), `scripts/db/*`.

Key modules:
| File | Role |
|---|---|
| `src/utils/supabase.ts:1-70` | Single client factory: runtime config → env → base64 defaults; `supabase`, `createSupabaseClient(token)`, `createSupabaseAdminClient()`. Pins `storageKey`, proxies auth through `auth-cf.readest.com` |
| `src/context/AuthContext.tsx:13-160` | Session in localStorage (`token`,`refresh_token`,`user`); `onAuthStateChange`, refresh/signOut, user type from `@supabase/supabase-js` |
| `src/app/auth/page.tsx:8-391`, `auth/{callback,recovery,update}/page.tsx`, `components/{AuthPanel,EmailPasswordAuth,CodeSignIn}.tsx` | Login UI; direct `signInWithOAuth`, `signInWithIdToken`, `signInWithPassword`, `onAuthStateChange`; `AuthPanel` takes a `SupabaseClient` prop |
| `src/utils/access.ts:2,292-319` | `getUserID()`/`getAccessToken()`/`validateUserAndToken()` call `supabase.auth.getSession()/getClaims()` |
| `src/pages/api/sync.ts:1-20`, `src/app/api/**/route.ts` (~20 routes) | Server-side data access via `createSupabaseAdminClient()` — crosspoint, share, stats, stripe, storage, device auth |
| `src/services/runtimeConfig.ts:2-47` | supabaseUrl/anonKey config fields |

**Isolation verdict**: auth/session is **leaked into UI** (`AuthContext`, auth pages, `utils/access.ts`, and plan gates read JWT claims), but **data sync is not** — it goes through HTTP endpoints with Bearer tokens. Storage/metadata/supabase-admin usage is confined to server routes and `src/libs/payment/**`, `src/libs/shareServer.ts`, `src/libs/crosspoint.ts`. An upstream plan confirms: *"DB access is already server-side only (API routes); auth is the client-facing lock-in"* (`.claude/memory/supabase-exit-host-migration.md`, 2026-10-12/11-06).

## 3. Local data layer

**No Dexie / no IndexedDB schema for library data** (only test mocks in `src/__tests__/ai/ai-store.test.ts:18-61`). Local storage is SQLite:
- Web: `@readest/turso-database-wasm/webpack` → OPFS (`src/services/database/webDatabaseService.ts:22-27`), WAL checkpoint on close (`:56`).
- Native: `tauri-plugin-turso` (`src/services/database/nativeDatabaseService.ts:2,18`).
- Common interface: `DatabaseService` in `src/types/database.ts` (`execute/select/batch/close`); migrations in `src/services/database/migrations/index.ts` (schema types: opds, bookorbit-sync, grimmory-sync, hardcover-sync, notion-sync, statistics, library-search, reedy…).
- Library books live as files `Books/<hash>/{book file, cover.png, config.json, nav.json}` + SQLite metadata; settings are `settings.json` (device-level, explicitly *not* account-filtered — `.claude/memory/bookshelves-review-2026-09-21.md:27`).
- KOReader-compatible stats schema in `migrations/index.ts:138-195`: tables `book`, `page_stat_data`, view `page_stat`, `readest_page_ext`, `readest_book_ext`, `readest_stat_sync_state`.

Key wire types (`src/types/book.ts`):
```
Book { hash, metaHash, format, title, author, tags, coverHash, createdAt, updatedAt,
       deletedAt, progress?: [current,total] (1-based pages), readingStatus,
       readingStatusUpdatedAt, coverUpdatedAt, metadataUpdatedAt, groupUpdatedAt, metadata? }
BookNote { id, type: 'bookmark'|'annotation'|'excerpt'|'notebook'|'ink', cfi, xpointer0/1,
           page, text, style, color, note, ink?, crosspoint?, createdAt, updatedAt, deletedAt }
BookConfig (docs/book-config.md:37-52): location = CFI, xpointer = KOReader location.
```
Cloud mirror schema (Postgres): `docker/volumes/db/init/schema.sql` — `books`, `book_configs`, `book_notes`, `files`; RLS per `user_id`; `synced_at` trigger `books_set_synced_at` decoupled from `updated_at` (migration 016).

## 4. Reading engine & progress representation

- Renderer: **fork of foliate-js** (`packages/foliate-js` submodule, `readest/foliate-js`), imported as `foliate-js/epubcfi.js`, `foliate-js/footnotes.js` across reader. `src/libs/document.ts:8-9` re-exports `CFI = epubcfi`. PDF via vendored PDF.js 6 (`package.json:copy-pdfjs-*`, `libs/document.ts:configurePDFWorker`); CBZ via zip.js + comic image reader; TXT/MD/HTML converted in-browser (`src/workers/txt-converter.worker.ts`) and by Rust `convert` commands.
- `FoliateViewer.tsx` (1743 lines) is the integration hub: view creation, CFI navigation, `useProgressSync`, `useKOSync` (3 providers), annotations, translation.
- Types (`src/libs/document.ts:14-60`, `src/types/book.ts`): `Location { current, next, total }` (page ordinals), `TOCItem.cfi`, `SectionItem.cfi`, `PageInfo { current, next?, total }`, `TimeInfo { section, total }`, `BookDoc`, `FoliateView` (`src/types/view.ts`, has `getCFI(index,range)`, `goTo(cfi)`, `resolveCFI`).
- **Progress is dual**: reflowable = canonical CFI string (`BookConfig.location`) + fraction `progress` JSON; fixed layout = `progress:[page,total]` + `progress.section`; KOReader interop = XPointer (`BookConfig.xpointer`) and percentage. `docs/book-config.md:37-52`.

## 5. Sync protocol our Go server must mimic

Transport is plain HTTP + `Authorization: Bearer <JWT>`; base `getAPIBaseUrl() = getBaseUrl() + '/api'`, `getBaseUrl()` = runtime config → `API_BASE_URL` → `NEXT_PUBLIC_API_BASE_URL` → `READEST_WEB_BASE_URL` (`src/services/environment.ts:14-42`).

| Endpoint | Payload / semantics | Evidence |
|---|---|---|
| `GET/POST /api/sync?type=books\|book_configs\|book_notes\|stats` | Legacy sync. POST upsert rows; GET incremental by `synced_at` cursor with `limit` paging (10k-book fix); deletion = `deleted_at` tombstones. Conflict: row LWW on `updated_at` **plus field-level LWW** for `reading_status`, `cover_hash`, `metadata`, `group_id/name` (each with own `*_updated_at`), note column omission preservation, TTS pack union | `src/pages/api/sync.ts:1-20,25-290`, `src/__tests__/sync-*-test.ts` |
| `POST /api/sync/replicas` `{rows:[…]}` / `{cursors:[{kind,since}]}` | New CRDT replica sync. Row: `{user_id, kind, replica_id, fields_jsonb: Record<string,{v,t,s}>, manifest_jsonb, deleted_at_ts, reincarnation, updated_at_ts, schema_version}`. HLC string `13hex-8hex-deviceId`, lexicographically ordered; per-field merge via `mergeFields`; server rejects clock skew >60 s (409 CLOCK_SKEW); `MAX_PUSH_BATCH=100`; batched pull returns `{results:[{kind,rows}]}`; 404 → empty for old backends | `src/types/replica.ts:1-45`, `src/libs/replicaSyncClient.ts:12-120`, `src/services/sync/replicaSyncManager.ts:1-300`, `src/libs/crdt.ts` |
| `GET/POST/DELETE /api/sync/replica-keys` | E2E passphrase salts `{saltId, alg, salt, createdAt}` | `src/libs/replicaSyncClient.ts:130-255` |
| `/api/storage/{upload,list,download,delete,purge,stats}` | S3/MinIO book-file storage with quota enforcement | `src/pages/api/storage/*` |
| `/api/kosync` proxy, `/api/opds/proxy` | Client-side proxies to arbitrary KOSync/OPDS servers | `KOSyncClient.ts:60-100` |
| `/api/auth/device/*`, `/api/crosspoint/*`, `/api/share/*`, `/api/stats/*`, `/api/stripe/*` | ancillary Supabase-admin-backed routes (drop or reimplement) | `src/app/api/**/route.ts` |

Legacy sync already has explicit 10k-library support (paged pull test `src/__tests__/sync-books-paged-pull.test.ts:5`). Stats: KOReader-shaped local `statistics.db`, union-merge across devices, server-side archive in R2 (`docs/stats-archive.md`).

## 6. Auth swap points (minimum for JWT)

Assume-a-Supabase-session components: `AuthContext.tsx` (user type `User` from supabase-js, `onAuthStateChange`), `src/app/auth/page.tsx` (OAuth providers, magic link, ID token, QR code), `components/AuthPanel.tsx` (takes `SupabaseClient` prop), `EmailPasswordAuth.tsx`, `CodeSignIn.tsx`, `utils/access.ts` (`getClaims` decodes plan from JWT), `utils/iap.ts` (user UUID as `appAccountToken`).

Minimum swap (~8–12 files): replace `utils/supabase.ts` with a tiny client exposing `getAccessToken()`, `refresh()`, `getUser()`; rewrite `AuthContext.tsx` against our JWT endpoints; replace `src/app/auth/**` UI with email/password against our server; update `utils/access.ts:292-319` (claims → our payload); keep `ReplicaSyncClient`/`useProgressSync` unchanged (they only need a Bearer token). Server routes under `src/app/api/**` can be **deleted** if we host our own API and set `API_BASE_URL`; `SELF_HOSTED=true` already unlocks premium features without a signed-in user (`docker/README.md:35-40`).

## 7. Build targets / UI

Next.js **App Router + Pages Router hybrid**, React 19, Tailwind 4 + daisyUI 5 + Radix primitives, Zustand 5 stores, i18next (key-as-content English, `public/locales/<lng>/translation.json`, `src/i18n/i18n.ts:1-50`, `docs/i18n.md:1-66`). i18n is independent of Supabase → no fork work.
Scripts (`apps/readest-app/package.json:5-100`): `dev`/`build`/`start` (Tauri env), `dev-web`/`build-web`/`start-web`, `build-tauri`, platform builds (`build-win-x64`, `build-linux-x64`, iOS/Android), Cloudflare deploy via OpenNext. Dropping Tauri: web build (`pnpm build-web`) works standalone; breakage is limited to `src/services/nativeAppService.ts`, `src/utils/bridge.ts`, `src-tauri/**` imports guarded by `isTauriAppPlatform()` (`services/environment.ts:10`) — fork can ignore them.

## 8. Existing self-host story — YES

`docker/README.md` documents a full Compose stack (`supabase/postgres`, `kong`, `gotrue`, `postgrest`, `minio`) with client image `ghcr.io/readest/readest`; runtime env `SUPABASE_PUBLIC_URL`, `SUPABASE_ANON_KEY`, `API_BASE_URL`, `OBJECT_STORAGE_TYPE`, `SELF_HOSTED`, `STORAGE_FIXED_QUOTA`, `TRANSLATION_FIXED_QUOTA`, `FONT_BASE_URL`, `READEST_TRUSTED_PRIVATE_HOSTS`. `src/app/runtime-config.js/route.ts` (873 B) emits runtime config. An upstream memory doc records the maintainers' own plan to move auth off Supabase via `auth.readest.com` + GoTrue/Postgres (step 3), with password proxy already shipped (2026-11-06). Sector implication: our Go server can realistically target the **existing HTTP contract** rather than reimplementing the whole client.

## Dependency diagram — modules to touch

```
[Our Go server: Gin+SQLite]
   ├── /auth/token (JWT) ───────────> utils/access.ts ──> utils/supabase.ts* (REWRITE)
   │                                  context/AuthContext.tsx* (REWRITE)
   │                                  app/auth/{page,callback,recovery,update,components/*}* (REPLACE)
   ├── /api/sync (legacy) ──────────> pages/api/sync.ts [can DELETE; client endpoint unchanged]
   ├── /api/sync/replicas ──────────> libs/replicaSyncClient.ts (keep; token source only)
   │                                  services/sync/replicaSyncManager.ts (keep)
   │                                  types/replica.ts, libs/crdt.ts (keep)
   ├── /api/storage/* ──────────────> pages/api/storage/* [DELETE]
   ├── /kosync (native impl) ───────> services/sync/KOSyncClient.ts (keep)
   ├── /opds (native impl) ─────────> services/opds/* (keep; client-side feature)
   └── /api/metadata/search ────────> services/metadata/providers/{googlebooks,openlibrary}.ts
                                      (ADD our xiaoshuohub provider + Douban/Hardcover)
   Data layer (UNTOUCHED): libs/document.ts + foliate-js submodule;
      services/database/{web,native}DatabaseService.ts; types/book.ts; stores/*; reader/*
   DROP: src/app/api/{stripe,share,stats,apple,google,auth/device,crosspoint}/**
```

## Fork cost estimate (explicit)

| Scope | Files | Work |
|---|---|---|
| **Minimal viable** (JWT auth + legacy sync + storage + metadata + KOSync/OPDS server) | **~8–12 client files** + [server from scratch] | `utils/supabase.ts`, `AuthContext.tsx`, 5–6 auth UI files, `utils/access.ts`, `services/runtimeConfig.ts`; keep HTTP clients; delete server API routes and set `API_BASE_URL` |
| **Full Supabase excision** (also replica E2E, stats, share, stripe/IAP, crosspoint, workers, extension, koplugin) | **~60–65 non-test source files** + ~25 tests + ~20 ancillary config/build files | koplugin `readest_supabaseauth.lua` + `docs/*` + CI workflows + `Dockerfile` + `workers/*` + `scripts/db/*` |
| Risk hotspots | — | `utils/supabase.ts:9-10` throws if env base64 absent (breaks bare `vitest`); cloud sessions persisted under `sb-readest-auth-token`; sign-in gating in `AuthContext.onAuthStateChange`; 60-s HLC skew rule; `SELF_HOSTED` entitlement logic |

Upstream evidence: maintainers decided to exit Supabase on 2026-10-12; step 1 (host move) merged #6729; auth proxy merged #7355 (2026-11-06); steps 2–3 on hold — i.e., **the seams we need are actively being carved upstream**, which lowers fork risk. [Source: `apps/readest-app/.claude/memory/supabase-exit-host-migration.md`, modified 2026-11-02, read 2026-11-23.]

Unverified / [INFERENCE] tags: all LOC figures, the 60–65 distinct-file total (overlap estimated), and the "minimal viable = 8–12 files" estimate are inferred from file listings and import counts; all other claims cite concrete files/lines above.