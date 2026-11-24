package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bookroom/internal/api"
	"bookroom/internal/library"
)

func TestGoogleParsesVolumes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "k1", r.URL.Query().Get("key"))
		assert.Equal(t, "isbn:9787536692930", r.URL.Query().Get("q"))
		fmt.Fprint(w, `{"items":[{"id":"v1","volumeInfo":{"title":"三体","authors":["刘慈欣"],"publisher":"重庆出版社",
			"publishedDate":"2008-01","description":"<p>A &amp; B</p>","pageCount":302,"language":"zh",
			"industryIdentifiers":[{"type":"ISBN_10","identifier":"7536692935"},{"type":"ISBN_13","identifier":"9787536692930"}],
			"imageLinks":{"thumbnail":"http://books.google.com/t.jpg","small":"http://books.google.com/s.jpg"}}}]}`)
	}))
	defer srv.Close()
	g := NewGoogle("k1")
	g.Base = srv.URL
	ok, _ := g.Enabled()
	require.True(t, ok)
	cands, err := g.Search(context.Background(), Query{ISBN: "9787536692930"})
	require.NoError(t, err)
	require.Len(t, cands, 1)
	c := cands[0]
	assert.Equal(t, "三体", c.Title)
	assert.Equal(t, "9787536692930", c.ISBN)
	assert.Equal(t, "A & B", c.Description)
	assert.Equal(t, "https://books.google.com/s.jpg", c.CoverURL, "largest size, https")
	assert.Equal(t, 302, *c.PageCount)
	ok, reason := NewGoogle("").Enabled()
	assert.False(t, ok)
	assert.Contains(t, reason, "GOOGLE_BOOKS_KEY")
}

func TestHardcoverSendsBearerAndParsesEditions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		var body struct {
			Query     string
			Variables map[string]any
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Contains(t, body.Query, "editions")
		assert.Equal(t, "9780765382030", body.Variables["isbn"])
		fmt.Fprint(w, `{"data":{"editions":[{"isbn_13":"9780765382030","publisher":{"name":"Tor"},
			"book":{"id":7,"title":"The Three-Body Problem","pages":400,"slug":"tbp","image":{"url":"https://img/x.jpg"},
			"contributions":[{"author":{"name":"Liu Cixin"}}]}}]}}`)
	}))
	defer srv.Close()
	h := NewHardcover("tok")
	h.Endpoint = srv.URL
	cands, err := h.Search(context.Background(), Query{ISBN: "9780765382030"})
	require.NoError(t, err)
	require.Len(t, cands, 1)
	assert.Equal(t, "7", cands[0].ProviderID)
	assert.Equal(t, "Tor", cands[0].Publisher)
	assert.Equal(t, []string{"Liu Cixin"}, cands[0].Authors)
	assert.Equal(t, "https://hardcover.app/books/tbp", cands[0].SourceURL)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errors":[{"message":"invalid token"}]}`)
	}))
	defer bad.Close()
	h.Endpoint = bad.URL
	_, err = h.Search(context.Background(), Query{ISBN: "9780765382030"})
	assert.ErrorContains(t, err, "invalid token")
}

func xshServer(t *testing.T, remaining *int, calls *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authorization/token":
			user, pass, ok := r.BasicAuth()
			assert.True(t, ok && user == "app" && pass == "secret")
			body, _ := io.ReadAll(r.Body)
			assert.Equal(t, "grant_type=client_credentials", string(body))
			fmt.Fprint(w, `{"access_token":"T","token_type":"bearer","expires_in":172800}`)
			return
		}
		*calls++
		assert.Equal(t, "Bearer T", r.Header.Get("Authorization"))
		w.Header().Set("Ratelimit-Remaining", fmt.Sprint(*remaining))
		switch r.URL.Path {
		case "/v1/search":
			assert.Equal(t, "凡人修仙传", r.URL.Query().Get("q"))
			fmt.Fprint(w, `{"total":1,"page":1,"pages":1,"items":[{"id":549460,"title":"凡人修仙传",
				"title_html":"<mark>凡人修仙传</mark>","author":"忘语","category":"古典仙侠","status":"completed",
				"word_count":7183000,"intro_html":"已改编影视…","cover":"https://c/x.jpg"}]}`)
		case "/v1/works/549460":
			fmt.Fprint(w, `{"id":549460,"title":"凡人修仙传","author":"忘语","intro":"一个普通山村少年。",
				"cover":"https://c/x.jpg","genre":{"slug":"xianxia","name":"仙侠"},"category":{"site_name":"起点","names":["仙侠","古典仙侠"]},
				"tags":["凡人流"],"status":"completed","word_count":7183000,"chapter_count":2451,"latest_chapter":"完本感言",
				"latest_update":"2016-01-06T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestXiaoshuohubTokenSearchAndQuota(t *testing.T) {
	remaining, calls := 500, 0
	srv := xshServer(t, &remaining, &calls)
	defer srv.Close()
	x := NewXiaoshuohub(true, srv.URL, "app", "secret")
	ctx := context.Background()
	cands, err := x.Search(ctx, Query{Title: "凡人修仙传", Author: "忘语"})
	require.NoError(t, err)
	require.Len(t, cands, 1)
	assert.Equal(t, api.WorkTypeWebnovel, cands[0].WorkType)
	assert.Equal(t, "已改编影视…", cands[0].Description)

	full, err := x.Get(ctx, "549460")
	require.NoError(t, err)
	assert.Equal(t, "一个普通山村少年。", full.Description)
	assert.Equal(t, []string{"凡人流", "仙侠", "仙侠", "古典仙侠"}, full.Tags)
	require.NotNil(t, full.Webnovel)
	assert.Equal(t, int64(7183000), *full.Webnovel.WordCount)
	assert.Equal(t, 2451, *full.Webnovel.ChapterCount)
	assert.Equal(t, api.Completed, *full.Webnovel.Status)

	// Below the low-water mark the answer is still used, but the next call fails fast without spending quota.
	remaining = 3
	_, err = x.Get(ctx, "549460")
	require.NoError(t, err)
	before := calls
	_, err = x.Get(ctx, "549460")
	var qe *PauseError
	require.ErrorAs(t, err, &qe)
	assert.Equal(t, before, calls, "no request was sent")
	_, err = x.Get(ctx, "abc")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestTooManyRequestsBecomesPauseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	ol := NewOpenLibrary(true, "")
	ol.Base = srv.URL
	_, err := ol.Search(context.Background(), Query{ISBN: "9780765382030"})
	var qe *PauseError
	require.True(t, errors.As(err, &qe))
	assert.Equal(t, "openlibrary", qe.Provider)
}

const doubanPage = `<html><body>
<h1><span property="v:itemreviewed">三体</span></h1>
<div id="mainpic"><img src="https://img.doubanio.com/s1.jpg"></div>
<div id="info">
 <span><span class="pl"> 作者</span>: <a>刘慈欣</a></span><br/>
 <span class="pl">出版社:</span> 重庆出版社<br/>
 <span class="pl">出版年:</span> 2008-1<br/>
 <span class="pl">页数:</span> 302<br/>
 <span class="pl">丛书:</span> 中国科幻基石丛书<br/>
 <span class="pl">ISBN:</span> 9787536692930<br/>
</div>
<div id="link-report"><div class="intro"><p>文化大革命如火如荼进行的同时。</p><p>第二段。</p></div></div>
<div id="db-tags-section"><a class="tag">科幻</a><a class="tag">刘慈欣</a></div>
</body></html>`

func TestParseDouban(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(doubanPage))
	require.NoError(t, err)
	c := parseDouban(doc, "2567698", "https://book.douban.com")
	assert.Equal(t, "三体", c.Title)
	assert.Equal(t, []string{"刘慈欣"}, c.Authors)
	assert.Equal(t, "重庆出版社", c.Publisher)
	assert.Equal(t, "2008-1", c.Published)
	assert.Equal(t, 302, *c.PageCount)
	assert.Equal(t, "9787536692930", c.ISBN)
	assert.Equal(t, "中国科幻基石丛书", c.Series)
	assert.Equal(t, "文化大革命如火如荼进行的同时。\n第二段。", c.Description)
	assert.Equal(t, []string{"科幻", "刘慈欣"}, c.Tags)
	ok, _ := NewDouban(false).Enabled()
	assert.False(t, ok, "scraping is opt-in")
}

func TestMatchingAndScoring(t *testing.T) {
	assert.Equal(t, "三体ⅰ", normalize("三体 Ⅰ"))
	assert.Equal(t, "abc123", normalize("ＡＢＣ-１２３"))
	assert.InDelta(t, 1.0, similarity("凡人修仙传", "凡人 修仙传"), 0)
	assert.Less(t, similarity("凡人修仙传", "仙逆"), 0.3)

	q := Query{Title: "凡人修仙传", Author: "忘语"}
	assert.True(t, exactMatch(&Candidate{Metadata: candMeta("凡人修仙传", "忘语")}, q, true))
	assert.False(t, exactMatch(&Candidate{Metadata: candMeta("凡人修仙传", "别人")}, q, true), "same title, other author")
	isbnQ := Query{ISBN: "9787536692930"}
	c := Candidate{Metadata: candMeta("三体", "刘慈欣")}
	assert.False(t, exactMatch(&c, isbnQ, false), "no ISBN, no automatic match")
	c.ISBN = "9787536692930"
	assert.True(t, exactMatch(&c, isbnQ, false))

	strong := score(&Candidate{Metadata: c.Metadata, CoverURL: "x"}, Query{ISBN: c.ISBN, Title: "三体", Author: "刘慈欣"}, "googlebooks")
	weak := score(&Candidate{Metadata: candMeta("三体", "刘慈欣")}, Query{ISBN: c.ISBN, Title: "三体", Author: "刘慈欣"}, "googlebooks")
	assert.Equal(t, 100, strong)
	assert.Less(t, weak, 60, "an ISBN mismatch sinks the candidate")
}

func candMeta(title, author string) library.Metadata {
	return library.Metadata{Title: title, Authors: []string{author}}
}
