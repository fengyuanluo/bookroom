package readest

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/auth"
	"bookroom/internal/library"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// batch bounds the IN (...) lists of one query.
const batch = 500

// Sync implements GET/POST /api/sync.
type Sync struct {
	st *store.Store
}

// NewSync builds the sync service.
func NewSync(st *store.Store) *Sync { return &Sync{st: st} }

// Fail builds a Readest-style error.
func Fail(status int, msg string) *apperr.Error {
	return apperr.WithBody(status, api.ReadestError{Error: msg})
}

// PullParams are the query parameters of GET /api/sync.
type PullParams struct {
	Since    string
	Type     string
	Book     string
	MetaHash string
	Limit    int
}

// Pull returns rows changed since the cursor.
func (s *Sync) Pull(ctx context.Context, userID string, p PullParams) (api.ReadestPullResponse, error) {
	res := api.ReadestPullResponse{
		Books: []api.ReadestBookRow{}, Configs: []api.ReadestConfigRow{}, Notes: []api.ReadestNoteRow{},
		StatBooks: []api.ReadestStatBook{}, StatPages: []api.ReadestStatPage{},
	}
	if p.Since == "" {
		return res, Fail(http.StatusBadRequest, `"since" query parameter is required`)
	}
	f, err := strconv.ParseFloat(p.Since, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return res, Fail(http.StatusBadRequest, `Invalid "since" timestamp`)
	}
	since := int64(f)
	var book, meta any
	if p.Book != "" {
		book = p.Book
	}
	if p.MetaHash != "" {
		meta = p.MetaHash
	}
	q := s.st.Q

	if p.Type == "" || p.Type == "books" {
		var rows []db.Book
		if p.Limit > 0 && p.Type == "books" {
			rows, err = q.PullBooksPage(ctx, db.PullBooksPageParams{UserID: userID, Since: since, Book: book, Meta: meta, Lim: int64(p.Limit)})
			if err == nil && len(rows) == p.Limit {
				// Complete the page to its trailing synced_at so a strict `> cursor` skips nothing.
				var extra []db.Book
				extra, err = q.BooksAtSyncedAt(ctx, db.BooksAtSyncedAtParams{UserID: userID, SyncedAt: rows[len(rows)-1].SyncedAt, Book: book, Meta: meta})
				seen := map[string]bool{}
				for _, r := range rows {
					seen[r.BookHash] = true
				}
				for _, r := range extra {
					if !seen[r.BookHash] {
						rows = append(rows, r)
					}
				}
			}
		} else {
			rows, err = q.PullBooks(ctx, db.PullBooksParams{UserID: userID, Since: since, Book: book, Meta: meta})
		}
		if err != nil {
			return res, err
		}
		for i := range rows {
			res.Books = append(res.Books, bookRow(&rows[i]))
		}
		// Readest's hotfix for its initial-sync race: an empty first pull carries one deleted dummy book.
		if len(res.Books) == 0 && since < 1000 {
			now := time.Now().UnixMilli()
			dummy := db.Book{UserID: userID, BookHash: "00000000000000000000000000000000", Format: "EPUB", Title: "Dummy Book",
				CreatedAt: &now, UpdatedAt: &now, DeletedAt: &now, SyncedAt: now}
			res.Books = append(res.Books, bookRow(&dummy))
		}
	}
	if p.Type == "" || p.Type == "configs" {
		rows, err := q.PullConfigs(ctx, db.PullConfigsParams{UserID: userID, Since: new(since), Book: book, Meta: meta})
		if err != nil {
			return res, err
		}
		for i := range rows {
			res.Configs = append(res.Configs, configRow(&rows[i]))
		}
	}
	if p.Type == "" || p.Type == "notes" {
		rows, err := q.PullNotes(ctx, db.PullNotesParams{UserID: userID, Since: new(since), Book: book, Meta: meta})
		if err != nil {
			return res, err
		}
		rows = dedupeNotes(rows, func(n *db.BookNote) string { return n.ID })
		for i := range rows {
			res.Notes = append(res.Notes, noteRow(&rows[i]))
		}
	}
	if p.Type == "" || p.Type == "stats" {
		if err := s.pullStats(ctx, userID, since, book, p.Limit, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (s *Sync) pullStats(ctx context.Context, userID string, since int64, book any, limit int, res *api.ReadestPullResponse) error {
	q := s.st.Q
	statBooks, err := q.PullStatBooks(ctx, db.PullStatBooksParams{UserID: userID, UpdatedAt: since})
	if err != nil {
		return err
	}
	var pages []db.StatPage
	if limit > 0 {
		pages, err = q.PullStatPagesPage(ctx, db.PullStatPagesPageParams{UserID: userID, Since: since, Book: book, Lim: int64(limit)})
		if err == nil && len(pages) == limit {
			var extra []db.StatPage
			extra, err = q.StatPagesAt(ctx, db.StatPagesAtParams{UserID: userID, UpdatedAt: pages[len(pages)-1].UpdatedAt, Book: book})
			type key struct {
				h    string
				p, t int64
			}
			seen := map[key]bool{}
			for _, r := range pages {
				seen[key{r.BookHash, r.Page, r.StartTime}] = true
			}
			for _, r := range extra {
				if !seen[key{r.BookHash, r.Page, r.StartTime}] {
					pages = append(pages, r)
				}
			}
		}
	} else {
		pages, err = q.PullStatPages(ctx, db.PullStatPagesParams{UserID: userID, Since: since, Book: book})
	}
	if err != nil {
		return err
	}
	byHash := map[string]db.StatBook{}
	order := []string{}
	for _, b := range statBooks {
		byHash[b.BookHash] = b
		order = append(order, b.BookHash)
	}
	// A statistics deletion (#7494) flags every earlier event of the book; deletions committed after the
	// stat_books read above are returned too because the cursor moves on pages alone.
	deletedAt := map[string]int64{}
	if len(pages) > 0 {
		deletions, err := q.DeletedStatBooks(ctx, userID)
		if err != nil {
			return err
		}
		for _, d := range deletions {
			deletedAt[d.BookHash] = ms(d.DeletedAt)
			if d.UpdatedAt > since {
				if _, ok := byHash[d.BookHash]; !ok {
					order = append(order, d.BookHash)
				}
				byHash[d.BookHash] = d
			}
		}
	}
	for _, h := range order {
		b := byHash[h]
		res.StatBooks = append(res.StatBooks, statBookRow(&b))
	}
	for i := range pages {
		p := &pages[i]
		if at, ok := deletedAt[p.BookHash]; ok && p.StartTime*1000 < at {
			p.DeletedAt = new(at)
		}
		res.StatPages = append(res.StatPages, statPageRow(p))
	}
	return nil
}

// Push merges a client's changes and returns the authoritative rows.
func (s *Sync) Push(ctx context.Context, user auth.User, req api.ReadestPushRequest) (api.ReadestPushResponse, error) {
	res := api.ReadestPushResponse{Books: []api.ReadestBookRow{}, Configs: []api.ReadestConfigRow{}, Notes: []api.ReadestNoteRow{}}
	now := time.Now().UnixMilli()
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		if req.Books != nil {
			if err := pushBooks(ctx, q, user, *req.Books, now, &res); err != nil {
				return err
			}
		}
		if req.Configs != nil {
			if err := pushConfigs(ctx, q, user.ID, *req.Configs, now, &res); err != nil {
				return err
			}
		}
		if req.Notes != nil {
			if err := pushNotes(ctx, q, user.ID, *req.Notes, now, &res); err != nil {
				return err
			}
		}
		if req.StatBooks != nil {
			for _, b := range *req.StatBooks {
				if b.BookHash == "" {
					continue
				}
				var err error
				if b.DeletedAt != nil && *b.DeletedAt != "" || b.DeletedAtMs != nil && *b.DeletedAtMs != 0 {
					err = q.DeleteStatBook(ctx, db.DeleteStatBookParams{UserID: user.ID, BookHash: b.BookHash, Title: str(b.Title), Authors: str(b.Authors), Now: now})
				} else {
					err = q.UpsertStatBook(ctx, db.UpsertStatBookParams{UserID: user.ID, BookHash: b.BookHash, Title: str(b.Title), Authors: str(b.Authors), UpdatedAt: now})
				}
				if err != nil {
					return err
				}
			}
		}
		if req.StatPages != nil {
			for _, p := range *req.StatPages {
				if p.BookHash == "" {
					continue
				}
				row := db.UpsertStatPageParams{
					UserID: user.ID, BookHash: p.BookHash, Page: int64(p.Page), StartTime: int64(p.StartTime),
					Duration: int64(p.Duration), Ext: rawJSON(p.Ext), UpdatedAt: now,
				}
				if p.TotalPages != nil {
					row.TotalPages = int64(*p.TotalPages)
				}
				if p.DeletedAt != nil {
					if t, ok := parseISO(*p.DeletedAt); ok {
						row.DeletedAt = &t
					}
				}
				if err := q.UpsertStatPage(ctx, row); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return res, err
}

func pushBooks(ctx context.Context, q *db.Queries, user auth.User, in []api.ReadestBookIn, now int64, res *api.ReadestPushResponse) error {
	for start := 0; start < len(in); start += batch {
		chunk := in[start:min(start+batch, len(in))]
		hashes := make([]string, 0, len(chunk))
		for i := range chunk {
			hashes = append(hashes, chunk[i].Hash)
		}
		existing, err := q.GetBooksByHashes(ctx, db.GetBooksByHashesParams{UserID: user.ID, Hashes: hashes})
		if err != nil {
			return err
		}
		server := make(map[string]*db.Book, len(existing))
		for i := range existing {
			server[existing[i].BookHash] = &existing[i]
		}
		lib, err := q.GetLibraryBooksByHashes(ctx, hashes)
		if err != nil {
			return err
		}
		inLibrary := make(map[string]*db.LibraryBook, len(lib))
		for i := range lib {
			inLibrary[lib[i].Hash] = &lib[i]
		}
		for i := range chunk {
			if chunk[i].Hash == "" {
				continue
			}
			client := bookFromWire(&chunk[i], user.ID, now)
			stored := server[client.BookHash]
			var write *db.Book
			if stored == nil {
				client.UpdatedAt = new(now) // an insert is stamped now (Readest does the same)
				write = &client
			} else {
				write = mergeBook(&client, stored)
			}
			lb := inLibrary[client.BookHash]
			if write == nil {
				res.Books = append(res.Books, bookRow(stored))
				continue
			}
			// A library book is always downloadable: a device that never uploaded it must not hide that.
			if lb != nil && write.UploadedAt == nil {
				write.UploadedAt = new(lb.CreatedAt)
			}
			write.SyncedAt = now
			if err := q.UpsertBook(ctx, upsertBookParams(write)); err != nil {
				return err
			}
			server[write.BookHash] = write
			// An admin's metadata edit made in Readest becomes the library's metadata (locked).
			if lb != nil && user.IsAdmin && ms(write.MetadataUpdatedAt) > lb.UpdatedAt &&
				(stored == nil || ms(write.MetadataUpdatedAt) > ms(stored.MetadataUpdatedAt)) {
				var tags []string
				if write.Tags != nil {
					_ = json.Unmarshal([]byte(*write.Tags), &tags)
				}
				edit := library.ReadestEdit{Title: write.Title, Author: write.Author, Tags: tags}
				if write.Metadata != nil {
					edit.Metadata = json.RawMessage(*write.Metadata)
				}
				if err := library.ApplyReadestEdit(ctx, q, write.BookHash, user.ID, edit, now); err != nil {
					return err
				}
			}
			res.Books = append(res.Books, bookRow(write))
		}
	}
	return nil
}

func upsertBookParams(b *db.Book) db.UpsertBookParams {
	return db.UpsertBookParams{
		UserID: b.UserID, BookHash: b.BookHash, MetaHash: b.MetaHash, Format: b.Format, Title: b.Title,
		SourceTitle: b.SourceTitle, OriginalFilename: b.OriginalFilename, Author: b.Author, GroupID: b.GroupID,
		GroupName: b.GroupName, GroupUpdatedAt: b.GroupUpdatedAt, Tags: b.Tags, Progress: b.Progress,
		ReadingStatus: b.ReadingStatus, ReadingStatusUpdatedAt: b.ReadingStatusUpdatedAt, CoverHash: b.CoverHash,
		CoverUpdatedAt: b.CoverUpdatedAt, PageCount: b.PageCount, Metadata: b.Metadata,
		MetadataUpdatedAt: b.MetadataUpdatedAt, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, DeletedAt: b.DeletedAt,
		UploadedAt: b.UploadedAt, SyncedAt: b.SyncedAt,
	}
}

func pushConfigs(ctx context.Context, q *db.Queries, userID string, in []api.ReadestConfigIn, now int64, res *api.ReadestPushResponse) error {
	var written []db.BookConfig
	for start := 0; start < len(in); start += batch {
		chunk := in[start:min(start+batch, len(in))]
		hashes := make([]string, 0, len(chunk))
		for i := range chunk {
			hashes = append(hashes, chunk[i].BookHash)
		}
		existing, err := q.GetConfigsByHashes(ctx, db.GetConfigsByHashesParams{UserID: userID, Hashes: hashes})
		if err != nil {
			return err
		}
		server := make(map[string]*db.BookConfig, len(existing))
		for i := range existing {
			server[existing[i].BookHash] = &existing[i]
		}
		for i := range chunk {
			if chunk[i].BookHash == "" {
				continue
			}
			client, om := configFromWire(&chunk[i], userID, now)
			stored := server[client.BookHash]
			var write *db.BookConfig
			if stored == nil {
				// A pack-only seed (updatedAt 0) must not outrank the reader's saved position: keep it at 1 ms.
				stamp := now
				if chunk[i].UpdatedAt != nil && *chunk[i].UpdatedAt == 0 && client.TtsPacks != nil {
					stamp = 1
				}
				client.UpdatedAt, client.CreatedAt = new(stamp), new(now)
				write = &client
			} else {
				write = mergeConfig(&client, stored, om)
			}
			if write == nil {
				res.Configs = append(res.Configs, configRow(stored))
				written = append(written, *stored)
				continue
			}
			if err := q.UpsertConfig(ctx, db.UpsertConfigParams{
				UserID: write.UserID, BookHash: write.BookHash, MetaHash: write.MetaHash, Location: write.Location,
				Xpointer: write.Xpointer, Progress: write.Progress, RsvpPosition: write.RsvpPosition,
				SearchConfig: write.SearchConfig, ViewSettings: write.ViewSettings, TtsPacks: write.TtsPacks,
				CreatedAt: write.CreatedAt, UpdatedAt: write.UpdatedAt, DeletedAt: write.DeletedAt,
			}); err != nil {
				return err
			}
			res.Configs = append(res.Configs, configRow(write))
			written = append(written, *write)
		}
	}
	// Piggyback reading progress onto the books row so other devices' library views refresh; never
	// downgrade a newer books row.
	for i := range written {
		c := &written[i]
		if c.Progress == nil || c.UpdatedAt == nil {
			continue
		}
		var p []float64
		if json.Unmarshal([]byte(*c.Progress), &p) != nil || len(p) != 2 {
			continue
		}
		if err := q.SetBookProgressIfOlder(ctx, db.SetBookProgressIfOlderParams{
			Progress: jsonText(p), UpdatedAt: c.UpdatedAt, SyncedAt: now, UserID: userID, BookHash: c.BookHash,
		}); err != nil {
			return err
		}
	}
	return nil
}

func pushNotes(ctx context.Context, q *db.Queries, userID string, in []api.ReadestNoteIn, now int64, res *api.ReadestPushResponse) error {
	for start := 0; start < len(in); start += batch {
		chunk := in[start:min(start+batch, len(in))]
		ids := make([]string, 0, len(chunk))
		for i := range chunk {
			ids = append(ids, chunk[i].Id)
		}
		existing, err := q.GetNotesByIDs(ctx, db.GetNotesByIDsParams{UserID: userID, Ids: ids})
		if err != nil {
			return err
		}
		server := map[string]*db.BookNote{}
		var withMeta []db.BookNote
		for i := range existing {
			server[existing[i].BookHash+"|"+existing[i].ID] = &existing[i]
			if str(existing[i].MetaHash) != "" {
				withMeta = append(withMeta, existing[i])
			}
		}
		// The same note may live under another copy of the book (meta_hash bridges them).
		otherCopies := map[string]*db.BookNote{}
		for _, n := range dedupeNotes(withMeta, func(n *db.BookNote) string { return n.ID + "|" + str(n.MetaHash) }) {
			otherCopies[n.ID+"|"+str(n.MetaHash)] = &n
		}
		for i := range chunk {
			if chunk[i].BookHash == "" || chunk[i].Id == "" {
				continue
			}
			client, om := noteFromWire(&chunk[i], userID, now)
			stored := server[client.BookHash+"|"+client.ID]
			var write *db.BookNote
			if stored == nil {
				if cp := otherCopies[client.ID+"|"+str(client.MetaHash)]; cp != nil && !outranksCopy(&client, cp) {
					res.Notes = append(res.Notes, noteRow(cp))
					continue
				}
				client.UpdatedAt = new(now)
				write = &client
			} else {
				write = mergeNote(&client, stored, om)
			}
			if write == nil {
				res.Notes = append(res.Notes, noteRow(stored))
				continue
			}
			if err := q.UpsertNote(ctx, db.UpsertNoteParams{
				UserID: write.UserID, BookHash: write.BookHash, ID: write.ID, MetaHash: write.MetaHash, Type: write.Type,
				Cfi: write.Cfi, Xpointer0: write.Xpointer0, Xpointer1: write.Xpointer1, Page: write.Page, Text: write.Text,
				Style: write.Style, Color: write.Color, Note: write.Note, Global: write.Global, Ink: write.Ink,
				Crosspoint: write.Crosspoint, CreatedAt: write.CreatedAt, UpdatedAt: write.UpdatedAt, DeletedAt: write.DeletedAt,
			}); err != nil {
				return err
			}
			server[write.BookHash+"|"+write.ID] = write
			res.Notes = append(res.Notes, noteRow(write))
		}
	}
	return nil
}
