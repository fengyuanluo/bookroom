package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bookroom/internal/apperr"
)

func status(t *testing.T, err error) int {
	t.Helper()
	ae, ok := apperr.As(err)
	require.True(t, ok, "%v", err)
	return ae.Status
}

func TestSanitizeXML(t *testing.T) {
	in := `<a t="x&y">A & B &amp; &#38; &#x26; <![CDATA[keep & this]]> &bogus</a>`
	want := `<a t="x&amp;y">A &amp; B &amp; &#38; &#x26; <![CDATA[keep & this]]> &amp;bogus</a>`
	assert.Equal(t, want, string(SanitizeXML([]byte(in))))
}

func TestHostPolicy(t *testing.T) {
	p := New(false, nil)
	for _, h := range []string{"localhost", "a.localhost", "nas.local", "x.internal", "box.lan", "intranet", ""} {
		assert.True(t, p.blockedName(h), h)
	}
	assert.False(t, p.blockedName("opds.example.com"))
	assert.False(t, p.blockedName("10.0.0.1"), "IP literals are judged at dial time")
	lan := New(true, nil)
	assert.False(t, lan.blockedName("nas.local"))
	assert.True(t, lan.blockedName("localhost"), "loopback names stay blocked")
}

func TestGuardBlocksLoopbackEvenWithPrivateAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	for _, allowPrivate := range []bool{false, true} {
		_, err := New(allowPrivate, nil).do(context.Background(), http.MethodGet, srv.URL, http.Header{}, nil)
		assert.True(t, errors.Is(err, Blocked), "allowPrivate=%v: %v", allowPrivate, err)
	}
	// A trusted host bypasses the guard.
	resp, err := New(false, []string{"127.0.0.1"}).do(context.Background(), http.MethodGet, srv.URL, http.Header{}, nil)
	require.NoError(t, err)
	resp.Body.Close()
}

func TestOPDSRedirectDropsCredentialsAcrossOrigins(t *testing.T) {
	var gotAuth string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<script>x</script>")
	}))
	defer other.Close()
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer start.Close()
	// Both test servers listen on 127.0.0.1 with different ports: different origins, same trusted host.
	p := New(false, []string{"127.0.0.1"})
	resp, err := p.OPDS(context.Background(), OPDSRequest{URL: start.URL, Auth: "Basic secret"})
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Empty(t, gotAuth, "credentials must not follow a cross-origin redirect")
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"), "active content is neutralized")

	_, err = p.OPDS(context.Background(), OPDSRequest{URL: "ftp://x/y"})
	assert.Equal(t, http.StatusBadRequest, status(t, err))
	_, err = p.OPDS(context.Background(), OPDSRequest{})
	assert.Equal(t, http.StatusBadRequest, status(t, err))
}

func TestOPDSUnauthorizedBecomesForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	resp, err := New(false, []string{"127.0.0.1"}).OPDS(context.Background(), OPDSRequest{URL: srv.URL})
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.Status)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
}

func TestKosyncForwarding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/base/users/auth":
			http.Redirect(w, r, "/base/v2/users/auth", http.StatusMovedPermanently)
		case "/base/v2/users/auth":
			assert.Equal(t, "application/vnd.koreader.v1+json", r.Header.Get("Accept"))
			assert.Equal(t, "u", r.Header.Get("x-auth-user"))
			fmt.Fprint(w, `{"authorized":"OK"}`)
		case "/base/syncs/progress":
			body, _ := io.ReadAll(r.Body)
			assert.JSONEq(t, `{"document":"d"}`, string(body))
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"document":"d","timestamp":1}`)
		}
	}))
	defer srv.Close()
	p := New(false, []string{"127.0.0.1"})
	h := map[string]string{"x-auth-user": "u", "x-auth-key": "k"}
	resp, err := p.Kosync(context.Background(), srv.URL+"/base/", "/users/auth", "GET", &h, nil)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.JSONEq(t, `{"authorized":"OK"}`, string(body))

	resp, err = p.Kosync(context.Background(), srv.URL+"/base", "/syncs/progress", "PUT", &h, json.RawMessage(`{"document":"d"}`))
	require.NoError(t, err)
	resp.Body.Close()

	for _, tc := range []struct{ server, endpoint string }{
		{srv.URL, "/admin"},
		{"ftp://x", "/users/auth"},
		{"https://user:pw@x.example.com", "/users/auth"},
		{"https://x.example.com/?q=1", "/users/auth"},
	} {
		_, err := p.Kosync(context.Background(), tc.server, tc.endpoint, "GET", nil, nil)
		assert.Equal(t, http.StatusBadRequest, status(t, err), tc)
	}
}

func TestGrimmoryAndBookOrbit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/g/api/v1/books/12/content":
			assert.Equal(t, "bookType=EPUB", r.URL.RawQuery)
			w.Header().Set("Content-Type", "application/epub+zip")
			w.Header().Set("Set-Cookie", "s=1")
			fmt.Fprint(w, "PK")
		case "/api/v1/koreader/plugin/version":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}
	}))
	defer srv.Close()
	p := New(false, []string{"127.0.0.1"})
	resp, err := p.Grimmory(context.Background(), srv.URL+"/g", "/api/v1/books/12/content?bookType=EPUB", "GET", nil, nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, "application/epub+zip", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Set-Cookie"))
	_, err = p.Grimmory(context.Background(), srv.URL, "/api/v1/books/12", "DELETE", nil, nil)
	assert.Equal(t, http.StatusBadRequest, status(t, err), "method must match the allowlist entry")

	_, err = p.BookOrbit(context.Background(), srv.URL, "/plugin/version", "GET", nil, nil)
	assert.Equal(t, http.StatusInternalServerError, status(t, err), "redirects are not followed")
	_, err = p.BookOrbit(context.Background(), srv.URL, "/plugin/../admin", "GET", nil, nil)
	assert.Equal(t, http.StatusBadRequest, status(t, err))
}

func TestNotionAllowlist(t *testing.T) {
	assert.True(t, notionAllowed("GET", []string{"users", "me"}))
	assert.True(t, notionAllowed("POST", []string{"pages"}))
	assert.True(t, notionAllowed("POST", []string{"data_sources", "abc-1", "query"}))
	assert.True(t, notionAllowed("PATCH", []string{"blocks", "abc", "children"}))
	assert.False(t, notionAllowed("DELETE", []string{"pages", "abc"}))
	assert.False(t, notionAllowed("GET", []string{"pages", "../x"}))
	assert.False(t, notionAllowed("POST", []string{"search"}))

	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer srv.Close()
	p := New(false, nil)
	p.NotionBase = srv.URL
	q := url.Values{"page_size": {"50"}, "start_cursor": {"c1"}, "evil": {"1"}}
	resp, err := p.Notion(context.Background(), "GET", []string{"blocks", "b1", "children"}, "Bearer ntn_abc", q, nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, NotionVersion, got.Header.Get("Notion-Version"))
	assert.Equal(t, "page_size=50&start_cursor=c1", got.URL.RawQuery)
	_, err = p.Notion(context.Background(), "GET", []string{"users", "me"}, "Bearer nope", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, status(t, err))
}
