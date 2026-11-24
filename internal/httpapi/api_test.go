package httpapi_test

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bookroom/internal/library"
	"bookroom/internal/proxy"
	"bookroom/internal/scrape"
	"bookroom/internal/testutil"
)

type book struct {
	Hash         string   `json:"hash"`
	Title        string   `json:"title"`
	Authors      []string `json:"authors"`
	Isbn         string   `json:"isbn"`
	Description  string   `json:"description"`
	Tags         []string `json:"tags"`
	HasCover     bool     `json:"hasCover"`
	WorkType     string   `json:"workType"`
	LockedFields []string `json:"lockedFields"`
	State        struct {
		Shelf         string `json:"shelf"`
		ReadingStatus string `json:"readingStatus"`
	} `json:"state"`
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestAuthFlow(t *testing.T) {
	e := newEnv(t)
	var tp struct {
		AccessToken, RefreshToken string
	}
	e.expect(e.call("POST", "/api/v1/auth/login", "", map[string]string{"username": "ADMIN", "password": "adminpass"}), 200, &tp)
	var rotated struct{ RefreshToken string }
	e.expect(e.call("POST", "/api/v1/auth/refresh", "", map[string]string{"refreshToken": tp.RefreshToken}), 200, &rotated)
	assert.NotEqual(t, tp.RefreshToken, rotated.RefreshToken)
	// A consumed refresh token is dead.
	e.expect(e.call("POST", "/api/v1/auth/refresh", "", map[string]string{"refreshToken": tp.RefreshToken}), 401, nil)

	e.expect(e.call("POST", "/api/v1/auth/login", "", map[string]string{"username": "admin", "password": "nope"}), 401, nil)
	var problem struct{ Type, Detail string }
	e.expect(e.call("GET", "/api/v1/me", "", nil), 401, &problem)
	assert.Equal(t, "/problems/unauthorized", problem.Type)

	_, alice := e.user("alice")
	e.expect(e.call("GET", "/api/v1/users", alice, nil), 403, nil)
	e.expect(e.call("POST", "/api/v1/users", e.admin, map[string]any{"username": "alice", "password": "whatever1"}), 409, nil)
	e.expect(e.call("POST", "/api/v1/users", e.admin, map[string]any{"username": "bad:name", "password": "whatever1"}), 400, nil)

	// The GoTrue subset readest.koplugin logs in with; email accepts the username too.
	var sess struct {
		AccessToken string `json:"access_token"`
		User        struct{ ID string }
	}
	e.expect(e.call("POST", "/auth/v1/token?grant_type=password", "", map[string]string{"email": "admin@example.com", "password": "adminpass"}), 200, &sess)
	e.expect(e.call("GET", "/auth/v1/user", sess.AccessToken, nil), 200, nil)
	// The plugin's URL join yields a double slash.
	e.expect(e.call("POST", "/auth/v1//token?grant_type=refresh_token", "", map[string]string{"refresh_token": "nope"}), 401, nil)
	e.expect(e.call("POST", "/auth/v1/token?grant_type=password", "", map[string]string{"email": "admin", "password": "x"}), 400, nil)
}

func TestLibraryLifecycle(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	p := testutil.WriteEPUB(t, dir, "santi.epub", testutil.EPUB{Title: "三体", Author: "刘慈欣", ISBN: "9787536692930",
		Language: "zh", Subjects: []string{"科幻"}, Cover: true})
	var b book
	e.expect(e.upload(e.admin, p, "shelf", "小说/科幻"), 201, &b)
	assert.True(t, library.IsHash(b.Hash))
	assert.Equal(t, "三体", b.Title)
	assert.Equal(t, "published", b.WorkType)
	assert.True(t, b.HasCover)
	assert.Equal(t, "小说/科幻", b.State.Shelf)
	e.expect(e.upload(e.admin, p), 200, nil) // same bytes: existing book

	// One-character Chinese queries match (LIKE, not FTS trigram).
	var page struct {
		Items []book
		Total int
	}
	e.expect(e.call("GET", "/api/v1/books?q="+url.QueryEscape("体"), e.admin, nil), 200, &page)
	require.Equal(t, 1, page.Total)
	e.expect(e.call("GET", "/api/v1/books?shelf="+url.QueryEscape("小说")+"&recursive=true", e.admin, nil), 200, &page)
	assert.Equal(t, 1, page.Total)
	e.expect(e.call("GET", "/api/v1/books?shelf="+url.QueryEscape("小说"), e.admin, nil), 200, &page)
	assert.Equal(t, 0, page.Total)

	// Range download.
	resp := e.call("GET", "/api/v1/books/"+b.Hash+"/file", e.admin, nil, "Range", "bytes=0-3")
	body := e.expect(resp, 206, nil)
	assert.Equal(t, []byte("PK\x03\x04"), body)
	cover := e.call("GET", "/api/v1/books/"+b.Hash+"/cover", e.admin, nil)
	assert.Equal(t, "image/png", cover.Header.Get("Content-Type"))
	e.expect(cover, 200, nil)

	// A user edit is written and locked.
	e.expect(e.call("PATCH", "/api/v1/books/"+b.Hash, e.admin, map[string]any{"description": "地球往事三部曲之一"}), 200, &b)
	assert.Equal(t, "地球往事三部曲之一", b.Description)
	assert.Contains(t, b.LockedFields, "description")
	e.expect(e.call("PATCH", "/api/v1/books/"+b.Hash, e.admin, map[string]any{"isbn": "123"}), 400, nil)

	// Shelves and reading status.
	var shelves struct {
		Items []struct {
			Path              string
			Count, TotalCount int
		}
	}
	e.expect(e.call("GET", "/api/v1/shelves", e.admin, nil), 200, &shelves)
	require.Len(t, shelves.Items, 2)
	assert.Equal(t, "小说", shelves.Items[0].Path)
	assert.Equal(t, 1, shelves.Items[0].TotalCount)
	var moved struct{ Moved int }
	e.expect(e.call("POST", "/api/v1/shelves/rename", e.admin, map[string]string{"from": "小说", "to": "书/小说"}), 200, &moved)
	assert.Equal(t, 1, moved.Moved)
	e.expect(e.call("PUT", "/api/v1/books/"+b.Hash+"/state", e.admin, map[string]any{"readingStatus": "finished"}), 200, &b)
	assert.Equal(t, "书/小说/科幻", b.State.Shelf)
	assert.Equal(t, "finished", b.State.ReadingStatus)
	var cleared book
	e.expect(e.call("PUT", "/api/v1/books/"+b.Hash+"/state", e.admin, map[string]any{"shelf": nil}), 200, &cleared)
	assert.Empty(t, cleared.State.Shelf)
	assert.Equal(t, "finished", cleared.State.ReadingStatus, "an absent member is left alone")
	e.expect(e.call("PUT", "/api/v1/books/"+b.Hash+"/state", e.admin, map[string]any{"readingStatus": "bogus"}), 400, nil)

	var tags struct{ Items []struct{ Name string } }
	e.expect(e.call("GET", "/api/v1/tags", e.admin, nil), 200, &tags)
	assert.Equal(t, "科幻", tags.Items[0].Name)

	_, alice := e.user("alice")
	e.expect(e.call("DELETE", "/api/v1/books/"+b.Hash, alice, nil), 403, nil)
	e.expect(e.call("PATCH", "/api/v1/books/"+b.Hash, alice, map[string]any{"title": "x"}), 403, nil) // shared metadata: admins only
	e.expect(e.call("PUT", "/api/v1/books/"+b.Hash+"/metadata/locks", alice, map[string]any{"fields": []string{}}), 403, nil)
	e.expect(e.call("PUT", "/api/v1/books/"+b.Hash+"/state", alice, map[string]any{"readingStatus": "reading"}), 200, nil)
	e.expect(e.call("DELETE", "/api/v1/books/"+b.Hash, e.admin, nil), 204, nil)
	e.expect(e.call("GET", "/api/v1/books/"+b.Hash, e.admin, nil), 404, nil)
}

type pullResponse struct {
	Books []struct {
		BookHash   string  `json:"book_hash"`
		Title      string  `json:"title"`
		GroupName  *string `json:"group_name"`
		UploadedAt *string `json:"uploaded_at"`
		DeletedAt  *string `json:"deleted_at"`
		SyncedAt   string  `json:"synced_at"`
		Progress   []float64
	} `json:"books"`
	Configs []struct {
		BookHash string  `json:"book_hash"`
		Xpointer *string `json:"xpointer"`
		Progress *string `json:"progress"`
	} `json:"configs"`
	Notes []struct {
		ID        string  `json:"id"`
		DeletedAt *string `json:"deleted_at"`
	} `json:"notes"`
	StatBooks []map[string]any `json:"statBooks"`
	StatPages []map[string]any `json:"statPages"`
}

func TestReadestSyncProjectionAndMerge(t *testing.T) {
	e := newEnv(t)
	p := testutil.WriteEPUB(t, t.TempDir(), "b.epub", testutil.EPUB{Title: "Dune", Author: "Frank Herbert"})
	var b book
	e.expect(e.upload(e.admin, p), 201, &b)

	// A user created later sees the library book, downloadable, on the first pull.
	_, alice := e.user("alice")
	var pull pullResponse
	e.expect(e.call("GET", "/api/sync?since=0&type=books", alice, nil), 200, &pull)
	require.Len(t, pull.Books, 1)
	assert.Equal(t, b.Hash, pull.Books[0].BookHash)
	assert.NotNil(t, pull.Books[0].UploadedAt)

	// Pushing an older row with a fresher shelf: the shelf is grafted, the row's progress stays.
	now := time.Now().UnixMilli()
	push := map[string]any{
		"books": []map[string]any{{"hash": b.Hash, "title": "Dune", "author": "Frank Herbert", "format": "EPUB",
			"groupName": "SF", "groupId": "abc1234", "groupUpdatedAt": now, "updatedAt": 1000, "createdAt": 1000}},
		"configs": []map[string]any{{"bookHash": b.Hash, "progress": []int{12, 300}, "xpointer": "/body/DocFragment[3]/body/p[2]/text().0",
			"location": "epubcfi(/6/4)", "updatedAt": now}},
		"notes": []map[string]any{{"bookHash": b.Hash, "id": "n1", "type": "annotation", "text": "hi", "note": "", "updatedAt": now, "createdAt": now}},
	}
	var pushed struct {
		Books []struct {
			GroupName *string `json:"group_name"`
		} `json:"books"`
		Configs []any `json:"configs"`
		Notes   []any `json:"notes"`
	}
	e.expect(e.call("POST", "/api/sync", alice, push), 200, &pushed)
	require.Len(t, pushed.Books, 1)
	assert.Equal(t, "SF", *pushed.Books[0].GroupName)
	assert.Len(t, pushed.Configs, 1)
	assert.Len(t, pushed.Notes, 1)

	e.expect(e.call("GET", "/api/sync?since=0", alice, nil), 200, &pull)
	require.Len(t, pull.Books, 1)
	assert.Equal(t, []float64{12, 300}, pull.Books[0].Progress, "config progress piggybacks onto the books row")
	require.Len(t, pull.Configs, 1)
	assert.Equal(t, "/body/DocFragment[3]/body/p[2]/text().0", *pull.Configs[0].Xpointer)
	require.Len(t, pull.Notes, 1)

	// The admin's own library is untouched by alice's shelf.
	e.expect(e.call("GET", "/api/v1/books/"+b.Hash, e.admin, nil), 200, &b)
	assert.Empty(t, b.State.Shelf)

	// A since past the last change returns nothing; the empty first pull carries Readest's dummy row.
	e.expect(e.call("GET", fmt.Sprintf("/api/sync?since=%d&type=configs", time.Now().Add(time.Hour).UnixMilli()), alice, nil), 200, &pull)
	assert.Empty(t, pull.Configs)
	_, bob := e.user("bob")
	e.expect(e.call("GET", "/api/sync?since=0&type=notes", bob, nil), 200, &pull)
	assert.Empty(t, pull.Notes)
	e.expect(e.call("GET", "/api/sync?since=abc", alice, nil), 400, nil)
	e.expect(e.call("GET", "/api/sync?since=0", "", nil), 403, nil)

	// Library deletion tombstones the row for everyone.
	e.expect(e.call("DELETE", "/api/v1/books/"+b.Hash, e.admin, nil), 204, nil)
	e.expect(e.call("GET", "/api/sync?since=0&type=books", alice, nil), 200, &pull)
	require.Len(t, pull.Books, 1)
	assert.NotNil(t, pull.Books[0].DeletedAt)
}

func TestReadingStatsSyncAndAggregation(t *testing.T) {
	e := newEnv(t)
	start := time.Now().Add(-2 * time.Hour).Unix()
	pages := []map[string]any{
		{"book_hash": "aaaabbbbccccddddeeeeffff00001111", "page": 1, "start_time": start, "duration": 60, "total_pages": 100},
		{"book_hash": "aaaabbbbccccddddeeeeffff00001111", "page": 2, "start_time": start + 60, "duration": 30, "total_pages": 100},
		{"book_hash": "aaaabbbbccccddddeeeeffff00001111", "page": 2, "start_time": start + 60, "duration": 20, "total_pages": 100}, // shorter duplicate loses
	}
	e.expect(e.call("POST", "/api/sync", e.admin, map[string]any{
		"statBooks": []map[string]any{{"book_hash": "aaaabbbbccccddddeeeeffff00001111", "title": "T", "authors": "A"}},
		"statPages": pages,
	}), 200, nil)
	var pull pullResponse
	e.expect(e.call("GET", "/api/sync?since=0&type=stats", e.admin, nil), 200, &pull)
	require.Len(t, pull.StatPages, 2)
	assert.NotNil(t, pull.StatPages[0]["updated_at_ms"])

	var sum struct {
		TotalSeconds, TotalPages int64
		BooksRead, ReadingDays   int
		CurrentStreak            int
	}
	e.expect(e.call("GET", "/api/v1/stats/summary?tz=UTC", e.admin, nil), 200, &sum)
	assert.Equal(t, int64(90), sum.TotalSeconds)
	assert.Equal(t, int64(2), sum.TotalPages)
	assert.Equal(t, 1, sum.BooksRead)
	assert.GreaterOrEqual(t, sum.CurrentStreak, 1)
	var books struct {
		Items []struct {
			Title   string
			Seconds int64
		}
	}
	e.expect(e.call("GET", "/api/v1/stats/books", e.admin, nil), 200, &books)
	require.Len(t, books.Items, 1)
	assert.Equal(t, "T", books.Items[0].Title)

	// Deleting a book's statistics flags its events and drops them from aggregates.
	e.expect(e.call("POST", "/api/sync", e.admin, map[string]any{
		"statBooks": []map[string]any{{"book_hash": "aaaabbbbccccddddeeeeffff00001111", "title": "T", "authors": "A", "deleted_at": time.Now().Format(time.RFC3339)}},
	}), 200, nil)
	e.expect(e.call("GET", "/api/sync?since=0&type=stats", e.admin, nil), 200, &pull)
	assert.NotNil(t, pull.StatPages[0]["deleted_at_ms"])
	e.expect(e.call("GET", "/api/v1/stats/summary?tz=UTC", e.admin, nil), 200, &sum)
	assert.Zero(t, sum.TotalSeconds)
	e.expect(e.call("GET", "/api/v1/stats/daily?tz=Mars/Base", e.admin, nil), 400, nil)
}

func TestReadestStorageFlow(t *testing.T) {
	e := newEnv(t)
	_, alice := e.user("alice")
	var me struct{ ID string }
	e.expect(e.call("GET", "/api/v1/me", alice, nil), 200, &me)

	p := testutil.WriteEPUB(t, t.TempDir(), "x.epub", testutil.EPUB{Title: "Upload me", Author: "Me"})
	hash, size, err := library.PartialMD5File(p)
	require.NoError(t, err)
	fileName := "Readest/Books/" + hash + "/Upload me.epub"
	var up struct {
		UploadURL string `json:"uploadUrl"`
		FileKey   string `json:"fileKey"`
	}
	e.expect(e.call("POST", "/api/storage/upload", alice, map[string]any{"fileName": fileName, "fileSize": size, "bookHash": hash}), 200, &up)
	assert.Equal(t, me.ID+"/"+fileName, up.FileKey)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	e.expect(e.call("PUT", up.UploadURL, "", data), 200, nil)

	// The upload landed in the shared library, visible to the admin too.
	e.expect(e.call("GET", "/api/v1/books/"+hash, e.admin, nil), 200, nil)

	var dl struct {
		DownloadURL string `json:"downloadUrl"`
	}
	e.expect(e.call("GET", "/api/storage/download?fileKey="+url.QueryEscape(up.FileKey), alice, nil), 200, &dl)
	got := e.expect(e.call("GET", dl.DownloadURL, "", nil), 200, nil)
	assert.Equal(t, data, got)

	// Per-user objects (a font) round-trip and list.
	e.expect(e.call("POST", "/api/storage/upload", alice, map[string]any{"fileName": "Readest/Fonts/my.ttf", "fileSize": 4, "replicaKind": "font", "replicaId": "f1"}), 200, &up)
	e.expect(e.call("PUT", up.UploadURL, "", []byte("FONT")), 200, nil)
	var list struct {
		Files []struct {
			FileKey string `json:"file_key"`
		}
		Total int
	}
	e.expect(e.call("GET", "/api/storage/list", alice, nil), 200, &list)
	require.Equal(t, 1, list.Total)
	e.expect(e.call("GET", "/api/storage/list?bookHash="+hash, alice, nil), 200, &list)
	assert.NotEmpty(t, list.Files, "library file of the book is listed")

	var dls struct {
		DownloadUrls map[string]*string `json:"downloadUrls"`
	}
	e.expect(e.call("POST", "/api/storage/download", alice, map[string]any{"fileKeys": []string{me.ID + "/Readest/Fonts/my.ttf", "someone-else/x"}}), 200, &dls)
	assert.NotNil(t, dls.DownloadUrls[me.ID+"/Readest/Fonts/my.ttf"])
	assert.Nil(t, dls.DownloadUrls["someone-else/x"])

	// Only an admin may delete a library book through storage.
	e.expect(e.call("DELETE", "/api/storage/delete?fileKey="+url.QueryEscape(me.ID+"/"+fileName), alice, nil), 403, nil)
	e.expect(e.call("DELETE", "/api/storage/delete?fileKey="+url.QueryEscape(me.ID+"/Readest/Fonts/my.ttf"), alice, nil), 200, nil)
	e.expect(e.call("POST", "/api/storage/upload", alice, map[string]any{"fileName": "../etc/passwd", "fileSize": 4}), 400, nil)
	e.expect(e.call("PUT", e.srv.URL+"/api/storage/objects/forged", "", []byte("x")), 403, nil)
}

func TestKosyncProtocol(t *testing.T) {
	e := newEnv(t)
	p := testutil.WriteEPUB(t, t.TempDir(), "k.epub", testutil.EPUB{Title: "K", Author: "A"})
	var b book
	e.expect(e.upload(e.admin, p), 201, &b)
	var dev struct {
		Key            string
		KosyncUsername string
	}
	e.expect(e.call("POST", "/api/v1/devices", e.admin, map[string]string{"name": "Kobo"}), 201, &dev)
	h := []string{"x-auth-user", dev.KosyncUsername, "x-auth-key", md5hex(dev.Key), "Accept", "application/vnd.koreader.v1+json"}

	e.expect(e.call("GET", "/kosync/users/auth", "", nil, h...), 200, nil)
	e.expect(e.call("GET", "/kosync/users/auth", "", nil, "x-auth-user", "x", "x-auth-key", md5hex("wrong")), 401, nil)
	var kerr struct{ Code int }
	e.expect(e.call("POST", "/kosync/users/create", "", map[string]string{"username": "u", "password": "p"}), 402, &kerr)
	assert.Equal(t, 2005, kerr.Code)

	var unknown map[string]any
	e.expect(e.call("GET", "/kosync/syncs/progress/0123456789abcdef0123456789abcdef", "", nil, h...), 200, &unknown)
	assert.Empty(t, unknown, "unknown documents answer {}")

	xp := "/body/DocFragment[5]/body/div/p[3]/text().42"
	var put struct {
		Document  string
		Timestamp int64
	}
	e.expect(e.call("PUT", "/kosync/syncs/progress", "", map[string]any{"document": b.Hash, "progress": xp, "percentage": 0.42,
		"device": "Kobo", "device_id": "K1"}, h...), 200, &put)
	assert.Equal(t, b.Hash, put.Document)
	assert.InDelta(t, time.Now().Unix(), put.Timestamp, 5)

	var got struct {
		Progress   string
		Percentage float64
		Device     string
		DeviceID   string `json:"device_id"`
	}
	e.expect(e.call("GET", "/kosync/syncs/progress/"+b.Hash, "", nil, h...), 200, &got)
	assert.Equal(t, xp, got.Progress)
	assert.Equal(t, "K1", got.DeviceID)

	// The XPointer reached the Readest config, so Readest opens the book there.
	var pull pullResponse
	e.expect(e.call("GET", "/api/sync?since=0&type=configs", e.admin, nil), 200, &pull)
	require.Len(t, pull.Configs, 1)
	assert.Equal(t, xp, *pull.Configs[0].Xpointer)
	assert.Equal(t, "[42,100]", *pull.Configs[0].Progress)

	// Readest moves on later: KOSync GET returns Readest's newer XPointer.
	time.Sleep(5 * time.Millisecond)
	newer := "/body/DocFragment[9]/body/p[1]/text().0"
	e.expect(e.call("POST", "/api/sync", e.admin, map[string]any{"configs": []map[string]any{{"bookHash": b.Hash, "xpointer": newer,
		"progress": []int{80, 100}, "updatedAt": time.Now().Add(2 * time.Second).UnixMilli()}}}), 200, nil)
	e.expect(e.call("GET", "/kosync/syncs/progress/"+b.Hash, "", nil, h...), 200, &got)
	assert.Equal(t, newer, got.Progress)
	assert.Equal(t, "Readest", got.Device)
	assert.InDelta(t, 0.8, got.Percentage, 0.001)

	// File-name checksum mode resolves through the library's original file name.
	e.expect(e.call("PUT", "/kosync/syncs/progress", "", map[string]any{"document": md5hex("k.epub"), "progress": "12",
		"percentage": 0.1, "device": "Kobo"}, h...), 200, nil)
	e.expect(e.call("PUT", "/kosync/syncs/progress", "", map[string]any{"document": "bad:doc", "progress": "1", "percentage": 0.1, "device": "x"}, h...), 403, nil)
}

type atomFeed struct {
	Entries []struct {
		Title string `xml:"title"`
		Links []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
			Type string `xml:"type,attr"`
		} `xml:"link"`
	} `xml:"entry"`
	Total int `xml:"totalResults"`
}

func TestOPDSCatalog(t *testing.T) {
	e := newEnv(t)
	p := testutil.WriteEPUB(t, t.TempDir(), "o.epub", testutil.EPUB{Title: "OPDS & Me", Author: "A", Subjects: []string{"tag1"}, Cover: true})
	var b book
	e.expect(e.upload(e.admin, p, "shelf", "Shelf A/Sub"), 201, &b)
	var dev struct{ Key string }
	e.expect(e.call("POST", "/api/v1/devices", e.admin, map[string]string{"name": "Kindle"}), 201, &dev)

	get := func(path, user, pass string) *http.Response {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		req.SetBasicAuth(user, pass)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	resp := get("/opds", "admin", "nope")
	assert.Equal(t, `Basic realm="BookRoom", charset="UTF-8"`, resp.Header.Get("WWW-Authenticate"))
	e.expect(resp, 401, nil)

	resp = get("/opds", "admin", "adminpass")
	assert.Contains(t, resp.Header.Get("Content-Type"), "kind=navigation")
	e.expect(resp, 200, nil)

	var feed atomFeed
	decode := func(resp *http.Response) {
		feed = atomFeed{}
		require.NoError(t, xml.Unmarshal(e.expect(resp, 200, nil), &feed))
	}
	resp = get("/opds/books", "admin", dev.Key) // a device key works as the password
	require.NoError(t, xml.Unmarshal(e.expect(resp, 200, nil), &feed))
	require.Len(t, feed.Entries, 1)
	assert.Equal(t, "OPDS & Me", feed.Entries[0].Title)
	assert.Equal(t, 1, feed.Total)
	var acq string
	for _, l := range feed.Entries[0].Links {
		if l.Rel == "http://opds-spec.org/acquisition" {
			acq = l.Href
			assert.Equal(t, "application/epub+zip", l.Type)
		}
	}
	require.NotEmpty(t, acq)
	u, _ := url.Parse(acq)
	e.expect(get(u.Path, "admin", "adminpass"), 200, nil)

	decode(get("/opds/shelves", "admin", "adminpass"))
	require.Len(t, feed.Entries, 1)
	assert.Equal(t, "Shelf A", feed.Entries[0].Title)
	decode(get("/opds/shelves?path="+url.QueryEscape("Shelf A/Sub"), "admin", "adminpass"))
	assert.Len(t, feed.Entries, 1)
	decode(get("/opds/search?q=opds", "admin", "adminpass"))
	assert.Len(t, feed.Entries, 1)
	decode(get("/opds/books?tag=tag1", "admin", "adminpass"))
	assert.Len(t, feed.Entries, 1)
	assert.Contains(t, string(e.expect(get("/opds/opensearch.xml", "admin", "adminpass"), 200, nil)), "{searchTerms}")
}

func TestReplicaSync(t *testing.T) {
	e := newEnv(t)
	var me struct{ ID string }
	e.expect(e.call("GET", "/api/v1/me", e.admin, nil), 200, &me)
	hlc := func(ms int64, dev string) string { return fmt.Sprintf("%013x-%08x-%s", ms, 0, dev) }
	now := time.Now().UnixMilli()
	row := map[string]any{"user_id": me.ID, "kind": "dictionary", "replica_id": "d1", "schema_version": 1,
		"fields_jsonb":  map[string]any{"name": map[string]any{"v": "Oxford", "t": hlc(now, "dev1"), "s": "dev1"}},
		"updated_at_ts": hlc(now, "dev1")}
	var res struct {
		Rows    []map[string]any
		Results []struct {
			Kind string
			Rows []map[string]any
		}
	}
	e.expect(e.call("POST", "/api/sync/replicas", e.admin, map[string]any{"rows": []any{row}}), 200, &res)
	require.Len(t, res.Rows, 1)

	e.expect(e.call("GET", "/api/sync/replicas?kind=dictionary", e.admin, nil), 200, &res)
	require.Len(t, res.Rows, 1)
	e.expect(e.call("POST", "/api/sync/replicas", e.admin, map[string]any{"cursors": []map[string]any{{"kind": "dictionary", "since": nil}}}), 200, &res)
	require.Len(t, res.Results, 1)
	assert.Len(t, res.Results[0].Rows, 1)

	var rerr struct{ Code string }
	row["updated_at_ts"] = hlc(now-10*60*1000, "dev1")
	e.expect(e.call("POST", "/api/sync/replicas", e.admin, map[string]any{"rows": []any{row}}), 409, &rerr)
	assert.Equal(t, "CLOCK_SKEW", rerr.Code)
	row["kind"] = "nope"
	e.expect(e.call("POST", "/api/sync/replicas", e.admin, map[string]any{"rows": []any{row}}), 422, &rerr)
	assert.Equal(t, "UNKNOWN_KIND", rerr.Code)
	e.expect(e.call("GET", "/api/sync/replicas?kind=dictionary", "", nil), 401, &rerr)
	assert.Equal(t, "AUTH", rerr.Code)

	var key struct{ Row struct{ SaltID, Salt string } }
	e.expect(e.call("POST", "/api/sync/replica-keys", e.admin, map[string]string{"alg": "pbkdf2-600k-sha256"}), 201, &key)
	assert.NotEmpty(t, key.Row.Salt)
	e.expect(e.call("POST", "/api/sync/replica-keys", e.admin, map[string]string{"alg": "md5"}), 422, nil)
	e.expect(e.call("DELETE", "/api/sync/replica-keys", e.admin, nil), 200, nil)
	var keys struct{ Rows []any }
	e.expect(e.call("GET", "/api/sync/replica-keys", e.admin, nil), 200, &keys)
	assert.Empty(t, keys.Rows)
}

func TestVocabularySync(t *testing.T) {
	e := newEnv(t)
	type row struct {
		ID          string           `json:"id"`
		State       string           `json:"state"`
		Hint        *string          `json:"hint"`
		Occurrences []map[string]any `json:"occurrences"`
		CreatedAt   int64            `json:"created_at"`
		ModifiedAt  int64            `json:"modified_at"`
		DeletedAt   *int64           `json:"deleted_at"`
		UpdatedAt   string           `json:"updated_at"`
	}
	var res struct {
		Rows     []row
		Rejected []string
	}
	word := func(key, state string, modified int64, sentences ...string) map[string]any {
		occ := []map[string]any{}
		for i, s := range sentences {
			occ = append(occ, map[string]any{"sentence": s, "bookHash": "b1", "at": modified + int64(i)})
		}
		return map[string]any{"id": "en:" + key, "lang": "en", "word_key": key, "word": key, "state": state,
			"hint": nil, "occurrences": occ, "created_at": modified, "modified_at": modified, "deleted_at": nil}
	}

	bad := word("broken", "none", 1)
	bad["id"] = "en:other"
	e.expect(e.call("POST", "/api/sync/vocabulary", e.admin,
		map[string]any{"rows": []any{word("serendipity", "learning", 100, "first"), bad}}), 200, &res)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, []string{"en:other"}, res.Rejected, "a malformed row is reported, not fatal")

	// An older edit from another device loses the state but contributes its sentence.
	e.expect(e.call("POST", "/api/sync/vocabulary", e.admin,
		map[string]any{"rows": []any{word("serendipity", "mastered", 50, "second")}}), 200, &res)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, "learning", res.Rows[0].State)
	assert.Equal(t, int64(50), res.Rows[0].CreatedAt)
	assert.Len(t, res.Rows[0].Occurrences, 2)
	assert.Equal(t, "first", res.Rows[0].Occurrences[0]["sentence"], "newest sentence first")

	// A newer delete drops the sentences from before it.
	gone := word("serendipity", "learning", 200)
	gone["deleted_at"] = 200
	e.expect(e.call("POST", "/api/sync/vocabulary", e.admin, map[string]any{"rows": []any{gone}}), 200, &res)
	require.NotNil(t, res.Rows[0].DeletedAt)
	assert.Empty(t, res.Rows[0].Occurrences)

	e.expect(e.call("POST", "/api/sync/vocabulary", e.admin,
		map[string]any{"rows": []any{word("apple", "none", 300), word("banana", "none", 300)}}), 200, &res)

	// Keyset paging: the last row's (updated_at, id) resumes exactly after it.
	e.expect(e.call("GET", "/api/sync/vocabulary?limit=2", e.admin, nil), 200, &res)
	require.Len(t, res.Rows, 2)
	last := res.Rows[1]
	q := url.Values{"limit": {"2"}, "since_ts": {last.UpdatedAt}, "since_id": {last.ID}}
	e.expect(e.call("GET", "/api/sync/vocabulary?"+q.Encode(), e.admin, nil), 200, &res)
	require.Len(t, res.Rows, 1)
	assert.NotEqual(t, last.ID, res.Rows[0].ID)

	e.expect(e.call("GET", "/api/sync/vocabulary?since_ts=yesterday", e.admin, nil), 400, nil)
	tooMany := make([]any, 101)
	for i := range tooMany {
		tooMany[i] = word(fmt.Sprintf("w%d", i), "none", 1)
	}
	e.expect(e.call("POST", "/api/sync/vocabulary", e.admin, map[string]any{"rows": tooMany}), 413, nil)
	e.expect(e.call("GET", "/api/sync/vocabulary", "", nil), 401, nil)
}

func TestProxySSRFAndPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/feed", http.StatusFound)
			return
		}
		assert.Equal(t, "Basic dXNlcjpwdw==", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Header().Set("Set-Cookie", "secret=1")
		_, _ = io.WriteString(w, `<feed><title>A & B</title></feed>`)
	}))
	defer upstream.Close()

	e := newEnv(t)
	q := "/api/opds/proxy?url=" + url.QueryEscape(upstream.URL+"/redirect") + "&auth=" + url.QueryEscape("Basic dXNlcjpwdw==")
	var perr struct{ Error string }
	e.expect(e.call("GET", q, e.admin, nil), 400, &perr) // loopback is never reachable by default
	assert.Contains(t, perr.Error, "not allowed")
	e.expect(e.call("GET", q, "", nil), 401, nil)

	trusted := newEnv(t, envOptions{proxy: proxy.New(false, []string{"127.0.0.1"})})
	resp := trusted.call("GET", q+"&access_token="+trusted.admin, "", nil)
	assert.Empty(t, resp.Header.Get("Set-Cookie"), "upstream cookies never pass")
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "sandbox")
	body := trusted.expect(resp, 200, nil)
	assert.Equal(t, `<feed><title>A &amp; B</title></feed>`, string(body))

	e.expect(e.call("POST", "/api/kosync", e.admin, map[string]any{"serverUrl": "https://sync.example.com", "endpoint": "/etc/passwd", "method": "GET"}), 400, nil)
	e.expect(e.call("POST", "/api/grimmory", e.admin, map[string]any{"serverUrl": "https://g.example.com", "path": "/api/v1/admin", "method": "GET"}), 400, nil)
	e.expect(e.call("GET", "/api/notion/users/me", e.admin, nil, "x-notion-token", "Bearer nope"), 401, nil)
}

// fakeOpenLibrary answers the two Open Library calls the scraper makes for an ISBN.
func fakeOpenLibrary(t *testing.T, isbn string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/search.json":
			fmt.Fprintf(w, `{"docs":[{"key":"/works/OL1W","title":"The Three-Body Problem","author_name":["Liu Cixin"],"isbn":["%s"],"publisher":["Tor"],"first_publish_year":2008,"subject":["Science fiction"]}]}`, isbn)
		case r.URL.Path == "/isbn/"+isbn+".json":
			fmt.Fprint(w, `{"title":"The Three-Body Problem","publishers":["Tor Books"],"publish_date":"2014","number_of_pages":400,"works":[{"key":"/works/OL1W"}]}`)
		case r.URL.Path == "/works/OL1W.json":
			fmt.Fprint(w, `{"title":"The Three-Body Problem","description":{"value":"First contact."},"subjects":["Science fiction","China"],"authors":[{"author":{"key":"/authors/OL1A"}}]}`)
		case r.URL.Path == "/authors/OL1A.json":
			fmt.Fprint(w, `{"name":"Liu Cixin"}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestMetadataSearchApplyAndAutoFill(t *testing.T) {
	const isbn = "9780765382030"
	fake := fakeOpenLibrary(t, isbn)
	defer fake.Close()
	ol := scrape.NewOpenLibrary(true, "test@example.com") // a contact raises the limit to 3 req/s
	ol.Base, ol.Covers = fake.URL, fake.URL
	e := newEnv(t, envOptions{providers: []scrape.Provider{scrape.NewGoogle(""), ol}})

	p := testutil.WriteEPUB(t, t.TempDir(), "tbp.epub", testutil.EPUB{Title: "The Three-Body Problem", Author: "Liu Cixin", ISBN: isbn})
	var b book
	e.expect(e.upload(e.admin, p), 201, &b)

	var providers struct {
		Items []struct {
			ID      string `json:"id"`
			Enabled bool
		}
	}
	e.expect(e.call("GET", "/api/v1/metadata/providers", e.admin, nil), 200, &providers)
	require.Len(t, providers.Items, 2)
	assert.False(t, providers.Items[0].Enabled, "Google needs a key")

	var cands struct {
		Candidates []struct {
			Provider   string `json:"provider"`
			ProviderID string `json:"providerId"`
			Score      int    `json:"score"`
		}
	}
	e.expect(e.call("POST", "/api/v1/books/"+b.Hash+"/metadata/search", e.admin, map[string]any{}), 200, &cands)
	require.Len(t, cands.Candidates, 1)
	assert.Equal(t, "isbn:"+isbn, cands.Candidates[0].ProviderID)
	assert.GreaterOrEqual(t, cands.Candidates[0].Score, 90)

	// The background filler (queued on ingest) fills empty fields from the exact ISBN match only.
	require.Eventually(t, func() bool {
		e.expect(e.call("GET", "/api/v1/books/"+b.Hash, e.admin, nil), 200, &b)
		return b.Description != ""
	}, 5*time.Second, 20*time.Millisecond, "worker should fill the description")
	assert.Equal(t, "First contact.", b.Description)
	assert.Empty(t, b.LockedFields, "automatic filling never locks")

	// Applying a candidate is a user edit: fields are written and locked.
	e.expect(e.call("POST", "/api/v1/books/"+b.Hash+"/metadata/apply", e.admin,
		map[string]any{"provider": "openlibrary", "providerId": "isbn:" + isbn, "fields": []string{"publisher", "tags"}}), 200, &b)
	assert.ElementsMatch(t, []string{"publisher", "tags"}, b.LockedFields)
	e.expect(e.call("PUT", "/api/v1/books/"+b.Hash+"/metadata/locks", e.admin, map[string]any{"fields": []string{}}), 200, &b)
	assert.Empty(t, b.LockedFields)
	e.expect(e.call("POST", "/api/v1/books/"+b.Hash+"/metadata/apply", e.admin, map[string]any{"provider": "googlebooks", "providerId": "x"}), 400, nil)
}

func TestValidationAndSpec(t *testing.T) {
	e := newEnv(t)
	var problem struct {
		Type, Detail string
		Status       int
	}
	e.expect(e.call("GET", "/api/v1/books?limit=999", e.admin, nil), 400, &problem)
	assert.Equal(t, "/problems/invalid-request", problem.Type)
	assert.True(t, strings.Contains(problem.Detail, "limit"), problem.Detail)
	e.expect(e.call("GET", "/api/v1/books/not-a-hash", e.admin, nil), 400, nil)
	var spec map[string]any
	e.expect(e.call("GET", "/openapi.json", "", nil), 200, &spec)
	assert.Equal(t, "3.0.3", spec["openapi"])
	e.expect(e.call("GET", "/healthz", "", nil), 200, nil)
}
