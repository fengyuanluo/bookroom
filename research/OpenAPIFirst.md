# OpenAPI-first / codegen evaluation for BookRoom (research, 2026-11-23)

Greenfield project. Readest examined at v0.12.20 (`apps/readest-app/package.json:3`). All versions below checked against GitHub releases / npm registry on 2026-11-23. `[INFERENCE]` marks judgement or unverified claims.

## 1. Go OpenAPI tooling decision table

| Tool | Version | Mode | Gin? | Handler logic in plain Go | Churn | TS client story |
|---|---|---|---|---|---|---|
| **oapi-codegen** | v2.9.0 (released ~Nov 2026; needs Go **1.26** to build *and* run generated code; runtime ≥v1.9.1) | spec-first | ✅ `gin-server`, plus `strict-server` (README L470-486, 528-568) | strict handlers are `func(ctx, typedReq) (typedResp, error)`, thin adapters over a service layer; generated file is separate, so domain code untouched | generated file fully replaced; churn confined to gen.go; new operation = compile error until implemented | not its strength; use TS tooling below |
| **huma** | v2.41.0 | code-first (OpenAPI 3.1 generated from typed Go handlers at runtime) | ✅ `adapters/humagin` (huma.rocks/features/bring-your-own-router) | handlers are typed Go funcs; spec can't drift; problem+json default | zero generated files; schema changes are Go struct edits | export spec then TS-generate; needs a build step to dump spec |
| **go-swagger** | v0.36.8 | Swagger **2.0 only** (README: "golang implementation of Swagger 2.0"; no OpenAPI 3) | via net/http | generates full server scaffolding | heavy; opinionated | outdated Swagger tooling; skip |
| **Fuego** | v0.18.0 | code-first from signatures/generics, generates OpenAPI | can plug into Gin (`Adaptors` section) | Fuego-flavoured handlers with its own context + options; can fall back to std handlers | no generated files | would need exported spec; smaller ecosystem |
| **swag annotations** | v2.0.0-rc6 (2y+ in RC; v1 is Swagger 2.0, v2 `--v3.1`) | code-first comments → spec | ✅ `gin-swagger` | normal Gin handlers; annotations duplicate handler signature in comments | no gen churn for Go code, but **silent drift**: rename a field/status and spec stays stale (Encore comparison, Nov 2026) | spec quality depends on annotation discipline |

**Choice: oapi-codegen v2.9.0 + Gin v1.12.0 (needs Go 1.25+) + Go 1.26 toolchain.** Reasons: BookRoom has **two external client ecosystems** (forked Readest and agent/MCP generators) that need a stable contract as source of truth; spec-first gives compile-time drift detection (Encore: "compiler catches drift"), and every other artifact (TS types, MCP tools, docs) derives from one YAML. Strict-server keeps handlers thin: generated `ServerInterface`/`StrictServerInterface` is implemented in a separate `api/impl.go` (oapi-codegen gin example: `examples/minimal-server/gin/api/impl.go` shows a 20-line typed handler), delegating to services. Caveat: strict-server with Gin still receives `*gin.Context` in the generated layer; issue #1992 (open, 2025-05-21) asks for stdlib `context` in Gin strict mode — plan for a thin `ctx` adaptation, not a fight about it.
Validation: generated code only checks required headers/basic binding; add `github.com/oapi-codegen/gin-middleware` v1.2.1 (kin-openapi `openapi3filter`, `KeepValidatedValue` on by default so strict handlers bind exactly the validated value; README). Security (JWT) is **not** enforced by generated code (README L1581+) — implement JWT Gin middleware explicitly.
`[INFERENCE]` Huma would be the better solo-dev DX (faster iteration, no regen, RFC9457 built-in), but swaps "spec is truth" for "Go types are truth"; for an agent-facing API with multiple generated consumers, spec-first wins.

## 2. TS client generation — what Readest already does

Readest has **no OpenAPI/client generator** in its lockfile (`pnpm-lock.yaml` no openapi/hey-api/orval/axios entries; the `openapi` grep hits are unrelated app data). Current pattern is hand-written `fetch` + runtime-configured base URL + manual bearer token + ad-hoc response typing:
- `apps/readest-app/src/services/environment.ts:16-45`: `getBaseUrl()` runtime config → `API_BASE_URL` → `NEXT_PUBLIC_API_BASE_URL`; `getAPIBaseUrl()` adds `/api`.
- `apps/readest-app/src/utils/fetch.ts:3-28`: `fetchWithTimeout` (10 s), `fetchWithAuth` adds `Authorization: Bearer …`, reads `errorData.error` from JSON, throws.
- `apps/readest-app/src/libs/sync.ts:4,6,35-63`: `SYNC_API_ENDPOINT = getAPIBaseUrl() + '/sync'`, hand-written `SyncResult`, `StatPageRecord`, `StatBookRecord` interfaces; cursor math via `updated_at_ms` fields.
- `apps/readest-app/src/libs/storage.ts:14-19`: hardcoded `API_ENDPOINTS` map of `/storage/*` paths; response shapes cast inline (`const { uploadUrl }: {uploadUrl: string} = await response.json()` at line 53).
- `zod ^4.0.8` (`apps/readest-app/package.json:221`) is used in a few services (e.g. `src/libs/replicaSchemas.ts:2`), not for API clients.

| Tool | Version (npm 2026-11-23) | Model | Fit for BookRoom fork |
|---|---|---|---|
| **openapi-typescript + openapi-fetch** | 7.13.0 / 0.17.0 | types-only from spec (~no runtime for types; fetch wrapper ~6 kB), `createClient<paths>({baseUrl})`, middleware `onRequest`/`onResponse` for auth/errors (openapi-ts.dev/openapi-fetch, -/middleware-auth) | **Best fit**: preserves Readest's existing fetch/auth structure; swap `fetchWithAuth` for one generated client with auth middleware; generated types replace `SyncResult`/`StatPageRecord` copies; works with SWR (already in lockfile: `swr@2.4.1`) |
| Orval | 8.49.0 | full client + TanStack Query hooks via `orval.config.ts` | heavier: Readest uses SWR, not TanStack Query; pulls in a second data-fetching paradigm |
| @hey-api/openapi-ts | 0.99.0 | plugin-based SDK, Zod validators, fetch/Axios/Ky, TanStack Query/SWR plugins (heyapi.dev/docs/openapi/typescript/get-started) | strong runner-up if you later want generated Zod schemas + SWR hooks; 0.x API churn risk |

**Recommendation:** openapi-typescript 7.13.0 + openapi-fetch 0.17.0. Keep `getAPIBaseUrl()` from environment.ts as the `baseUrl` and keep `getAccessToken()` from utils/access as an `onRequest` middleware; route *all* new BookRoom endpoints through the generated client first, migrate `/sync` and `/storage/*` incrementally.

## 3. "Agent-friendly" concretely

| Practice | Evidence | Concrete effect |
|---|---|---|
| Single spec file (bundled, checked in) | MCP tools derive names/descriptions/inputSchema from `operationId`, `summary`/`description`, parameters (MCP spec 2026-07-28 server/tools; Scalar guide, Sep 2026) | generated tools are only as good as the spec; require `operationId` + descriptions via Redocly lint |
| Description quality matters more than tool count | FastMCP docs warn auto-converted OpenAPI servers perform "significantly worse" than curated tool sets; J. Lowin "Stop Converting Your REST APIs to MCP" (jlowin.dev blog) | curate an `x-mcp` include list; don't expose all ~50 admin paths |
| Typed errors | RFC 9457 §3.1: `type` URI is the **primary identifier**, `status` advisory, `title` stable, `detail` human-only, extensions for machine data | LLM can switch on `type` without parsing prose; browsers get human `detail` |
| Deterministic codegen | pin generator versions, check in generated output, CI `git diff --exit-code` after regen `[INFERENCE]` | reproducible diffs; agents see generated types in PRs |
| AGENTS.md | agents.md: "README for agents", setup commands/code style; 60k+ repos; no AGENTS.md/CLAUDE.md in readest today (404 on raw main, checked 2026-11-23) | put setup commands, codegen commands, layer rules here; coding agents stop guessing |
| llms.txt | answerdotai/llms-txt proposal (v2, updated 2026-08-10): site-root index of useful pages; Readest has none | serve `/llms.txt` pointing at OpenAPI, error taxonomy, auth guide |
| MCP server over API | MCP spec 2026-07-28 tools: tool = name + description + `inputSchema` + annotations; tool names SHOULD be 1–128 chars, no spaces/commas, unique | expose read-only ops with `readOnlyHint`; destructive ops annotated; agents can discover |
| Tool schemas are JSON Schema | Anthropic tool use: `input_schema` JSON Schema + description; OpenAI function calling: JSON-schema-defined tools (developers.openai.com/api/docs/guides/function-calling) | OpenAPI parameter schemas map directly to tool `inputSchema` |

## 4. Generating MCP from OpenAPI — viable options (2026)

| Option | Install / command | Notes |
|---|---|---|
| **openapi-mcp-generator** | npm **5.0.1**; `npx openapi-mcp-generator@5 -i openapi/bookroom.yaml -o mcp/ --force` (add `--transport=streamable-http --port 3000`; `--include-tags`/`--exclude-tags` to curate; `--output-schema`) | MIT; TS project you own; MCP 2026-07-28 + older clients; JSON-Schema validation; `x-mcp` filtering; Node ≥22.19. v5 is a breaking rewrite (README upgrade notes) |
| **mcp-openapi-proxy** | PyPI **0.4.0**; `uvx mcp-openapi-proxy` with `API_URL=… API_KEY=… TOOL_WHITELIST=…` env (README) | zero code, dynamic at startup, verified with Codex/Gemini/opencode; dual-stack MCP 1.x/2.x, stateless Streamable HTTP; less control over tool descriptions |
| **FastMCP from_openapi** | Python fastmcp **4.2.5**; `mcp = FastMCP.from_openapi(openapi_spec=spec, client=httpx.AsyncClient(base_url=...))`, `mcp.run()`; `RouteMap(..., MCPType.EXCLUDE)` to curate | runtime conversion; docs explicitly recommend curated tools for production |
| Speakeasy / Stainless / liblab | hosted or CLI generation | vendor lock-in / heavier; not needed for a self-hosted solo project |

**Recommendation:** start with `openapi-mcp-generator` checked in as `mcp/` and regenerate in the same Makefile target (or FastMCP with an explicit 8–12 tool allowlist). `[INFERENCE]` Do not expose the whole CRUD surface: use GET-only tools for discovery (books, lists, progress), and annotate mutating tools. Note FastMCP's warning that mirrored REST APIs make worse agent tools than curated ones.

## 5. Does spec-first slow a solo dev?

Evidence (all 2026-11-23): Encore's comparison – spec-first requires reviewing YAML like code but compiler catches drift; code-first (Huma/Fuego) derives spec from types, "spec cannot drift"; swag annotations are "a second description … rename a field or change a status code and the spec stays wrong until someone updates the comment"; DEV post "Maintaining OpenAPI specs in Go at scale" lists annotation tools' failure mode as "silent drift from struct/handler edits". OpenAPI best-practices (learn.openapis.org) recommends design-first. Real complaint: `oapi-codegen` issue #1782 (multipart upload in strict-server, open since 2024-09, updated 2026-03) and #1992 (Gin strict context) — strict mode has sharp edges; keep spec simple and don't fight the generator.
Verdict `[INFERENCE]`: for 10–20k books but a small API surface (~30–50 ops), spec-first costs ~1 extra file edit per endpoint and pays back via (a) TS types for Readest, (b) MCP tools, (c) compile-time enforcement. Code-first (Huma) would be faster for week 1 but makes the contract a build artifact of Gin code, which is worse for the fork's independent release cycle. **Hybrid fallback:** if regen friction becomes real, keep the same YAML but generate it from Go with Huma in a separate tool and freeze the YAML in CI — not needed initially.

## 6. Recommended pipeline (exact versions, commands, layout)

Toolchain: **Go 1.26**, gin **v1.12.0**, oapi-codegen **v2.9.0** (`go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.9.0`), runtime **v1.9.1**+, gin-middleware **v1.2.1**, Redocly CLI **2.82.0**, oasdiff (`go install github.com/oasdiff/oasdiff@latest`), openapi-typescript **7.13.0**, openapi-fetch **0.17.0**, openapi-mcp-generator **5.0.1**. Pin exact versions in `tools.go`/`package.json`.

File layout (server, single binary):
```
api/openapi.yaml                 # single source of truth, OpenAPI 3.1
api/generated/server.gen.go      # gin-server + strict-server + models + embedded-spec
api/generated/config.yaml        # oapi-codegen config, schema-pinned
api/handlers.go                  # implements StrictServerInterface; calls services
internal/service/...             # business logic, no Gin, no generated types except DTO mapping
internal/store/...               # SQLite
internal/jwt/...                 # auth middleware
web/src/client/api.d.ts          # openapi-typescript output (git-checked)
web/src/client/index.ts          # createClient<paths> + auth/error middleware
mcp/                             # generated MCP server (git-checked)
Makefile
```

`api/generated/config.yaml`:
```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/oapi-codegen/oapi-codegen/v2.9.0/configuration-schema.json
package: generated
output: server.gen.go
generate:
  models: true
  gin-server: true
  strict-server: true
  embedded-spec: true
output-options:
  skip-prune: false
```

Makefile targets:
```make
BIN := $(shell go env GOPATH)/bin
generate:
	cd api && $(BIN)/redocly lint openapi.yaml --extends=recommended-strict
	cd api && $(BIN)/oapi-codegen --config=generated/config.yaml openapi.yaml
	cd api && $(BIN)/oapi-codegen -generate types -package bookroom -o internal/bookroomclient/types.gen.go openapi.yaml # optional Go client for CLI/tools
	cd web && pnpm exec openapi-typescript ../api/openapi.yaml -o src/client/api.d.ts
	cd web && npx openapi-mcp-generator@5.0.1 -i ../api/openapi.yaml -o ../mcp --force --include-tags=public --output-schema
check-generated: generate
	git diff --exit-code -- api/generated web/src/client mcp
breaking:
	git show origin/main:api/openapi.yaml > /tmp/bookroom-base.yaml && $(BIN)/oasdiff breaking --fail-on ERR /tmp/bookroom-base.yaml api/openapi.yaml
```
(`oasdiff breaking --fail-on ERR <base> <revision>` — verified against oasdiff's BREAKING-CHANGES.md; levels: ERR definite break, WARN unverifiable, INFO non-breaking. oasdiff also has VERSIONING.md rules to "fail the build when a breaking change is released without a major version bump".)

Server wiring: `r := gin.New(); r.Use(jwt.Middleware()); middleware.OapiRequestValidatorWithOptions(spec, ...)` (gin-middleware v1.2.1); `generated.RegisterHandlers(r, handlers.New(services...))` with strict handler wrapped by `generated.NewStrictHandler`. One `POST /books`, `GET /books`, `PATCH /books/{id}` etc. per operation; handlers only map types and call `service`. Keep pagination cursor logic in `internal/service`.

## 7. Error envelope + pagination

**Errors — RFC 9457 `application/problem+json`** (rfc-editor.org/rfc/rfc9457): one `Problem` schema in `components/schemas`, referenced from every 4xx/5xx response. Fields:
```json
{"type":"https://bookroom.local/probs/book-not-found","title":"Book not found",
 "status":404,"detail":"No book with id 42","instance":"/books/42",
 "code":"book_not_found","retryable":false,"field":"id"}
```
Use stable **absolute** `type` URIs as machine key (§3.1.1: primary identifier; relative URIs resolve per-document and cause confusion), stable `title`, human-only `detail` (consumers SHOULD NOT parse it), `code` as a short enum for the TS client, `retryable` for agent backoff, `field` for form validation. Huma uses problem+json by default (its README L39); for oapi-codegen you define it in the spec and write a small Gin error renderer. `[INFERENCE]` This envelope lets browser code switch on `code` and LLM agents on `type`, without either parsing prose. Readest's current `fetchWithAuth` expects `errorData.error` (utils/fetch.ts:27) — during migration map `problem.detail ?? problem.title` into that field for backward compatibility, not forever.

**Pagination — opaque cursor** for list endpoints (books, reading lists, search): request `?limit=…&cursor=…`, response `{items: [...], nextCursor: string|null, total?: number}`; cursor is an opaque base64 of (sort key, last id) so ordering changes don't shift pages; use `?page=` only for the human UI's page numbers if needed. `[INFERENCE]` Cursor avoids OFFSET cost at 10–20k rows and matches Readest's existing cursor-style sync (`since`, `limit` in libs/sync.ts:80-84). Give cursors 24 h+ validity and include a stable sort default (`updated_at DESC, id DESC`). Avoid `total` on hot search paths (COUNT cost).

## 8. Versioning / not breaking the fork

- URL prefix `/api/v1` (maps to Readest's existing `/api` base — set `getAPIBaseUrl()`+`/v1` in the fork's client config); additive changes within v1, new prefix `/api/v2` only for breaking changes. Header/media-type versioning adds permanent complexity for two consumers `[INFERENCE]`; path versioning is simplest to route, cache, and debug.
- Additive-safe within v1: new optional request fields, new response fields, new enum values in **responses** only if clients ignore unknown values, new endpoints, new error `type` URIs. Breaking: removing/renaming fields, changing a field's type, tightening a request enum, changing auth scheme.
- CI gate: Redocly lint + `oasdiff breaking` on every PR against `origin/main`; oasdiff docs explicitly warn to fail a breaking change without a major bump. Check generated artifacts into git so the fork pins a known client version via a git submodule or published tarball.
- In the Readest fork: generated client lives in one module (`web/src/client`); pin generator versions in `package.json`, commit `api.d.ts`, never hand-edit generated files. Since `getBaseUrl()` is runtime-overridable (environment.ts:16-19), a self-hosted user can point the fork at any BookRoom server; keep the server's `/api/v1/openapi.yaml` reachable anonymously and include an `X-BookRoom-API-Version: 1.x.y` response header for diagnostics.
- Deprecation: add `deprecated: true` in the spec + `Sunset`/`Deprecation` headers (RFC 8594 `[INFERENCE]` — not verified in this pass); keep old paths for ≥1 fork release.

## Executive summary (5 lines)
1. Use spec-first **oapi-codegen v2.9.0 + strict-server + Gin 1.12.0/Go 1.26**; thin handlers over services; add gin-middleware v1.2.1 + explicit JWT.
2. For Readest, use **openapi-typescript 7.13.0 + openapi-fetch 0.17.0**: it has no existing generator and hand-rolls fetch/auth/types; this drops in cleanly.
3. Agent-friendly = one linted spec, stable operationIds/descriptions, RFC 9457 problem+json with stable `type` URIs, opaque cursors, checked-in deterministic output, AGENTS.md/llms.txt, curated MCP tools.
4. MCP generation is viable: **openapi-mcp-generator 5.0.1** (code-owned, MIT) or **mcp-openapi-proxy 0.4.0** (zero-code) — curate 8–12 tools, don't mirror CRUD wholesale.
5. Guard the fork with `/api/v1`, oasdiff breaking checks in CI, committed generated artifacts, and additive-only evolution.
