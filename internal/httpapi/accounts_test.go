package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bookroom/internal/scrape"
	"bookroom/internal/testutil"
)

func TestAccountAdministration(t *testing.T) {
	e := newEnv(t)
	aliceID, alice := e.user("alice")

	var tp struct{ RefreshToken string }
	e.expect(e.call("POST", "/api/v1/auth/login", "", map[string]string{"username": "alice", "password": "alicepass1"}), 200, &tp)
	e.expect(e.call("PUT", "/api/v1/me/password", alice, map[string]string{"currentPassword": "wrong", "newPassword": "newpassword"}), 400, nil)
	e.expect(e.call("PUT", "/api/v1/me/password", alice, map[string]string{"currentPassword": "alicepass1", "newPassword": "newpassword"}), 204, nil)
	e.expect(e.call("POST", "/api/v1/auth/refresh", "", map[string]string{"refreshToken": tp.RefreshToken}), 401, nil)
	e.expect(e.call("POST", "/api/v1/auth/login", "", map[string]string{"username": "alice", "password": "newpassword"}), 200, nil)

	e.expect(e.call("PUT", "/api/v1/users/"+aliceID+"/password", e.admin, map[string]string{"password": "resetpass1"}), 204, nil)
	e.expect(e.call("POST", "/api/v1/auth/login", "", map[string]string{"username": "alice", "password": "resetpass1"}), 200, nil)
	e.expect(e.call("PUT", "/api/v1/users/nobody/password", e.admin, map[string]string{"password": "resetpass1"}), 404, nil)

	var list struct{ Items []struct{ Username string } }
	e.expect(e.call("GET", "/api/v1/users", e.admin, nil), 200, &list)
	assert.Len(t, list.Items, 2)

	var me struct{ ID string }
	e.expect(e.call("GET", "/api/v1/me", e.admin, nil), 200, &me)
	e.expect(e.call("DELETE", "/api/v1/users/"+me.ID, e.admin, nil), 400, nil)
	e.expect(e.call("DELETE", "/api/v1/users/"+aliceID, e.admin, nil), 204, nil)
	e.expect(e.call("GET", "/api/v1/me", alice, nil), 401, nil) // a deleted user's token stops working at once
}

func TestDeviceKeysAndRevocation(t *testing.T) {
	e := newEnv(t)
	var dev struct {
		Device struct{ ID string }
		Key    string
	}
	e.expect(e.call("POST", "/api/v1/devices", e.admin, map[string]string{"name": "Kobo Libra"}), 201, &dev)
	h := []string{"x-auth-user", "anything", "x-auth-key", md5hex(dev.Key)}
	e.expect(e.call("GET", "/kosync/users/auth", "", nil, h...), 200, nil)

	var list struct {
		Items []struct {
			Name       string
			LastUsedAt *time.Time
		}
	}
	require.Eventually(t, func() bool {
		e.expect(e.call("GET", "/api/v1/devices", e.admin, nil), 200, &list)
		return len(list.Items) == 1 && list.Items[0].LastUsedAt != nil
	}, 2*time.Second, 10*time.Millisecond)

	e.expect(e.call("DELETE", "/api/v1/devices/"+dev.Device.ID, e.admin, nil), 204, nil)
	e.expect(e.call("GET", "/kosync/users/auth", "", nil, h...), 401, nil)
	e.expect(e.call("DELETE", "/api/v1/devices/"+dev.Device.ID, e.admin, nil), 404, nil)

	// OPDS: an account password check is cached, and a password change invalidates the cache.
	req, _ := http.NewRequest("GET", e.srv.URL+"/opds", nil)
	req.SetBasicAuth("admin", "adminpass")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	e.expect(resp, 200, nil)
	e.expect(e.call("PUT", "/api/v1/me/password", e.admin, map[string]string{"currentPassword": "adminpass", "newPassword": "changed123"}), 204, nil)
	req, _ = http.NewRequest("GET", e.srv.URL+"/opds", nil)
	req.SetBasicAuth("admin", "adminpass")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	e.expect(resp, 401, nil)
}

func TestCBRStaysOutOfReadest(t *testing.T) {
	e := newEnv(t)
	// A minimal RAR5 signature is enough: parsing fails, the file-name fallback names the book.
	p := testutil.WriteFile(t, t.TempDir(), "《漫画》作者.cbr", []byte("Rar!\x1a\x07\x01\x00garbage"))
	var b book
	e.expect(e.upload(e.admin, p, "shelf", "Comics"), 201, &b)
	assert.Equal(t, "漫画", b.Title)
	assert.Empty(t, b.State.Shelf, "CBR has no per-user state")

	var pull pullResponse
	e.expect(e.call("GET", "/api/sync?since=0&type=books", e.admin, nil), 200, &pull)
	for _, row := range pull.Books {
		assert.NotEqual(t, b.Hash, row.BookHash)
	}
	e.expect(e.call("PUT", "/api/v1/books/"+b.Hash+"/state", e.admin, map[string]any{"readingStatus": "reading"}), 409, nil)
	var page struct{ Total int }
	e.expect(e.call("GET", "/api/v1/books?format=CBR", e.admin, nil), 200, &page)
	assert.Equal(t, 1, page.Total, "still listed in the library")
}

func TestWebnovelTXTAutoFill(t *testing.T) {
	works := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/search":
			assert.Equal(t, "忘语", r.URL.Query().Get("author"))
			fmt.Fprint(w, `{"items":[{"id":1,"title":"凡人修仙传","author":"忘语"},{"id":2,"title":"凡人修仙传之仙界篇","author":"忘语"}]}`)
		case "/v1/works/1":
			fmt.Fprint(w, `{"id":1,"title":"凡人修仙传","author":"忘语","intro":"一个普通山村少年。","tags":["凡人流"],
				"status":"completed","word_count":7183000,"chapter_count":2451}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer works.Close()
	e := newEnv(t, envOptions{providers: []scrape.Provider{scrape.NewXiaoshuohub(true, works.URL, "", "")}})
	p := testutil.WriteFile(t, t.TempDir(), "《凡人修仙传》作者：忘语.txt", []byte("第一章 山边小村\n\n二愣子睁大着双眼。\n"))
	var b book
	e.expect(e.upload(e.admin, p), 201, &b)
	assert.Equal(t, "unknown", b.WorkType)
	require.Eventually(t, func() bool {
		e.expect(e.call("GET", "/api/v1/books/"+b.Hash, e.admin, nil), 200, &b)
		return b.Description != ""
	}, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, "一个普通山村少年。", b.Description)
	assert.Equal(t, "webnovel", b.WorkType)
	assert.Equal(t, []string{"凡人流"}, b.Tags)

	// The stored file is the original TXT: its hash is what KOReader computes for the same file.
	resp := e.call("GET", "/api/v1/books/"+b.Hash+"/file", e.admin, nil)
	assert.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")
	data := e.expect(resp, 200, nil)
	orig, _ := os.ReadFile(p)
	assert.Equal(t, orig, data)
}

func TestReadestCoverUploadIsAdopted(t *testing.T) {
	e := newEnv(t)
	p := testutil.WriteEPUB(t, t.TempDir(), "nocover.epub", testutil.EPUB{Title: "No Cover", Author: "A"})
	var b book
	e.expect(e.upload(e.admin, p), 201, &b)
	assert.False(t, b.HasCover)
	png := testutil.PNG(t)
	var up struct {
		UploadURL string `json:"uploadUrl"`
	}
	e.expect(e.call("POST", "/api/storage/upload", e.admin, map[string]any{"fileName": "Readest/Books/" + b.Hash + "/cover.png", "fileSize": len(png)}), 200, &up)
	e.expect(e.call("PUT", up.UploadURL, "", png), 200, nil)
	e.expect(e.call("GET", "/api/v1/books/"+b.Hash, e.admin, nil), 200, &b)
	assert.True(t, b.HasCover, "a client-rendered cover fills a missing library cover")

	var pull struct {
		Books []struct {
			BookHash  string  `json:"book_hash"`
			CoverHash *string `json:"cover_hash"`
		} `json:"books"`
	}
	e.expect(e.call("GET", "/api/sync?since=0&type=books", e.admin, nil), 200, &pull)
	require.Len(t, pull.Books, 1)
	assert.NotNil(t, pull.Books[0].CoverHash, "the new cover reaches Readest through cover_hash")
}

func TestStaticWebBuild(t *testing.T) {
	e := newEnv(t)
	web := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>app</html>"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(web, "library"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(web, "library", "index.html"), []byte("library page"), 0o644))
	e.deps.Cfg.WebDir = web
	srv := newServer(t, e)
	get := func(path string) (int, string) {
		resp, err := http.Get(srv + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		b := make([]byte, 64)
		n, _ := resp.Body.Read(b)
		return resp.StatusCode, string(b[:n])
	}
	code, body := get("/library/")
	assert.Equal(t, 200, code)
	assert.Equal(t, "library page", body)
	code, body = get("/reader/" + url.PathEscape("x"))
	assert.Equal(t, 200, code)
	assert.Equal(t, "<html>app</html>", body, "unknown client routes fall back to index.html")
	code, _ = get("/api/nope")
	assert.Equal(t, 404, code)
	_, body = get("/../../etc/passwd")
	assert.NotContains(t, body, "root:", "no file outside the web build is served")
}

func TestListPaging(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	for i, title := range []string{"delta", "Alpha", "charlie", "Bravo", "echo"} {
		p := testutil.WriteEPUB(t, dir, fmt.Sprintf("%d.epub", i), testutil.EPUB{Title: title, Author: "A", Salt: title})
		e.expect(e.upload(e.admin, p), 201, nil)
	}
	walk := func(query string) []string {
		var titles []string
		cursor := ""
		for {
			var page struct {
				Items      []book
				Total      int
				NextCursor *string
			}
			e.expect(e.call("GET", "/api/v1/books?limit=2&"+query+"&cursor="+url.QueryEscape(cursor), e.admin, nil), 200, &page)
			assert.Equal(t, 5, page.Total)
			for _, b := range page.Items {
				titles = append(titles, b.Title)
			}
			if page.NextCursor == nil {
				return titles
			}
			cursor = *page.NextCursor
		}
	}
	assert.Equal(t, []string{"Alpha", "Bravo", "charlie", "delta", "echo"}, walk("sort=title&order=asc"))
	assert.Equal(t, []string{"echo", "delta", "charlie", "Bravo", "Alpha"}, walk("sort=title&order=desc"))
	var all struct{ Items []book }
	e.expect(e.call("GET", "/api/v1/books?limit=200&sort=added&order=desc", e.admin, nil), 200, &all)
	var want []string
	for _, b := range all.Items {
		want = append(want, b.Title)
	}
	assert.Equal(t, want, walk("sort=added&order=desc"), "pages concatenate to the single-page order")
	assert.Len(t, walk("sort=lastRead"), 5, "offset paging for the per-user sort")

	var page struct{ NextCursor *string }
	e.expect(e.call("GET", "/api/v1/books?limit=2&sort=title", e.admin, nil), 200, &page)
	require.NotNil(t, page.NextCursor)
	e.expect(e.call("GET", "/api/v1/books?limit=2&sort=added&cursor="+url.QueryEscape(*page.NextCursor), e.admin, nil), 400, nil)
	e.expect(e.call("GET", "/api/v1/books?cursor=%25%25", e.admin, nil), 400, nil)
}

func TestUnreachableSourcePausesInsteadOfGivingUp(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused from now on
	e := newEnv(t, envOptions{providers: []scrape.Provider{scrape.NewXiaoshuohub(true, deadURL, "", "")}})
	var b1, b2 book
	e.expect(e.upload(e.admin, testutil.WriteFile(t, t.TempDir(), "《甲》作者：某.txt", []byte("一"))), 201, &b1)

	var providers struct {
		Items []struct {
			ID          string     `json:"id"`
			PausedUntil *time.Time `json:"pausedUntil"`
		}
	}
	require.Eventually(t, func() bool {
		e.expect(e.call("GET", "/api/v1/metadata/providers", e.admin, nil), 200, &providers)
		return providers.Items[0].PausedUntil != nil
	}, 5*time.Second, 20*time.Millisecond)
	assert.WithinDuration(t, time.Now().Add(10*time.Minute), *providers.Items[0].PausedUntil, time.Minute)

	// A book queued while the source is paused waits for it rather than being marked no_match.
	e.expect(e.upload(e.admin, testutil.WriteFile(t, t.TempDir(), "《乙》作者：某.txt", []byte("二"))), 201, &b2)
	ctx := context.Background()
	require.Eventually(t, func() bool {
		job, err := e.deps.Store.Q.GetScrapeJob(ctx, b2.Hash)
		return err == nil && job.NextRunAt > time.Now().Add(5*time.Minute).UnixMilli()
	}, 5*time.Second, 20*time.Millisecond)
	for _, h := range []string{b1.Hash, b2.Hash} {
		job, err := e.deps.Store.Q.GetScrapeJob(ctx, h)
		require.NoError(t, err)
		assert.Equal(t, "pending", job.Status)
		assert.Zero(t, job.Attempts, "a pause does not burn retry attempts")
	}

	var cands struct {
		Errors []struct {
			Provider string     `json:"provider"`
			RetryAt  *time.Time `json:"retryAt"`
		}
	}
	e.expect(e.call("POST", "/api/v1/books/"+b1.Hash+"/metadata/search", e.admin, map[string]any{}), 200, &cands)
	require.Len(t, cands.Errors, 1)
	assert.NotNil(t, cands.Errors[0].RetryAt, "manual search explains the paused source")
}
