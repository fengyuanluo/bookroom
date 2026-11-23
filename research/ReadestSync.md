# ReadestSync.md — sync/data-layer deep dive

**Source**: `github.com/readest/readest`, app `@readest/readest-app` **v0.12.20** (`apps/readest-app/package.json:3`), git `origin/main` = `fb978e48`, latest upstream release v0.12.20 published **2026-11-05T22:40:01Z** (github.com/readest/readest/releases, api.github.com/repos/readest/readest/releases/latest). All line refs below are under `apps/readest-app/src/` unless noted. This fork is far ahead of upstream mainline docs: it has 4 distinct sync subsystems and **already implements reading-time stats** (Q4 answer is NOT greenfield).

## Executive summary (5 lines)
1. Four sync layers coexist: native Readest Cloud `/api/sync` (books/configs/notes/stats), CRDT "replica" rows (bookshelf=书单, settings, catalogs), third-party file backends (WebDAV/GDrive/S3/OneDrive/iCloud via `library.json`+`config.json`), and KOSync.
2. Local book data is mostly **filesystem JSON**, not IndexedDB: `Books/library.json` + `Books/<hash>/{config.json,cover.png,nav.json}`; IndexedDB is only the **web FS shim** (`AppFileSystem`/store `files`) + AI/translation caches; stats live in SQLite `statistics.db`.
3. Conflict rule is whole-row **LWW on `updatedAt`** with field-level LWW overrides (`readingStatusUpdatedAt`, `coverUpdatedAt`, `metadataUpdatedAt`, `groupUpdatedAt`) + tombstones `deletedAt`/`deleted_at_ts`; replicas use HLC field-level CRDT LWW.
4. KOReader sync is real and reusable (`KOSyncClient.ts` + `useKOSync.ts`), with CFI↔XPointer conversion and conflict detection; reading stats are KOReader-schema-compatible (`page_stat_data`).
5. Library page is **not hard-gated** by sign-in (anonymous users see a local/demo shelf); gate points are `keepLogin→/auth` redirect, "Sign in to sync" buttons, and `user`-requiring sync hooks.

## Q1 — Sync implementation map

| Subsystem | Entry files | Entities | Wire endpoint | Triggers |
|---|---|---|---|---|
| Native Cloud (legacy) | `libs/sync.ts` (`SyncClient`:10, `pullChanges`:59, `pushChanges`:108), `hooks/useSync.ts` (`useSync`:128) | `books`,`configs`,`notes`,`stats` (`SyncType`:17) | `getAPIBaseUrl()+/sync` → server `pages/api/sync.ts` | below |
| Replica CRDT | `services/sync/replicaSyncManager.ts`, `replicaPullAndApply.ts`, `replicaPublish.ts`, `libs/replicaSyncClient.ts`, `libs/crdt.ts` | `bookshelf`(=书单),`settings`,`dictionary`,`font`,`texture`,`opds_catalog`,`abs_server`,`custom_translator`,`translation_prompt`,`custom_tts` (`libs/replicaSchemas.ts:146-236`, `types/settings.ts:433-460`) | `/api/sync/replicas`, `/api/sync/replica-keys` | boot full-pull, visibility/online, `useReplicaPull` in `app/library/page.tsx:~225` |
| File backends | `services/sync/file/{engine,runLibrarySync,merge,appLocalStore}.ts`, `providers/{webdav,s3,gdrive,onedrive,icloud}/*` | library membership + per-book `config.json` blobs | provider REST (WebDAV/S3/Drive…) | `useLibraryFileSync`, `pullLibrary` (`app/library/hooks/useBooksSync.ts:84-135`) |
| KOSync | `services/sync/KOSyncClient.ts`, `app/reader/hooks/useKOSync.ts`, `app/reader/hooks/kosyncProgress.ts`, server `pages/api/kosync.ts` | progress only (`document`,`progress`,`percentage`,`timestamp`,`device`) | KOReader `/syncs/progress/*`, direct for LAN/Tauri, else proxy `getAPIBaseUrl()+/kosync` (`KOSyncClient.ts:96-112`) | book open pull, debounced push on progress, conflict dialog |

**Push/pull triggers (native):**
- Books: `app/library/hooks/useBooksSync.ts` — auto sync throttled `SYNC_BOOKS_INTERVAL_SEC=5s` (`services/constants.ts:1041`, `useBooksSync.ts:145-176`), fires on `user`/`library` change; pull on load (`:189-193`); `getNewBooks` selects rows where `!syncedAt || lastSyncedAtBooks < getBookChangedAt(book) || < deletedAt` (`:45-58`). Paged pull 1000 rows (`hooks/useSync.ts:73-119`).
- Progress: `app/reader/hooks/useProgressSync.ts` — pull once on open w/ retries `[1500,4000,10000]ms` (`:28,118-151`); auto-push debounced `SYNC_PROGRESS_INTERVAL_SEC=3s` (`constants.ts:1039`) on `progress.location` change (`:271-275`); flush+push on close/window-inactive/manual `sync-book-progress` event (`:201-269`).
- Notes: `app/reader/hooks/useNotesSync.ts` — throttled `SYNC_NOTES_INTERVAL_SEC=5s` (`constants.ts:1040`); enriches XPointers/CrossPoint anchors then `syncNotes(...,'both')` (`:225-260`).
- Stats: `app/reader/components/ReadingStatsTracker.tsx` — pull on open (`:110-121`), push on 10s timer (`:101-109`) + close (`:175-186`).

**Local table/store names:**
- Native cloud wire rows: `DBBook`,`DBBookConfig`,`DBBookNote` (`types/records.ts:3-73`) — server Postgres tables `books`,`book_configs`,`book_notes`,`stat_books`,`stat_page_data`. Book hash dirs under `Books/`; config filename `getConfigFilename = <hash>/config.json` (`utils/book.ts`).
- Replica rows: `ReplicaRow{kind,replica_id,fields_jsonb,manifest_jsonb,deleted_at_ts,reincarnation,updated_at_ts,schema_version}` (`types/replica.ts:20-33`); bookshelf fields = `definition,position,itemOrder,itemOrderMode` (`services/bookshelves/replica.ts:14-21`), max 200 rows/user (`libs/replicaSchemas.ts:149`).
- Stats SQLite: `statistics.db` in `Data/` (`services/statistics/statisticsDb.ts:62`), schema `book`, `page_stat_data`, view `page_stat`, `numbers`, `readest_page_ext`, `readest_book_ext`, `readest_stat_sync_state` (`services/database/migrations/index.ts:140-190`).
- IndexedDB: `AppFileSystem` DB, object store `files` keyPath `path` — **web-only filesystem shim** (`services/webAppService.ts:43-59`); AI `aiStore.ts`, translation `utils/translators/cache.ts`. **Book files on web are blobs in this store / OPFS, not a book-specific IDB table.**

## Q2 — Exact data shapes (pasted)

`types/book.ts:33`: `export type ReadingStatus = 'unread' | 'reading' | 'finished' | 'abandoned';`

`types/book.ts:717-764` — **BookConfig** (the synced progress/config entity):
```ts
export interface BookConfig {
  schemaVersion?: number;
  bookHash?: string;
  metaHash?: string;
  progress?: [number, number]; // [current pagenum, total pagenum], 1-based page number
  location?: string; // CFI of the current location
  xpointer?: string; // XPointer of the current location (for Koreader interoperability)
  booknotes?: BookNote[];
  rsvpPosition?: { cfi: string; wordText: string };
  searchConfig?: Partial<BookSearchConfig>;
  viewSettings?: Partial<ViewSettings>;
  audiobook?: PairedAudiobook;   // device-local, excluded from cloud push
  hardcover?: HardcoverBookLink; // device-local
  pagebound?: PageboundBookLink; // device-local
  ttsPacks?: TTSPacks;           // union-merged, never by updatedAt
  widePages?: string[];          // device-local
  panX?: number;                 // device-local
  lastSyncedAtConfig?: number; lastSyncedAtNotes?: number;
  lastPushedAtConfig?: number; lastPushedAtNotes?: number;
  foliateImportedAt?: number;
  updatedAt: number;
}
```
`types/book.ts:611-626` — **BookProgress** (in-memory reader state, NOT the wire form):
```ts
export interface BookProgress {
  location: string; sectionHref: string; sectionLabel: string;
  section: PageInfo; pageinfo: PageInfo;
  pageItem?: { label?: string; href?: string } | null;
  timeinfo: TimeInfo; fraction: number; index: number;
  range: Range; page: number;
}
```
`types/book.ts:~190-250` — **BookNote**:
```ts
export interface BookNote {
  bookHash?: string; metaHash?: string; id: string;
  type: BookNoteType; // 'bookmark'|'annotation'|'excerpt'|'notebook'|'ink'
  cfi: string; xpointer0?: string|null; xpointer1?: string|null; page?: number;
  text?: string; style?: HighlightStyle; color?: HighlightColor; note: string;
  global?: boolean; ink?: InkStroke[]; crosspoint?: CrossPointAnchor|null;
  createdAt: number; updatedAt: number; deletedAt?: number|null;
}
```
`types/book.ts` **Book** synced fields (excerpt): `hash, metaHash, format, title, author, groupId/groupName/groupUpdatedAt, tags, coverHash/coverUpdatedAt, progress:[number,number]|null, readingStatus, readingStatusUpdatedAt, metadata, metadataUpdatedAt, createdAt, updatedAt, deletedAt, uploadedAt, downloadedAt, coverDownloadedAt, syncedAt`. Device-local (stripped on push): `filePath, altFilePaths, audiobook, pagebound, absDownloadedAt`.

`types/records.ts:21-35` — **DBBookConfig** (wire shape): `user_id, book_hash, meta_hash, location, xpointer, progress:string(JSON), rsvp_position, search_config, view_settings, tts_packs, created_at, updated_at, deleted_at`. Transform: `utils/transform.ts transformBookConfigToDB/FromDB` (push carries only the whitelisted columns; `serializeConfig` in `utils/serializer.ts:23-57` strips global-default view settings before push).

`libs/sync.ts:48-57` — **SyncData payload**:
```ts
export interface SyncData {
  books?: Partial<BookRecord>[]; notes?: Partial<BookNoteRecord>[];
  configs?: Partial<BookConfigRecord>[]; statBooks?: StatBookRecord[];
  statPages?: StatPageRecord[];
}
```
`types/statistics.ts` — **PageStatEvent** `{bookMd5,page,startTime,duration,totalPages}` (Unix seconds; `StatBook{bookMd5,title,authors,deletedAt?}`).

## Q3 — Conflict resolution (concrete rules)

| Entity | Rule | Evidence |
|---|---|---|
| books (whole row) | LWW on `updatedAt`; tie → remote (`>=`) | `app/library/hooks/useBooksSync.ts processOldBook:~250` |
| books.readingStatus | field LWW `readingStatusUpdatedAt` (tie→client) | `useBooksSync.ts:258-262`; server `pages/api/sync.ts:52-66 resolveReadingStatusMerge` |
| books.cover | field LWW `coverUpdatedAt` | `useBooksSync.ts:265-268`; `pages/api/sync.ts:97-103 resolveCoverMerge` |
| books.metadata (title/author/tags) | field LWW `metadataUpdatedAt` | `useBooksSync.ts:271-283`; `pages/api/sync.ts:218-245 resolveMetadataMerge` |
| books.group | field LWW `groupUpdatedAt`; tie grouped-wins | `utils/book.ts pickFresherGroup:~280`; `pages/api/sync.ts:279-298 resolveGroupMerge` |
| books.ttsPacks | union merge, never `updatedAt` | `pages/api/sync.ts:184 resolveTTSPacksMerge`; `utils/ttsPacks.ts mergeTTSPacks` |
| configs (progress) | server LWW on `updated_at`; first-pull gate blocks stale push till remote applied | `useProgressSync.ts:118-151`; `libs/sync.ts:105` comment "last-writer-wins…implemented on server" |
| notes | LWW on `updatedAt`; **tie → tombstone**; clock-skew guard `nextBooknoteStamp` | `utils/booknoteStamp.ts:5-19 incomingNoteWins/nextBooknoteStamp` |
| stats | server keeps MAX duration per `(book,page,start_time)`; idempotent re-push | `types/... migrations/index.ts:147 UNIQUE`, `services/statistics/statsSync.ts:133` comment |
| replicas (bookshelf etc.) | field-level HLC LWW; tie by deviceId (`s`); `deleted_at_ts` tombstones + `reincarnation` tokens | `libs/crdt.ts:100-118 pickWinner/mergeFields`, `:150-190 mergeReplica`; `replicaSyncManager.ts:mergeDirtyRows:53-90` |

**Tombstones/cursors**: `deletedAt` on Book/BookNote, `deleted_at` on DB rows, `deleted_at_ts` on replicas; `synced_at` (server cursor for books, `:computeMaxTimestamp hooks/useSync.ts:25-39`), `uploaded_at` (book blob-ready flag), `fileSyncDeletionRequestedAt` (explicit provider-delete authorization, `types/book.ts`). `booknoteStamp` prevents clock-skew wipe (#6544).

## Q4 — Reading time / stats: EXISTS (not greenfield)

Full stack, KOReader-compatible: `types/statistics.ts`, `services/statistics/{statisticsDb,trackerCore,statsSync,readingStats,ttsStatsRecorder}.ts`, `app/reader/components/ReadingStatsTracker.tsx`, `components/stats/ReadingStatsDialog.tsx`, migration `services/database/migrations/index.ts:140-190`. Aggregates (sessions, streaks, totals) derived via SQL; streak logic `services/statistics/readingStats.ts` + tested in `__tests__/statistics/readingStats.test.ts`. Syncs via `SyncType='stats'` and also pushes to BookOrbit/Grimmory. Defaults: `idleTimeoutSeconds=120, maxEventSeconds=120, minEventSeconds=3` (`types/statistics.ts:45-49`). **Implication**: BookRoom's stats server can mirror the KOReader `page_stat_data` wire shape (`StatPageRecord`) and the client already speaks it.

## Q5 — Book file open / fetch path

- Local canonical path: `Books/<hash>/<safe-title>.<ext>` (`utils/book.ts getLocalBookFilename`), cover `Books/<hash>/cover.png`, config `Books/<hash>/config.json`. On web, `AppService.fs` is an IndexedDB store `files{path→File}` (`services/webAppService.ts:43-59`) with SQLite/OPFS for DBs — so "download into IndexedDB" happens implicitly on web via `webDownload`→`appService.writeFile` (`libs/storage.ts:186-190`).
- Remote book: `Book.url` → "lazy load content via url" (`types/book.ts:100`). Cloud download: `services/cloudService.ts:340-410 downloadBook` → `downloadCloudFile` → `libs/storage.ts:130-180 downloadFile` (signed URL from `/api/storage/download?fileKey=<userId>/<cfp>`); `cfp` = `Readest/Books/<hash>/<file>` (`utils/bookFileKey.ts:2-11`). TTS cache under `Cache/tts-cache/<hash>` (`cloudService.ts:78`).
- **Swappable**: YES. `getAPIBaseUrl()` derives from runtime config `apiBaseUrl` (`services/environment.ts:16-20`, `services/runtimeConfig.ts:38`); `downloadFile` accepts an explicit `url` param (bypassing signed-URL lookup) (`libs/storage.ts:130-145`). OPDS adapter already exists (`services/adapters/opdsCatalog.ts`), ABS uses direct fetch. A self-hosted HTTP source can be plugged via `apiBaseUrl` + `Book.url`, with minimal changes in `libs/storage.ts` and `services/cloudService.ts`.

## Q6 — Feature flags / cloud disable list

Env (`.env`, `.env.local.example`, `.env.tauri`) and code reads:
- `NEXT_PUBLIC_APP_PLATFORM` (`services/environment.ts:12-13`), `NEXT_PUBLIC_API_BASE_URL`/`API_BASE_URL` (`:19`, `runtimeConfig.ts:38`), `NEXT_PUBLIC_NODE_BASE_URL` (`:21`)
- `NEXT_PUBLIC_(DEFAULT_)?SUPABASE_URL/KEY[_BASE64]` (`runtimeConfig.ts:31-32`, `.env:8-9`), `SUPABASE_PUBLIC_URL`
- `NEXT_PUBLIC_SELF_HOSTED`/`SELF_HOSTED` (`utils/access.ts:148-153`, `runtimeConfig.ts:43-45`) — **self-hosted bypasses all premium gates** via `isCustomizationAllowed` (`utils/access.ts:154-156`); set this to true.
- `NEXT_PUBLIC_STORAGE_FIXED_QUOTA`, `TRANSLATION_FIXED_QUOTA`, `OBJECT_STORAGE_TYPE`, `FONT_BASE_URL`, `TTS_FIXED_QUOTA` (`runtimeConfig.ts:37-66`), `POSTHOG_*`, `STRIPE_PUBLISHABLE_KEY*`, `GOOGLE_CLIENT_ID`, `MICROSOFT_CLIENT_ID`, `USE_APPLE_SIGN_IN`, `DIST_CHANNEL`, `DISABLE_UPDATER` (scripts/memory).
- Premium master switches: `CLOUD_SYNC_REQUIRES_PREMIUM=true` (`utils/access.ts:64`), `TTS_CACHE_REQUIRES_PREMIUM`, `ABS_OFFLINE_REQUIRES_PREMIUM`, `ANKI_REQUIRES_PREMIUM`, `NEARBY_PAIRING_REQUIRES_PREMIUM` (same file) — all neutralized by `isSelfHosted()`.
- Cloud-provider selection: `isReadestCloudEnabled()` (`services/sync/cloudSyncProvider.ts:~90`), gating `PROVIDER_GATED_CATEGORIES` (`services/sync/syncCategories.ts:~98-113`), `resolveCloudSyncGate`/`getActiveFileSyncBackends` (`cloudSyncProvider.ts:~175-200`).
- Auth/telemetry to remove: `context/AuthContext.tsx` (supabase + posthog), `utils/supabase.ts`, `utils/access.ts:311-328 getAccessToken`, `app/user/page.tsx` + `app/user/components/{PlansComparison,Checkout,PlanCard,StorageManager,Subscription…}`, `app/auth/page.tsx`, Stripe/IAP API routes.

## Q7 — Library UI entry & sign-in gating

- Entry page: `app/library/page.tsx` (`LibraryPageContent`:~215). It renders from `useLibraryStore().library` loaded via `appService.loadLibraryBooks()` (`:~775-800`), and mounts `useBooksSync()`, `useLibraryFileSync()`, `useReplicaPull({kinds:[…,'bookshelf']})` (`:~225-245`). "Server booklists" (书单) are the `bookshelf` replica kind (`services/bookshelves/replica.ts`) pulled here.
- **No hard sign-in wall**: anonymous visitors get a local/demo shelf (`useDemoBooks`, local `library.json`). Gating points:
  - `app/library/page.tsx:752-769 initLogin` — if `settings.keepLogin && !token/user` → `router.push('/auth')`.
  - `app/library/components/LibraryEmptyState.tsx:45-50` — "Sign in to sync your library" button when `!user`.
  - `app/library/components/SettingsMenu.tsx:300,357` — account row / "Sign In" item.
  - `hooks/useSync.ts:287-289` — on "Not authenticated" with `keepLogin` → `navigateToLogin`.
  - `app/library/hooks/useBooksSync.ts:33,174,197`, `useLibraryRefresh.ts:29` — `user` required for native pulls.
- To get "login → see server booklists directly": swap `AuthContext`/`utils/supabase.ts` for own JWT issuance+validation (store `token`,`user` in localStorage as today), point `apiBaseUrl` at BookRoom, and let `useBooksSync.pullLibrary` + `useReplicaPull('bookshelf')` hydrate immediately on login.

## Precise files to modify (with line refs)

| Area | Files |
|---|---|
| Auth/JWT | `context/AuthContext.tsx` (whole), `utils/supabase.ts`, `utils/access.ts:311-328`, `app/auth/page.tsx`, `app/user/page.tsx`, `context/SyncContext.tsx` |
| Sync API/protocol | `libs/sync.ts:10,17-57,59-140` (endpoint/payload), `pages/api/sync.ts` (LWW logic), `pages/api/sync/replicas.ts`, `services/runtimeConfig.ts:38`, `services/environment.ts:16-20` |
| Cloud gating | `services/sync/cloudSyncProvider.ts:~90-215`, `services/sync/syncCategories.ts:96-113`, `utils/access.ts:64-156` |
| Book/progress/note sync | `hooks/useSync.ts:25-447`, `app/library/hooks/useBooksSync.ts:45-415`, `app/reader/hooks/useProgressSync.ts`, `app/reader/hooks/useNotesSync.ts`, `utils/transform.ts`, `utils/booknoteStamp.ts`, `utils/serializer.ts:23-57` |
| KOSync | `services/sync/KOSyncClient.ts`, `pages/api/kosync.ts`, `app/reader/hooks/useKOSync.ts`, `app/reader/hooks/kosyncProgress.ts`, `app/reader/hooks/kosyncPreview.ts` |
| File fetch | `libs/storage.ts:11-18,130-210`, `services/cloudService.ts:340-410`, `services/sync/file/{engine,appLocalStore,runLibrarySync,merge}.ts`, `utils/bookFileKey.ts`, `services/environment.ts` |
| Stats | `services/statistics/statsSync.ts`, `services/statistics/statisticsDb.ts`, `app/reader/components/ReadingStatsTracker.tsx:55-186`, `services/database/migrations/index.ts:140-190` |
| Replicas/书单 | `services/sync/replica{SyncManager,PullAndApply,Publish,Persist,Registry}.ts`, `libs/{crdt,replicaSyncClient,replicaSchemas}.ts`, `services/bookshelves/replica.ts`, `types/replica.ts` |
| UI gating | `app/library/page.tsx:752-769`, `app/library/components/{LibraryEmptyState.tsx:45-50,SettingsMenu.tsx:300,357,BookItem.tsx:343}`, `app/library/hooks/useLibraryRefresh.ts:29` |
| Env/build | `.env`, `.env.local.example`, `.env.tauri`, `docker/.env.example`, `app/runtime-config.js/route.ts` |

## BookRoom design implications `[INFERENCE]`
- Reuse Readest's `SyncData` wire contract and LWW field clocks (`readingStatusUpdatedAt`, etc.) in the Go server so the fork needs only endpoint/JWT swaps, not conflict-logic rewrites.
- For KOReader support, implement the KOSync endpoints (`/users/auth`,`/users/create`,`/syncs/progress/*` with `application/vnd.koreader.v1+json`) — `KOSyncClient.ts` already speaks this and `pages/api/kosync.ts` is a reference proxy.
- For 书单, mirror the `bookshelf` replica CRDT row (`fields_jsonb` with HLC envelopes) or expose it as a simpler server-owned table with a client adapter in `services/bookshelves/replica.ts`.
- Stats server: expose `/sync?type=stats` with `statBooks`/`statPages` arrays and the `UNIQUE(book_hash,page,start_time)` + "keep max duration" rule; client already handles paging/cursors.

**Could not confirm**: server-side source of `pages/api/sync.ts` beyond the exported merge helpers (grep showed 50+ matches; full body not read) — exact SQL upsert details are `[INFERENCE]` from comments; verify before reimplementing server LWW.