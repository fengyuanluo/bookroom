package library

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

var seedNow = time.Now().UnixMilli()

// seed fills a library of n books projected into one user's Readest rows, a third of them shelved.
func seed(b *testing.B, n int) (*Service, string) {
	b.Helper()
	dir := b.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	lib, err := New(st, dir, 1<<30, false)
	if err != nil {
		b.Fatal(err)
	}
	const uid = "u1"
	now := seedNow
	err = st.Tx(ctx, func(q *db.Queries) error {
		if err := q.CreateUser(ctx, db.CreateUserParams{ID: uid, Username: "u", PasswordHash: "x", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for i := range n {
			lb := db.LibraryBook{
				Hash: fmt.Sprintf("%032x", i), Format: "EPUB", Size: 1, FileName: "f.epub", OriginalFilename: "f.epub",
				FilenameMd5: fmt.Sprintf("%032x", i), Title: fmt.Sprintf("第%d卷 修仙传说 Book %d", i, i),
				Authors: fmt.Sprintf(`["作者%d"]`, i%500), Tags: fmt.Sprintf(`["tag%d"]`, i%50), WorkType: string(api.WorkTypeUnknown),
				LockedFields: "[]", CreatedAt: now + int64(i), UpdatedAt: now + int64(i),
			}
			if err := q.InsertLibraryBook(ctx, insertParams(&lb)); err != nil {
				return err
			}
			row := projection(&lb, now)
			row.UserID = uid
			if err := q.InsertBookIfAbsent(ctx, row); err != nil {
				return err
			}
			if i%3 == 0 {
				if _, err := setGroup(ctx, q, uid, lb.Hash, fmt.Sprintf("小说/分类%d", i%20), now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := st.W.ExecContext(ctx, "ANALYZE"); err != nil { // what housekeeping's PRAGMA optimize does over time
		b.Fatal(err)
	}
	return lib, uid
}

// BenchmarkList20k measures the native list endpoint's query at the target library size.
// Run: go test ./internal/library -bench List20k -benchtime 200x
func BenchmarkList20k(b *testing.B) {
	lib, uid := seed(b, 20000)
	ctx := context.Background()
	shelf := "小说/分类7"
	cases := []struct {
		name string
		q    ListQuery
	}{
		{"newest", ListQuery{Sort: "added", Desc: true, Limit: 50}},
		{"title", ListQuery{Sort: "title", Limit: 50}},
		{"search-2-chars", ListQuery{Q: "修仙", Limit: 50}},
		{"search-rare", ListQuery{Q: "Book 19999", Limit: 50}},
		{"shelf", ListQuery{Shelf: &shelf, Limit: 50}},
		{"tag", ListQuery{Tag: "tag7", Limit: 50}},
		// Book i was added at now+i, so position 15000 of the newest-first order is book 4999.
		{"deep-page-keyset", ListQuery{Sort: "added", Desc: true, Limit: 50,
			Cursor: cursor{Sort: "added", Desc: true, Num: new(seedNow + 4999), Hash: fmt.Sprintf("%032x", 4999)}.encode()}},
		{"deep-page-offset", ListQuery{Sort: "added", Desc: true, Limit: 50, Cursor: EncodeCursor(15000)}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := lib.List(ctx, uid, c.q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("shelves", func(b *testing.B) {
		for b.Loop() {
			if _, err := lib.Shelves(ctx, uid); err != nil {
				b.Fatal(err)
			}
		}
	})
}
