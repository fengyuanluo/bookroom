# OPDS research (as of 2026-11-23)

**Executive summary**
1. Implement **OPDS 1.2 (Atom) first and completely**; it is the only format every target reader understands (https://specs.opds.io/opds-1.2, published 2018-11-11, now labelled "historical").
2. **Implement OPDS 2.0 as a second serializer over the same domain model**: it became a "living standard" on 2026-05-19 (https://github.com/opds-community/specs/commit/ff42456) and is now served by Calibre 9.18+, Komga, Stump (experimental) and parsed by Thorium, KOReader (PR #15696, merged 2026-07-24) and Readest; cost is low if models are shared.
3. **Add OPDS-PSE 1.2 on the 1.2 feed only** for CBZ/CBR page streaming; exact rel `http://vaemendis.net/opds-pse/stream`, ns `http://vaemendis.net/opds-pse/ns`, `{pageNumber}`/`{maxWidth}` placeholders, `pse:count` mandatory (https://github.com/anansi-project/opds-pse/blob/master/v1.2.md:25-69).
4. **Page every acquisition feed (20–25 items)** with `next`/`previous`; real clients follow `rel=next` (Readest `readest/apps/readest-app/src/services/opds/feedChecker.ts:92-98`, KOReader `koreader/plugins/opds.koplugin/opdsbrowser.lua:1712`) and 20k-book unpaged feeds are a known failure mode (Kavita issue #3500).
5. For Go, prefer a thin hand-rolled `encoding/xml` writer or **github.com/ophymx/opds v0.5.1** (MIT, stdlib-only, both versions + PSE + Basic auth + conformance tests); archived `libopds2-go` is dead (https://github.com/opds-community/libopds2-go).

## 1. OPDS 1.2 checklist (Atom/XML)

Source: https://specs.opds.io/opds-1.2.html (sections cited inline); RELAX NG schema https://github.com/opds-community/specs/blob/master/schema/1.2/opds.rnc.

| Requirement | Detail / test |
|---|---|
| Feed type | Every feed MUST be navigation OR acquisition; acquisition identified by entries with acquisition links (§2). `Content-Type: application/atom+xml;profile=opds-catalog;kind=acquisition\|navigation`. |
| Feed elements | Atom required: `id`, `title`, `updated`, `author`; `rel=self` link SHOULD be present; `rel=start` to root SHOULD be present (§2.1, §2.6). |
| Navigation entry | `id`, `title`, `updated`, one link with `rel=subsection`/`sort/new`/etc., type `application/atom+xml;profile=opds-catalog;kind=navigation` (§2.2). MUST NOT contain publications. |
| Acquisition entry | `id` (stable URN, never changes), `title` (not `dc:title`), `updated`; at least one acquisition link with MIME `type` (§5, §5.1.1, §5.4). SHOULD use `atom:author`, `atom:category`, `atom:summary`/`content`, `dc:language`, `dc:identifier`, `dc:issued` (§5.1.1). |
| Summary | `atom:summary` MUST have `type="text"` and no child elements (§5.1.3). |
| Acquisition rels | `http://opds-spec.org/acquisition`, `/open-access`, `/borrow`, `/buy` (requires ≥1 `opds:price currencycode=ISO4217`), `/sample` (deprecated → `preview`), `/subscribe`. Registry: https://registry.opds.io/rel.html. |
| Artwork/grouping | `http://opds-spec.org/image`, `/image/thumbnail`; IANA `collection` for grouping entries into series (§5.2.2–3). |
| Paging | RFC5005 links: `first`, `last`, `next`, `previous` (also `prev` used in the wild); optional `opensearch:totalResults`, `itemsPerPage`, `startIndex` (§2.4, https://registry.opds.io/rel.html). |
| Search | Root link `rel=search` type `application/opensearchdescription+xml`; description doc has `<Url type="application/atom+xml;profile=opds-catalog;kind=acquisition" template="...q={searchTerms}"/>` (§3). |
| Auth | Minimum capability is **HTTP Basic over TLS 1.3**; 401/403 for unauthenticated (§7.2.1). Digest is *not* required (RFC2617 is cited historically); OPDS Auth 1.0 draft adds `application/opds-authentication+json` doc with `http://opds-spec.org/auth/basic` (https://drafts.opds.io/authentication-for-opds-1.0.html §2.4/§3.3). Send both a `WWW-Authenticate: Basic` header (KOReader, Foliate) and the JSON auth doc body (Thorium, Cantook). |
| Content negotiation | Same URL may serve 1.2/2.0 with `Vary: Accept`; 1.2 default. Calibre requires strictly higher q for JSON (https://github.com/kovidgoyal/calibre/blob/master/src/calibre/srv/opds.py:60-83). |
| Security | Sanitize/escape title, summary, author (XSS §7.2.4); strip control chars; implementers need not resolve external XML entities (§7.2.2); enable gzip but also accept `Accept-Encoding: identity` (KOReader sends it: opdsbrowser.lua:548). |

Minimal valid XML (well-formedness checked with Python ElementTree; no PSE so it stays core-1.2):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"
      xmlns:dcterms="http://purl.org/dc/terms/"
      xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/">
  <id>urn:uuid:11111111-1111-4111-8111-111111111111</id>
  <title>BookRoom Library</title>
  <updated>2026-11-23T00:00:00Z</updated>
  <author><name>BookRoom</name></author>
  <link rel="self" href="/opds/v1.2/catalog" type="application/atom+xml;profile=opds-catalog;kind=acquisition"/>
  <link rel="start" href="/opds/v1.2/catalog" type="application/atom+xml;profile=opds-catalog;kind=acquisition"/>
  <link rel="search" href="/opds/v1.2/search.xml" type="application/opensearchdescription+xml"/>
  <link rel="next" href="/opds/v1.2/books?page=2" type="application/atom+xml;profile=opds-catalog;kind=acquisition"/>
  <entry>
    <title>The Go Programming Language</title>
    <id>urn:uuid:22222222-2222-4222-8222-222222222222</id>
    <updated>2026-11-20T12:00:00Z</updated>
    <author><name>Alan Donovan</name></author>
    <dcterms:language>en</dcterms:language>
    <summary type="text">A guide to Go.</summary>
    <category term="Computers" label="Computers"/>
    <link rel="http://opds-spec.org/image/thumbnail" href="/covers/gopl-small.jpg" type="image/jpeg"/>
    <link rel="http://opds-spec.org/image" href="/covers/gopl.jpg" type="image/jpeg"/>
    <link rel="alternate" href="/opds/v1.2/books/42" type="application/atom+xml;type=entry;profile=opds-catalog"/>
    <link rel="http://opds-spec.org/acquisition/open-access" href="/download/42.epub" type="application/epub+zip"/>
    <link rel="http://opds-spec.org/acquisition/open-access" href="/download/42.pdf" type="application/pdf"/>
  </entry>
</feed>
```

## 2. OPDS 2.0 — status, structure, consumers

- Status: **living standard since 2026-05-19** (commit above; spec https://specs.opds.io/opds-2.0.html). Core stable since 2017; not a frozen "1.2 successor" release. Media type `application/opds+json`; publication `application/opds-publication+json`.
- Structure: Readium Web Publication Manifest model. Feed MUST contain `metadata.title`, `links` incl. `self`, and ≥1 of `navigation`, `publications`, `groups` (JSON Schema https://specs.opds.io/schema/feed.schema.json). Compact collections: `navigation`, `images`, `facets` links. `numberOfItems`/`itemsPerPage`/`currentPage` in metadata; `first`/`previous`/`next`/`last` link rels. Search: `{"rel":"search","href":"search{?query}","templated":true}`. Acq rels simplified: `acquisition`, `borrow`, `buy`, `download` (open access), `preview`, `subscribe`; legacy full URIs still valid. No PSE mapping.
- Consumers 2026 (evidence):
  - **Calibre 9.18.0 (2026-11-06)** serves both at `/opds` via Accept; new OPDS 2 catalog with Divina comic streaming, FTS (Changelog.txt:134; src/calibre/srv/opds.py:60-83). Major endorsement.
  - **Komga** `/opds/v2/catalog`, full v2 with groups/auth doc/WebPub manifests (https://komga.org/docs/guides/opds).
  - **Stump** `/opds/v2.0/catalog`, "experimental", few clients (https://www.stumpapp.dev/docs/guides/features/opds).
  - **Thorium** parses both, converts OPDS1→2 internally and handles Basic/Digest challenges and OPDS Auth docs (https://github.com/edrlab/thorium-reader/blob/develop/src/main/services/opds.ts).
  - **KOReader** added basic OPDS 2 only on 2026-07-24 after user request (issues #14681, PR #15696); initial implementation tested against one LCP catalog only.
  - **Readest** already parses both (`application/opds+json` branch: readest/apps/readest-app/src/app/opds/utils/opdsUtils.ts:22-30,245-360; foliate-js OPDS parser).
  - **Cantook/Aldiko** works best with Komga's v2 feed; covers can break (Komga docs table).
- Still effectively 1.2-only: Moon+ Reader, Librera, KyBook 3, FBReader, Panels, Chunky, most e-ink clients (Komga/Kavita/Stump compatibility tables). **Decision: implement both, but treat 1.2 as the compatibility surface; 2.0 is a thin second serializer, not a separate feature set.**

Minimal valid JSON (JSON.parse checked; schema shape: metadata+links+publications):

```json
{
  "metadata": {"title": "BookRoom Library", "numberOfItems": 12000, "itemsPerPage": 25, "currentPage": 1},
  "links": [
    {"rel": "self", "href": "https://example.com/opds/v2/catalog", "type": "application/opds+json"},
    {"rel": "next", "href": "https://example.com/opds/v2/catalog?page=2", "type": "application/opds+json"},
    {"rel": "search", "href": "https://example.com/opds/v2/search{?query}", "type": "application/opds+json", "templated": true}
  ],
  "publications": [{
    "metadata": {"@type": "http://schema.org/Book", "title": "The Go Programming Language",
                 "author": "Alan Donovan", "identifier": "urn:isbn:9780134190440", "language": "en",
                 "modified": "2026-11-20T12:00:00Z"},
    "links": [
      {"rel": "self", "href": "https://example.com/opds/v2/books/42", "type": "application/opds-publication+json"},
      {"rel": "download", "href": "https://example.com/download/42.epub", "type": "application/epub+zip"}
    ],
    "images": [{"href": "https://example.com/covers/gopl.jpg", "type": "image/jpeg", "height": 1400, "width": 800}]
  }]
}
```

## 3. OPDS-PSE 1.2

Published 2023-03-05, **unofficial** extension to OPDS 1.x; 1.2 feeds remain valid OPDS (https://github.com/anansi-project/opds-pse/blob/master/v1.2.md).
- Feed declares `xmlns:pse="http://vaemendis.net/opds-pse/ns"`; link `rel="http://vaemendis.net/opds-pse/stream"`, `type` ∈ {image/jpeg,image/gif,image/png}, `pse:count` = page count (mandatory), optional `pse:lastRead` (1-based) and `pse:lastReadDate` (Atom date).
- `href` MUST contain literal `{pageNumber}`; MAY contain `{maxWidth}`; pages **0..N-1** (counterintuitive vs 1-based lastRead). Escape `{`/`}`? No: keep literal, but escape `&` as `&amp;` in XML, e.g. `...page={pageNumber}&amp;width={maxWidth}`.
- Discovery = presence of the link in a 1.2 acquisition entry; no separate capability doc. Clients: Panels 2.14.2+ (1.2), KOReader (`koreader/plugins/opds.koplugin/opdsbrowser.lua:985-1001` reads `*:count`, `:lastRead`; `opdspse.lua:112-113` substitutes both placeholders), Readest (`pseStream.ts:35-40`, also handles `%7B...%7D`), ComicVerse, Easemic; older Panels/Gutter/Longbox = PSE 1.0/1.1. Servers: Komga, Kavita, Codex, LANraragi. Calibre rejected PSE 1.2 ("badly designed extension", requires page counts up front, no AVIF/JXL; PR #3404 closed 2026-10-28) and instead shipped Divina streaming in OPDS 2.0 — a caution for any new design.

## 4. Komga vs Kavita (source evidence)

| | Komga | Kavita |
|---|---|---|
| Versions | v1 `/opds/v1.2/*` and v2 `/opds/v2/*`; v1 root carries `alternate` link to v2 (`komga/.../v1/OpdsController.kt:79-90,162`). | OPDS 1.2 only (`Kavita.Server/Controllers/OPDSController.cs`). |
| Root URL | `/opds/v1.2/catalog`, `/opds/v2/catalog`. | `/api/opds/{apiKey}` — API key embedded in path for clients lacking header auth (`OpdsService.cs:49,61-63`). |
| Paging | `?page=` and prev/next via Spring `Page` (`OpdsController.kt:130-148`); v2 metadata `itemsPerPage/currentPage/numberOfItems` (`v2/dto/Opds2Dto.kt:38-54`). | `?pageNumber=`, 1-based, PageSize=20 (`OpdsService.cs:47-49`), prev/next + OpenSearch total/startIndex (`:968-1040`). |
| Grouping | Navigation feeds for libraries, series, collections, read lists, publishers; collection/series entries link to sub-feeds (`OpdsController.kt:83-101,607-694`); v2 uses `groups[]` (`:250-287`). | Dedicated feeds for collections, reading lists, want-to-read, smart filters, series/volume/chapter hierarchy (`OPDSController.cs:85-344`); "Continue From" alias entry + progress glyphs in titles (Kavita wiki). |
| PSE | `{pageNumber}` path, JPEG fallback `?convert=jpeg`, `pse:count/lastRead/lastReadDate` (`OpdsController.kt:714-727,750-758`; `v1/dto/OpdsLink.kt:76-90`). | Query-string template `...pageNumber={pageNumber}`, `image/jpeg`, count/lastRead (`OpdsService.cs:1277-1290`, `FeedLink.cs`). |
| Auth | Spring Security Basic + OPDS Auth doc `/opds/v2/auth` (DTO `OpdsAuthDto.kt`). | URL API key; Download action has separate policy (`OPDSController.cs:639-651`). |
| Quirks | Content field replaces `\n` with `<br/>` after escaping (`OpdsEntry.kt:19`). Unicode oembed; page streaming needs PAGE_STREAMING role. | Serializer strips invalid XML chars, rewrites `utf-16`→`utf-8` for Chunky (`OpdsService.cs:851-862`); prior bug: control char 0x02 broke feeds at page 6 (issue #3500); acquisition uses `.../acquisition/open-access` even for downloads (`FeedLinkRelation.cs`). |

## 5. Reader clients

| Client | Versions | Notes/breakages |
|---|---|---|
| KOReader | 1.2 + basic 2.0 (Jul 2026) | Prefers JSON via `Accept` (`opdsbrowser.lua:36,548`); tolerant custom XML parser strips CDATA/comments and rewrites content (`opdsparser.lua:68-110`); follows `next`; PSE; HTTP Basic via URL user/pass (`opdsclient.lua:47-56`); rejects HTTPS→HTTP redirects. |
| Readest (fork target) | 1.2 + 2.0 + PSE | Uses foliate-js parser, `getNextPageUrl`, OpenSearch and templated search; already supports kosync + OPDS credentials (paths above). |
| Thorium | 1.2 + 2.0 | Converts 1.2→2.0 internally; Basic/Digest WWW-Authenticate; OPDS Auth doc. |
| Calibre | both since 9.18 (Nov 2026) | Default 1.x unless Accept q(JSON)>q(Atom); `max_opds_items` paging with `offset` (srv/opds.py:478-485,807-814). |
| Moon+ Reader | 1.2 | Tested OK with Komga/Kavita/Stump; no OpenSearch, no PSE, no Basic header support (Stump adds API key in URL). |
| KyBook 3 | 1.2 | Tested OK with Komga (OpenSearch yes, PSE no); Stump untested. No 2.0 evidence found `[INFERENCE]`. |
| Panels | 1.2 | PSE 1.0 (2.8+), 1.1 (2.9.7+), 1.2 (2.14.2+), OpenSearch ≥3.0; no 2.0 evidence `[INFERENCE]`. |
| Librera | 1.2 | Works but shows file names not titles (Stump); Cantook v2 fails there. |
| FBReader | 1.2 | CBZ download issues, PDF premium-only (Komga table). |
| Cantook/Aldiko | 2.0 preferred | Cover rendering issues; no v1 header auth support. |

## 6. Go libraries

| Library | Verdict |
|---|---|
| `github.com/ophymx/opds` v0.5.1 (2026-08-31, MIT, Go 1.25) | Only mature all-in-one: version-neutral model, `opds1`/`opds2` codecs, `opdshttp` handler (negotiation, paging, search, Basic + auth doc, PSE page endpoint, optional progression), `opdsclient`, conformance tests against official RNC/JSON schemas (~6.3k LOC incl. tests). README: https://github.com/ophymx/opds. May be too opinionated/young for a lean server — take it as reference or vendor only `opds1`+`opds2` codecs. |
| `opds-community/libopds2-go` | Archived 2023-12-27. Skip. |
| `dubyte/dir2opds/opds` v1.11 | OPDS **1.1** builders only, GPL-3.0. Skip (GPL, wrong version). |
| `htol/bopds`, `littfed/go-opds` | Tiny/obscure (v0.1.x), no conformance evidence. Skip. |

`encoding/xml` pitfalls: tag names are literal (no namespace-URI scoping) → write `xmlns:...` attrs and `pse:count` prefixes yourself, as ophymx does (`opds1/xml.go:5-18`, `atom.go:194-206`); struct field order fixes output order, so declare feed metadata before `link`/`entry`; `,chardata` is escaped but markup in summaries must be forced to `type="text"`; `&` in PSE href must be `&amp;`; XML declaration/UTF-8 and control-char sanitization are your job (Kavita's 0x02 bug); Go's decoder does not expand external entities (safe by default). A ~200-line hand-rolled writer is viable if using only the subset above; use ophymx if wanting schema conformance and 2.0 for free.

## 7. Feed size / paging strategy

- 20k books ÷ 25/page ≈ 800 pages. Both reference servers page at 20 (`OpdsService.cs:47`; Komga Spring default), Calibre uses `max_opds_items`; none serve a 20k-entry acquisition feed. **Always page.**
- Use `?page=` (Komga/Calibre style) **1-based** plus `next`/`previous`; for deep pages use keyset pagination on `(updated_at, id)` with a stable sort, cache the total count, and add `opensearch:totalResults/itemsPerPage/startIndex`. `[INFERENCE]` SQLite `OFFSET` at 20k rows is tolerable but keyset avoids regressions.
- gzip + `Vary: Accept`, ETag/Last-Modified: KOReader caches with `If-None-Match`/`If-Modified-Since` and 304 (`opdsbrowser.lua:524-618`) but explicitly requests `Accept-Encoding: identity`; do not gzip-only. Readest/Komga-style clients follow `next` recursively for subscriptions — cap crawl rate and support 304 there too.
- Avoid `pse:count` computation for every comic in a large feed (needs opening archives) — compute lazily or accept Calibre's criticism; store page count in DB at import.

## 8. Test checklist (turn into Go table tests)

- [ ] Root feed: exactly one root, `id/title/updated/author`, `self`+`start`, nav vs acq `kind` param correct.
- [ ] Nav entries have no acquisition links; acq entries have ≥1 acq link with MIME type; stable `id` across pagination.
- [ ] `summary type="text"` no children; control chars/HTML escaped; CJK titles intact (UTF-8).
- [ ] All six acq rels recognized; `buy` always has `opds:price`.
- [ ] `next`/`previous`/`first`/`last` present on page 2 of a 20k library; no `previous` on page 1, no `next` on last; OpenSearch counters present.
- [ ] OpenSearch doc linked with correct media type, `template` uses `{searchTerms}`, search returns acq feed.
- [ ] Unauthenticated request → 401 with `WWW-Authenticate: Basic` and OPDS Auth JSON doc; authenticated request works with Basic header and URL-embedded credentials variants.
- [ ] PSE link: rel/ns/type/count correct, both placeholders literal, page 0 and page N-1 return images, `lastRead` 1-based, `&amp;` in XML.
- [ ] OPDS 2: schema validate feed/publication, `self` link, `application/opds+json`, templated search, groups/facets optional, PSE absent.
- [ ] Content negotiation: default 1.2; `Accept: application/opds+json` → 2.0; `Vary: Accept`; 304 on conditional GET.
