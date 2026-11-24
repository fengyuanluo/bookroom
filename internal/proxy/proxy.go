// Package proxy forwards the Readest web build's cross-origin calls (OPDS catalogs, KOSync, BookOrbit,
// Grimmory, Hardcover, Notion), mirroring Readest's Next.js proxy routes. Every connection goes through a
// dial-time SSRF guard (code.dny.dev/ssrf), so DNS rebinding cannot reach private networks; operators can
// allow LAN targets (a self-hosted Komga next door) or name trusted hosts.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"code.dny.dev/ssrf"

	"bookroom/internal/apperr"
)

// Blocked is returned for a target the policy forbids.
var Blocked = errors.New("proxy: target not allowed")

// Proxy holds the guarded clients.
type Proxy struct {
	guarded      *http.Client // dial guard on, no automatic redirects
	trusted      *http.Client // no guard, for operator-trusted hosts
	trustedHosts map[string]bool
	allowPrivate bool

	HardcoverEndpoint string // https://api.hardcover.app/v1/graphql
	NotionBase        string // https://api.notion.com/v1
}

// New builds the proxy. allowPrivate permits RFC 1918 / ULA targets (never loopback or link-local).
func New(allowPrivate bool, trustedHosts []string) *Proxy {
	opts := []ssrf.Option{ssrf.WithAnyPort()}
	if allowPrivate {
		opts = append(opts,
			ssrf.WithAllowedV4Prefixes(netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
				netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("100.64.0.0/10")),
			ssrf.WithAllowedV6Prefixes(netip.MustParsePrefix("fc00::/7")))
	}
	guard := ssrf.New(opts...)
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	p := &Proxy{
		guarded: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, Control: guard.Safe}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				MaxIdleConnsPerHost:   4,
			},
			CheckRedirect: noRedirect,
		},
		trusted:           &http.Client{CheckRedirect: noRedirect, Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}},
		trustedHosts:      map[string]bool{},
		allowPrivate:      allowPrivate,
		HardcoverEndpoint: "https://api.hardcover.app/v1/graphql",
		NotionBase:        "https://api.notion.com/v1",
	}
	for _, h := range trustedHosts {
		if h = normalizeHost(h); h != "" {
			p.trustedHosts[h] = true
		}
	}
	return p
}

func normalizeHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

// blockedName mirrors Readest's isBlockedHost for names the dial guard cannot judge: localhost, mDNS and
// internal suffixes, and single-label hosts. IP literals are left to the dial guard.
func (p *Proxy) blockedName(host string) bool {
	h := normalizeHost(host)
	if h == "" {
		return true
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if p.allowPrivate {
		return false
	}
	return strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".lan") || !strings.Contains(h, ".")
}

// clientFor validates a target URL and picks the client.
func (p *Proxy) clientFor(u *url.URL) (*http.Client, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: only http(s) URLs are supported", Blocked)
	}
	if p.trustedHosts[normalizeHost(u.Hostname())] {
		return p.trusted, nil
	}
	if p.blockedName(u.Hostname()) {
		return nil, fmt.Errorf("%w: this URL is not allowed", Blocked)
	}
	return p.guarded, nil
}

// do sends one request, translating guard rejections into Blocked.
func (p *Proxy) do(ctx context.Context, method, target string, header http.Header, body []byte) (*http.Response, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid URL", Blocked)
	}
	c, err := p.clientFor(u)
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	resp, err := c.Do(req)
	if err != nil && (errors.Is(err, ssrf.ErrProhibitedIP) || errors.Is(err, ssrf.ErrProhibitedNetwork) || errors.Is(err, ssrf.ErrProhibitedPort) || errors.Is(err, ssrf.ErrInvalidHostPort)) {
		return nil, fmt.Errorf("%w: requests to private/internal addresses are not allowed", Blocked)
	}
	return resp, err
}

// Response is an upstream answer to stream back.
type Response struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// Fail builds a proxy-style JSON error.
func Fail(status int, msg string, details ...string) *apperr.Error {
	body := map[string]string{"error": msg}
	if len(details) > 0 {
		body["details"] = details[0]
	}
	return apperr.WithBody(status, body)
}

func failFor(err error) *apperr.Error {
	if errors.Is(err, Blocked) {
		return Fail(http.StatusBadRequest, strings.TrimPrefix(err.Error(), Blocked.Error()+": "))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Fail(http.StatusGatewayTimeout, "Request timeout - the upstream server took too long to respond")
	}
	return Fail(http.StatusInternalServerError, "Proxy request failed", err.Error())
}

// ── OPDS ──

const opdsUserAgent = "BookRoom/0.1 (OPDS proxy; Readest compatible)"

// OPDSRequest is the query of /api/opds/proxy.
type OPDSRequest struct {
	URL, Auth, Stream, Headers string
	Head                       bool
}

// OPDS fetches a catalog resource, following up to five redirects and re-checking every hop.
func (p *Proxy) OPDS(ctx context.Context, r OPDSRequest) (*Response, error) {
	if r.URL == "" {
		return nil, Fail(http.StatusBadRequest, "Missing URL parameter. Usage: /api/opds/proxy?url=YOUR_OPDS_URL")
	}
	start, err := url.Parse(r.URL)
	if err != nil || start.Host == "" {
		return nil, Fail(http.StatusBadRequest, "Invalid URL format")
	}
	header := http.Header{}
	header.Set("User-Agent", opdsUserAgent)
	header.Set("Accept", "application/atom+xml, application/xml, text/xml, application/json, */*")
	custom := map[string]string{}
	if r.Headers != "" {
		_ = json.Unmarshal([]byte(r.Headers), &custom)
	}
	for k, v := range custom {
		if k = http.CanonicalHeaderKey(strings.TrimSpace(k)); k != "" && !strings.ContainsAny(k+v, "\r\n") {
			header.Set(k, v)
		}
	}
	if r.Auth != "" {
		header.Set("Authorization", r.Auth)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	method := http.MethodGet
	if r.Head {
		method = http.MethodHead
	}
	current := start
	var resp *http.Response
	for hop := 0; ; hop++ {
		resp, err = p.do(ctx, method, current.String(), header, nil)
		if err != nil {
			cancel()
			return nil, failFor(err)
		}
		loc := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
			break
		}
		resp.Body.Close()
		if hop >= 5 {
			cancel()
			return nil, Fail(http.StatusBadRequest, "Too many redirects")
		}
		next, err := current.Parse(loc)
		if err != nil {
			cancel()
			return nil, Fail(http.StatusBadRequest, "Invalid redirect")
		}
		if next.Scheme+"://"+next.Host != current.Scheme+"://"+current.Host {
			// Credentials and custom headers never follow a cross-origin redirect.
			header = http.Header{}
			header.Set("User-Agent", opdsUserAgent)
			header.Set("Accept", "application/atom+xml, application/xml, text/xml, application/json, */*")
		}
		current = next
	}
	out := &Response{Status: resp.StatusCode, Header: isolate(resp.Header), Body: &cancelBody{resp.Body, cancel}}
	// A 401 would make the browser show its own login prompt; Readest answers 403 instead.
	if resp.StatusCode == http.StatusUnauthorized {
		out.Status = http.StatusForbidden
	}
	isFile := r.Stream == "true" || strings.Contains(strings.ToLower(resp.Header.Get("Content-Disposition")), "attachment")
	if resp.StatusCode >= 400 || isFile || r.Auth != "" || len(custom) > 0 {
		out.Header.Set("Cache-Control", "no-store")
	} else {
		out.Header.Set("Cache-Control", "public, max-age=300")
	}
	out.Header.Set("Vary", "Authorization")
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !r.Head && !isFile && strings.Contains(ct, "xml") && resp.StatusCode < 400 {
		// Many catalogs emit bare '&'; escape them so browser XML parsers accept the feed.
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		cancel()
		if err != nil {
			return nil, failFor(err)
		}
		data = SanitizeXML(data)
		out.Body = io.NopCloser(bytes.NewReader(data))
		out.Header.Set("Content-Length", fmt.Sprint(len(data)))
	}
	if r.Head {
		resp.Body.Close()
		cancel()
		out.Body = http.NoBody
	}
	return out, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

var allowedHeaders = map[string]bool{
	"Content-Type": true, "Content-Length": true, "Content-Disposition": true, "Content-Range": true, "Accept-Ranges": true,
	"Etag": true, "Last-Modified": true, "Www-Authenticate": true,
}

var safeMIME = regexp.MustCompile(`^(?:image/(?:png|jpeg|gif|webp|avif|svg\+xml)|audio/[a-z0-9.+-]+|video/[a-z0-9.+-]+|text/(?:plain|xml)|application/(?:[a-z0-9.+-]+\+(?:xml|json)|xml|json|pdf|epub\+zip|zip|octet-stream))$`)

// isolate keeps only safe upstream headers and neutralizes active content, as Readest's proxy does.
func isolate(h http.Header) http.Header {
	out := http.Header{}
	for k, v := range h {
		if allowedHeaders[http.CanonicalHeaderKey(k)] {
			out[http.CanonicalHeaderKey(k)] = v
		}
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(out.Get("Content-Type"), ";")[0]))
	if !safeMIME.MatchString(mime) {
		out.Set("Content-Type", "application/octet-stream")
	}
	out.Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	out.Set("X-Content-Type-Options", "nosniff")
	return out
}

var entityRe = regexp.MustCompile(`^&(?:amp|lt|gt|quot|apos|#[0-9]+|#x[0-9a-fA-F]+);`)

// SanitizeXML escapes '&' that does not start an entity, outside CDATA sections.
func SanitizeXML(b []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(b))
	inCDATA := false
	for i := 0; i < len(b); i++ {
		switch {
		case !inCDATA && bytes.HasPrefix(b[i:], []byte("<![CDATA[")):
			inCDATA = true
		case inCDATA && bytes.HasPrefix(b[i:], []byte("]]>")):
			inCDATA = false
		case !inCDATA && b[i] == '&' && !entityRe.Match(b[i:min(len(b), i+12)]):
			out.WriteString("&amp;")
			continue
		}
		out.WriteByte(b[i])
	}
	return out.Bytes()
}

// ── JSON forwarders (KOSync, BookOrbit, Grimmory) ──

var kosyncEndpoints = []*regexp.Regexp{
	regexp.MustCompile(`^/users/create$`), regexp.MustCompile(`^/users/auth$`),
	regexp.MustCompile(`^/syncs/progress(?:/[a-fA-F0-9]{32})?$`),
}

var bookorbitEndpoints = []*regexp.Regexp{
	regexp.MustCompile(`^/users/auth$`), regexp.MustCompile(`^/plugin/version$`), regexp.MustCompile(`^/plugin/match-check$`),
	regexp.MustCompile(`^/plugin/annotations/exchange(-ack)?$`), regexp.MustCompile(`^/plugin/bookmarks/exchange(-ack)?$`),
	regexp.MustCompile(`^/plugin/page-stats$`), regexp.MustCompile(`^/plugin/book-states$`),
}

type methodPath struct {
	method string
	path   *regexp.Regexp
}

var grimmoryPaths = []methodPath{
	{"POST", regexp.MustCompile(`^/api/v1/auth/login$`)},
	{"GET", regexp.MustCompile(`^/api/v1/version$`)},
	{"GET", regexp.MustCompile(`^/api/v1/books$`)},
	{"GET", regexp.MustCompile(`^/api/v1/books/\d+$`)},
	{"GET", regexp.MustCompile(`^/api/v1/books/\d+/content(\?bookType=[A-Za-z0-9_]+)?$`)},
	{"POST", regexp.MustCompile(`^/api/v1/books/(progress|status)$`)},
	{"GET", regexp.MustCompile(`^/api/v1/(annotations|bookmarks)/book/\d+$`)},
	{"POST", regexp.MustCompile(`^/api/v1/(annotations|bookmarks)$`)},
	{"PUT", regexp.MustCompile(`^/api/v1/(annotations|bookmarks)/\d+$`)},
	{"DELETE", regexp.MustCompile(`^/api/v1/(annotations|bookmarks)/\d+$`)},
	{"GET", regexp.MustCompile(`^/api/v1/reading-sessions/book/\d+\?page=\d+&size=\d+$`)},
	{"POST", regexp.MustCompile(`^/api/v1/reading-sessions$`)},
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// baseURL validates a serverUrl: http(s), no credentials, no query or fragment.
func baseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, Fail(http.StatusBadRequest, "Invalid serverUrl")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, Fail(http.StatusBadRequest, "Only http and https URLs are allowed")
	}
	if u.User != nil || strings.ContainsAny(raw, "?#") {
		return nil, Fail(http.StatusBadRequest, "Invalid serverUrl")
	}
	return u, nil
}

func headerFrom(m *map[string]string) http.Header {
	h := http.Header{}
	if m != nil {
		for k, v := range *m {
			if !strings.ContainsAny(k+v, "\r\n") {
				h.Set(k, v)
			}
		}
	}
	return h
}

func bodyBytes(raw json.RawMessage) []byte {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	return raw
}

// Kosync forwards a KOSync call, following same-origin redirects and http→https upgrades.
func (p *Proxy) Kosync(ctx context.Context, serverURL, endpoint, method string, headers *map[string]string, body json.RawMessage) (*Response, error) {
	if serverURL == "" || endpoint == "" {
		return nil, Fail(http.StatusBadRequest, "serverUrl and endpoint are required")
	}
	if !matchAny(kosyncEndpoints, endpoint) {
		return nil, Fail(http.StatusBadRequest, "Invalid endpoint")
	}
	base, err := baseURL(serverURL)
	if err != nil {
		return nil, err
	}
	target := *base
	target.Path = strings.TrimRight(base.Path, "/") + endpoint
	h := headerFrom(headers)
	h.Set("Accept", "application/vnd.koreader.v1+json")
	h.Set("Content-Type", "application/json")
	data := bodyBytes(body)
	current := &target
	for hop := 0; ; hop++ {
		resp, err := p.do(ctx, method, current.String(), h, data)
		if err != nil {
			return nil, failFor(err)
		}
		loc := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
			return jsonResponse(resp)
		}
		resp.Body.Close()
		next, err := current.Parse(loc)
		if err != nil {
			return nil, Fail(http.StatusInternalServerError, "Proxy request failed", "invalid redirect")
		}
		upgrade := current.Scheme == "http" && next.Scheme == "https" && current.Hostname() == next.Hostname() && next.Port() == ""
		if hop >= 5 || next.User != nil || (next.Scheme+"://"+next.Host != current.Scheme+"://"+current.Host && !upgrade) {
			return nil, Fail(http.StatusInternalServerError, "Proxy request failed", "Redirect destination is not allowed")
		}
		if resp.StatusCode == http.StatusSeeOther || (resp.StatusCode <= 302 && method == http.MethodPost) {
			method, data = http.MethodGet, nil
		}
		current = next
	}
}

// BookOrbit forwards a BookOrbit KOReader-plugin call; redirects are refused.
func (p *Proxy) BookOrbit(ctx context.Context, serverURL, endpoint, method string, headers *map[string]string, body json.RawMessage) (*Response, error) {
	if serverURL == "" || endpoint == "" {
		return nil, Fail(http.StatusBadRequest, "serverUrl and endpoint are required")
	}
	if !matchAny(bookorbitEndpoints, endpoint) {
		return nil, Fail(http.StatusBadRequest, "Invalid endpoint")
	}
	base, err := url.Parse(serverURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, Fail(http.StatusBadRequest, "Invalid serverUrl")
	}
	h := headerFrom(headers)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	resp, err := p.do(ctx, method, strings.TrimRight(serverURL, "/")+"/api/v1/koreader"+endpoint, h, bodyBytes(body))
	if err != nil {
		return nil, failFor(err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, Fail(http.StatusInternalServerError, "Proxy request failed", fmt.Sprintf("Redirects are not followed (status %d)", resp.StatusCode))
	}
	return jsonResponse(resp)
}

// Grimmory forwards an allow-listed Grimmory API call with a raw string body.
func (p *Proxy) Grimmory(ctx context.Context, serverURL, path, method string, headers *map[string]string, body *string) (*Response, error) {
	if serverURL == "" || path == "" || method == "" {
		return nil, Fail(http.StatusBadRequest, "serverUrl, path and method are required")
	}
	ok := false
	for _, mp := range grimmoryPaths {
		if mp.method == method && mp.path.MatchString(path) {
			ok = true
		}
	}
	if !ok {
		return nil, Fail(http.StatusBadRequest, "Invalid endpoint")
	}
	base, err := baseURL(serverURL)
	if err != nil {
		return nil, err
	}
	var data []byte
	if body != nil {
		data = []byte(*body)
	}
	resp, err := p.do(ctx, method, base.Scheme+"://"+base.Host+strings.TrimRight(base.Path, "/")+path, headerFrom(headers), data)
	if err != nil {
		return nil, failFor(err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, Fail(http.StatusInternalServerError, "Proxy request failed", fmt.Sprintf("Redirects are not followed (status %d)", resp.StatusCode))
	}
	h := http.Header{}
	for _, k := range []string{"Content-Type", "Content-Range"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	return &Response{Status: resp.StatusCode, Header: h, Body: resp.Body}, nil
}

// jsonResponse passes a JSON (or text) body through with its status.
func jsonResponse(resp *http.Response) (*Response, error) {
	h := http.Header{}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	h.Set("Content-Type", ct)
	return &Response{Status: resp.StatusCode, Header: h, Body: resp.Body}, nil
}

// ── Hardcover ──

var hardcoverForwarded = []string{"Retry-After", "RateLimit", "RateLimit-Policy"}

// Hardcover forwards a GraphQL request with the user's Hardcover token.
func (p *Proxy) Hardcover(ctx context.Context, token string, body map[string]any) (*Response, error) {
	if token == "" {
		return nil, Fail(http.StatusUnauthorized, "Missing authorization header")
	}
	data, _ := json.Marshal(body)
	h := http.Header{"Content-Type": {"application/json"}, "Authorization": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.HardcoverEndpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header = h
	resp, err := p.trusted.Do(req)
	if err != nil {
		return nil, Fail(http.StatusBadGateway, "Failed to reach Hardcover API")
	}
	out := http.Header{"Content-Type": {"application/json"}}
	for _, k := range hardcoverForwarded {
		if v := resp.Header.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	return &Response{Status: resp.StatusCode, Header: out, Body: resp.Body}, nil
}

// ── Notion ──

// NotionVersion is the API version header Readest pins (services/constants.ts).
const NotionVersion = "2026-03-11"

var (
	notionID    = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	notionToken = regexp.MustCompile(`^Bearer\s+(?:secret_|ntn_)[a-zA-Z0-9_-]+$`)
)

// notionAllowed is Readest's operation allowlist for the Notion export.
func notionAllowed(method string, path []string) bool {
	id := func(i int) bool { return len(path) > i && notionID.MatchString(path[i]) }
	switch len(path) {
	case 1:
		return method == http.MethodPost && path[0] == "pages"
	case 2:
		switch method {
		case http.MethodGet:
			return (path[0] == "users" && path[1] == "me") ||
				((path[0] == "data_sources" || path[0] == "databases" || path[0] == "pages") && id(1))
		case http.MethodPatch:
			return path[0] == "pages" && id(1)
		case http.MethodDelete:
			return path[0] == "blocks" && id(1)
		}
	case 3:
		switch method {
		case http.MethodGet, http.MethodPatch:
			return path[0] == "blocks" && id(1) && path[2] == "children"
		case http.MethodPost:
			return path[0] == "data_sources" && id(1) && path[2] == "query"
		}
	}
	return false
}

// Notion forwards an allow-listed Notion API call with the user's integration token.
func (p *Proxy) Notion(ctx context.Context, method string, path []string, token string, query url.Values, body map[string]any) (*Response, error) {
	if !notionAllowed(method, path) {
		return nil, Fail(http.StatusNotFound, "Notion operation not found")
	}
	if !notionToken.MatchString(token) {
		return nil, Fail(http.StatusUnauthorized, "Invalid Notion token")
	}
	u := p.NotionBase + "/" + strings.Join(path, "/")
	if method == http.MethodGet && len(path) == 3 && len(query) > 0 {
		q := url.Values{}
		if ps := query.Get("page_size"); ps != "" {
			q.Set("page_size", ps)
		}
		if c := query.Get("start_cursor"); c != "" && len(c) <= 512 {
			q.Set("start_cursor", c)
		}
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
	}
	var rd io.Reader
	if body != nil && method != http.MethodGet && method != http.MethodDelete {
		data, _ := json.Marshal(body)
		if len(data) > 500*1024 {
			return nil, Fail(http.StatusRequestEntityTooLarge, "Request body too large")
		}
		rd = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Notion-Version", NotionVersion)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.trusted.Do(req)
	if err != nil {
		cancel()
		return nil, Fail(http.StatusBadGateway, "Failed to reach Notion API")
	}
	return &Response{Status: resp.StatusCode, Header: http.Header{"Content-Type": {"application/json"}}, Body: &cancelBody{resp.Body, cancel}}, nil
}
