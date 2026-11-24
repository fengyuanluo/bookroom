package readest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/auth"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// seedBooks gives one user n Readest book rows with a ~600-byte metadata blob each.
func seedBooks(b *testing.B, n int) (*Sync, string) {
	b.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	const uid = "u1"
	now := time.Now().UnixMilli()
	meta := `{"title":"t","author":"a","language":"zh","description":"` + strings.Repeat("描述", 100) + `"}`
	err = st.Tx(ctx, func(q *db.Queries) error {
		if err := q.CreateUser(ctx, db.CreateUserParams{ID: uid, Username: "u", PasswordHash: "x", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for i := range n {
			if err := q.UpsertBook(ctx, db.UpsertBookParams{
				UserID: uid, BookHash: fmt.Sprintf("%032x", i), Format: "EPUB", Title: fmt.Sprintf("Book %d", i),
				Author: "A", Tags: new(`["x"]`), Metadata: &meta, CreatedAt: &now, UpdatedAt: &now, UploadedAt: &now,
				SyncedAt: now + int64(i),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	return NewSync(st), uid
}

// BenchmarkPull20k is a new device's first books sync of a 20k library: full delta and Readest's paged pull.
// Run: go test ./internal/readest -bench Pull20k -benchtime 20x
func BenchmarkPull20k(b *testing.B) {
	s, uid := seedBooks(b, 20000)
	ctx := context.Background()
	b.Run("full-delta+json", func(b *testing.B) {
		for b.Loop() {
			res, err := s.Pull(ctx, uid, PullParams{Since: "0", Type: "books"})
			if err != nil || len(res.Books) != 20000 {
				b.Fatal(err, len(res.Books))
			}
			if _, err := json.Marshal(res); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("page-500", func(b *testing.B) {
		for b.Loop() {
			res, err := s.Pull(ctx, uid, PullParams{Since: "0", Type: "books", Limit: 500})
			if err != nil || len(res.Books) != 500 {
				b.Fatal(err, len(res.Books))
			}
		}
	})
}

// BenchmarkPush100 is one Readest push batch of 100 changed books merged against stored rows.
func BenchmarkPush100(b *testing.B) {
	s, uid := seedBooks(b, 20000)
	ctx := context.Background()
	books := make([]api.ReadestBookIn, 100)
	for i := range books {
		books[i] = api.ReadestBookIn{Hash: fmt.Sprintf("%032x", i*97), Title: new("Book"), Author: new("A"),
			Format: new("EPUB"), Progress: &[]float64{3, 100}}
	}
	user := auth.User{ID: uid}
	b.ResetTimer()
	for b.Loop() {
		ts := float64(time.Now().UnixMilli())
		for i := range books {
			books[i].UpdatedAt = &ts
		}
		res, err := s.Push(ctx, user, api.ReadestPushRequest{Books: &books})
		if err != nil || len(res.Books) != 100 {
			b.Fatal(err)
		}
	}
}
