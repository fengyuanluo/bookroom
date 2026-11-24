package library

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// libraryColumns is the column list of library_books in db.LibraryBook field order.
const libraryColumns = `l.hash, l.format, l.size, l.file_name, l.original_filename, l.filename_md5, l.title, l.authors,
l.series, l.series_index, l.description, l.publisher, l.published, l.language, l.isbn, l.tags, l.page_count,
l.work_type, l.webnovel, l.cover_file, l.cover_hash, l.locked_fields, l.metadata_source, l.added_by,
l.created_at, l.updated_at`

// userState is the slice of a user's Readest books row the native API shows.
type userState struct {
	groupName     *string
	readingStatus *string
	progress      *string
	updatedAt     *int64
}

func scanBook(sc interface{ Scan(...any) error }) (db.LibraryBook, userState, error) {
	var lb db.LibraryBook
	var u userState
	err := sc.Scan(&lb.Hash, &lb.Format, &lb.Size, &lb.FileName, &lb.OriginalFilename, &lb.FilenameMd5, &lb.Title,
		&lb.Authors, &lb.Series, &lb.SeriesIndex, &lb.Description, &lb.Publisher, &lb.Published, &lb.Language,
		&lb.Isbn, &lb.Tags, &lb.PageCount, &lb.WorkType, &lb.Webnovel, &lb.CoverFile, &lb.CoverHash,
		&lb.LockedFields, &lb.MetadataSource, &lb.AddedBy, &lb.CreatedAt, &lb.UpdatedAt,
		&u.groupName, &u.readingStatus, &u.progress, &u.updatedAt)
	return lb, u, err
}

const userJoin = ` FROM library_books l LEFT JOIN books b ON b.user_id = ? AND b.book_hash = l.hash`

// Get returns one book with the user's state.
func (s *Service) Get(ctx context.Context, userID, hash string) (api.Book, error) {
	row := s.st.R.QueryRowContext(ctx, `SELECT `+libraryColumns+`, b.group_name, b.reading_status, b.progress, b.updated_at`+
		userJoin+` WHERE l.hash = ?`, userID, hash)
	lb, u, err := scanBook(row)
	if store.IsNotFound(err) {
		return api.Book{}, apperr.NotFound("no such book")
	}
	if err != nil {
		return api.Book{}, err
	}
	return toAPI(&lb, &u), nil
}

// ListQuery filters and orders a book listing.
type ListQuery struct {
	Q         string
	Shelf     *string
	Recursive bool
	Tag       string
	Format    string
	Status    string
	WorkType  string
	Sort      string // added, updated, title, author, lastRead
	Desc      bool
	Cursor    string
	Limit     int
}

// sortKey is one list order: its SQL expression and, for keyset paging, the row's value of it.
type sortKey struct {
	expr  string
	value func(lb *db.LibraryBook) any // nil: offset paging only (the value lives on the user's row)
}

var sortKeys = map[string]sortKey{
	"added":    {"l.created_at", func(lb *db.LibraryBook) any { return lb.CreatedAt }},
	"updated":  {"l.updated_at", func(lb *db.LibraryBook) any { return lb.UpdatedAt }},
	"title":    {"l.title COLLATE NOCASE", func(lb *db.LibraryBook) any { return lb.Title }},
	"author":   {"l.authors COLLATE NOCASE", func(lb *db.LibraryBook) any { return lb.Authors }},
	"lastRead": {"COALESCE(b.updated_at, 0)", nil},
}

// cursor is the decoded list cursor: an offset, or the sort value and hash of the last row returned.
type cursor struct {
	Offset int    `json:"o,omitempty"`
	Sort   string `json:"s,omitempty"`
	Desc   bool   `json:"d,omitempty"`
	Num    *int64 `json:"n,omitempty"`
	Str    string `json:"t,omitempty"`
	Hash   string `json:"h,omitempty"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// EncodeCursor is the opaque cursor of a list offset (OPDS pages).
func EncodeCursor(offset int) string { return cursor{Offset: offset}.encode() }

func decodeCursor(s string) (cursor, error) {
	var c cursor
	if s == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.Offset < 0 {
		return c, apperr.BadRequest("invalid cursor")
	}
	return c, nil
}

// filter is a WHERE fragment with its arguments.
type filter struct {
	where []string
	args  []any
}

func (f *filter) add(cond string, args ...any) {
	f.where = append(f.where, cond)
	f.args = append(f.args, args...)
}

func (f *filter) and() string {
	if len(f.where) == 0 {
		return ""
	}
	return " AND " + strings.Join(f.where, " AND ")
}

// List returns one page of books visible to the user (books they deleted in Readest are hidden). Pages are
// keyset-paged on the sort column, so deep pages cost the same as the first.
func (s *Service) List(ctx context.Context, userID string, lq ListQuery) (api.BookPage, error) {
	cur, err := decodeCursor(lq.Cursor)
	if err != nil {
		return api.BookPage{}, err
	}
	if lq.Limit <= 0 || lq.Limit > 200 {
		lq.Limit = 50
	}
	if _, ok := sortKeys[lq.Sort]; !ok {
		lq.Sort = "added"
	}
	key := sortKeys[lq.Sort]
	keyset := cur.Hash != ""
	if keyset && (cur.Sort != lq.Sort || cur.Desc != lq.Desc || key.value == nil) {
		return api.BookPage{}, apperr.BadRequest("cursor belongs to another sort order")
	}

	// Filters on library columns and on the user's row are kept apart: counting by library columns alone
	// avoids joining every library book with the user's rows.
	var lib, user filter
	if q := strings.TrimSpace(lq.Q); q != "" {
		like := "%" + escapeLike(q) + "%"
		lib.add(`(l.title LIKE ? ESCAPE '\' OR l.authors LIKE ? ESCAPE '\' OR l.series LIKE ? ESCAPE '\')`, like, like, like)
	}
	if lq.Tag != "" {
		// instr on the JSON text is a cheap prefilter; json_each makes the match exact.
		tagJSON, _ := json.Marshal(lq.Tag)
		lib.add("instr(l.tags, ?) > 0 AND EXISTS (SELECT 1 FROM json_each(l.tags) WHERE value = ?)", string(tagJSON), lq.Tag)
	}
	if lq.Format != "" {
		lib.add("l.format = ?", lq.Format)
	}
	if lq.WorkType != "" {
		lib.add("l.work_type = ?", lq.WorkType)
	}
	unshelved := false
	if lq.Shelf != nil {
		switch shelf := NormalizeShelf(*lq.Shelf); {
		case shelf == "":
			unshelved = true
			user.add("(b.group_name IS NULL OR b.group_name = '')")
		case lq.Recursive:
			// '0' follows '/' in byte order, so the range holds exactly the paths below shelf.
			user.add("(b.group_name = ? OR (b.group_name >= ? AND b.group_name < ?))", shelf, shelf+"/", shelf+"0")
		default:
			user.add("b.group_name = ?", shelf)
		}
	}
	if lq.Status != "" {
		user.add("b.reading_status = ?", lq.Status)
	}

	total, err := s.count(ctx, userID, &lib, &user, unshelved)
	if err != nil {
		return api.BookPage{}, err
	}

	dir, cmp := " ASC", ">"
	if lq.Desc {
		dir, cmp = " DESC", "<"
	}
	page := filter{args: []any{userID}}
	page.add("b.deleted_at IS NULL")
	page.where = append(page.where, lib.where...)
	page.args = append(page.args, lib.args...)
	page.where = append(page.where, user.where...)
	page.args = append(page.args, user.args...)
	if keyset {
		var v any = cur.Str
		if cur.Num != nil {
			v = *cur.Num
		}
		// A row-value comparison is a single range on the (sort column, hash) index.
		page.add(fmt.Sprintf("(%s, l.hash) %s (?, ?)", key.expr, cmp), v, cur.Hash)
	}
	query := `SELECT ` + libraryColumns + `, b.group_name, b.reading_status, b.progress, b.updated_at` + userJoin +
		` WHERE ` + strings.Join(page.where, " AND ") + ` ORDER BY ` + key.expr + dir + `, l.hash` + dir + ` LIMIT ? OFFSET ?`
	rows, err := s.st.R.QueryContext(ctx, query, append(page.args, lq.Limit+1, cur.Offset)...)
	if err != nil {
		return api.BookPage{}, err
	}
	defer rows.Close()
	out := api.BookPage{Items: make([]api.Book, 0, lq.Limit), Total: total}
	var last db.LibraryBook
	more := false
	for rows.Next() {
		if len(out.Items) == lq.Limit {
			more = true
			break
		}
		lb, u, err := scanBook(rows)
		if err != nil {
			return api.BookPage{}, err
		}
		out.Items = append(out.Items, toAPI(&lb, &u))
		last = lb
	}
	if err := rows.Err(); err != nil {
		return api.BookPage{}, err
	}
	if more {
		next := cursor{Offset: cur.Offset + lq.Limit}
		if key.value != nil {
			next = cursor{Sort: lq.Sort, Desc: lq.Desc, Hash: last.Hash}
			switch v := key.value(&last).(type) {
			case int64:
				next.Num = &v
			case string:
				next.Str = v
			}
		}
		out.NextCursor = new(next.encode())
	}
	return out, nil
}

// count counts the books a listing matches without joining every library book with the user's rows where
// it can: library filters alone count library rows minus the user's hidden ones; a shelf or status filter
// starts from the user's rows. Only "unshelved" needs the full join (a book without a row is unshelved).
func (s *Service) count(ctx context.Context, userID string, lib, user *filter, unshelved bool) (int, error) {
	var n int
	switch {
	case unshelved:
		args := append(append([]any{userID}, lib.args...), user.args...)
		err := s.st.R.QueryRowContext(ctx, `SELECT COUNT(*)`+userJoin+` WHERE b.deleted_at IS NULL`+lib.and()+user.and(), args...).Scan(&n)
		return n, err
	case len(user.where) > 0:
		args := append(append([]any{userID}, user.args...), lib.args...)
		err := s.st.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM books b JOIN library_books l ON l.hash = b.book_hash
			WHERE b.user_id = ? AND b.deleted_at IS NULL`+user.and()+lib.and(), args...).Scan(&n)
		return n, err
	}
	var all, hidden int
	where := ""
	if len(lib.where) > 0 {
		where = " WHERE " + strings.Join(lib.where, " AND ")
	}
	if err := s.st.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM library_books l`+where, lib.args...).Scan(&all); err != nil {
		return 0, err
	}
	err := s.st.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM books b JOIN library_books l ON l.hash = b.book_hash
		WHERE b.user_id = ? AND b.deleted_at IS NOT NULL`+lib.and(), append([]any{userID}, lib.args...)...).Scan(&hidden)
	return all - hidden, err
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func toAPI(lb *db.LibraryBook, u *userState) api.Book {
	b := api.Book{
		Hash: lb.Hash, Format: api.BookFormat(lb.Format), Size: lb.Size, Title: lb.Title,
		Authors: decodeList(lb.Authors), Tags: decodeList(lb.Tags), WorkType: api.WorkType(lb.WorkType),
		HasCover: lb.CoverFile != "", LockedFields: decodeFields(lb.LockedFields),
		AddedAt: time.UnixMilli(lb.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(lb.UpdatedAt).UTC(),
		OriginalFilename: nonEmpty(lb.OriginalFilename), Series: nonEmpty(lb.Series), SeriesIndex: lb.SeriesIndex,
		Description: nonEmpty(lb.Description), Publisher: nonEmpty(lb.Publisher), Published: nonEmpty(lb.Published),
		Language: nonEmpty(lb.Language), Isbn: nonEmpty(lb.Isbn), MetadataSource: nonEmpty(lb.MetadataSource),
	}
	if lb.PageCount != nil {
		b.PageCount = new(int(*lb.PageCount))
	}
	if lb.Webnovel != nil {
		var w api.WebnovelInfo
		if json.Unmarshal([]byte(*lb.Webnovel), &w) == nil {
			b.Webnovel = &w
		}
	}
	if u != nil {
		if u.groupName != nil && *u.groupName != "" {
			b.State.Shelf = u.groupName
		}
		if u.readingStatus != nil && *u.readingStatus != "" {
			b.State.ReadingStatus = new(api.ReadingStatus(*u.readingStatus))
		}
		if u.progress != nil {
			var p []float64
			if json.Unmarshal([]byte(*u.progress), &p) == nil && len(p) == 2 {
				b.State.Progress = &p
				if u.updatedAt != nil {
					b.State.LastReadAt = new(time.UnixMilli(*u.updatedAt).UTC())
				}
			}
		}
	}
	return b
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// NormalizeShelf trims a shelf path and collapses empty segments: " a//b/ " -> "a/b".
func NormalizeShelf(p string) string {
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, seg := range parts {
		if seg = strings.TrimSpace(seg); seg != "" {
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// GroupID is Readest's group id for a shelf path: md5Fingerprint (first 7 hex characters of the MD5).
func GroupID(path string) string { return md5Hex(path)[:7] }

// SetState changes a user's shelf and/or reading status of a book with fresh Readest field clocks.
func (s *Service) SetState(ctx context.Context, userID, hash string, shelf *string, shelfSet bool, status *string, statusSet bool) (api.Book, error) {
	if !shelfSet && !statusSet {
		return api.Book{}, apperr.BadRequest("nothing to update")
	}
	lb, err := s.row(ctx, hash)
	if err != nil {
		return api.Book{}, err
	}
	if !ReadestCanOpen(lb.Format) {
		return api.Book{}, apperr.Conflict(lb.Format + " books have no shelf or reading status: Readest cannot open them")
	}
	now := time.Now().UnixMilli()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		row := projection(&lb, now)
		row.UserID = userID
		if err := q.InsertBookIfAbsent(ctx, row); err != nil {
			return err
		}
		if shelfSet {
			path := ""
			if shelf != nil {
				path = *shelf
			}
			if _, err := setGroup(ctx, q, userID, hash, path, now); err != nil {
				return err
			}
		}
		if statusSet {
			var st *string
			if status != nil && *status != "" {
				st = status
			}
			if _, err := q.SetBookReadingStatus(ctx, db.SetBookReadingStatusParams{ReadingStatus: st, Now: new(now), UserID: userID, BookHash: hash}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return api.Book{}, err
	}
	return s.Get(ctx, userID, hash)
}

func (s *Service) setShelf(ctx context.Context, userID, hash, shelf string) error {
	_, err := s.SetState(ctx, userID, hash, &shelf, true, nil, false)
	return err
}

func setGroup(ctx context.Context, q *db.Queries, userID, hash, path string, now int64) (int64, error) {
	path = NormalizeShelf(path)
	p := db.SetBookGroupParams{Now: new(now), UserID: userID, BookHash: hash}
	if path != "" {
		p.GroupID, p.GroupName = new(GroupID(path)), new(path)
	}
	return q.SetBookGroup(ctx, p)
}

// Shelves lists the user's shelf tree, flattened, with every intermediate path present.
func (s *Service) Shelves(ctx context.Context, userID string) ([]api.Shelf, error) {
	rows, err := s.st.R.QueryContext(ctx, `SELECT b.group_name, COUNT(*) FROM books b
		JOIN library_books l ON l.hash = b.book_hash
		WHERE b.user_id = ? AND b.deleted_at IS NULL AND b.group_name IS NOT NULL AND b.group_name != ''
		GROUP BY b.group_name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	direct := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		direct[NormalizeShelf(name)] += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	total := map[string]int{}
	for path, n := range direct {
		segs := strings.Split(path, "/")
		for i := range segs {
			total[strings.Join(segs[:i+1], "/")] += n
		}
	}
	out := make([]api.Shelf, 0, len(total))
	for path, t := range total {
		segs := strings.Split(path, "/")
		out = append(out, api.Shelf{Path: path, Name: segs[len(segs)-1], Depth: len(segs) - 1, Count: direct[path], TotalCount: t})
	}
	slices.SortFunc(out, func(a, b api.Shelf) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// RenameShelf moves a shelf and its sub-shelves; an empty target unshelves the books.
func (s *Service) RenameShelf(ctx context.Context, userID, from, to string) (int, error) {
	from, to = NormalizeShelf(from), NormalizeShelf(to)
	if from == "" {
		return 0, apperr.BadRequest("from is empty")
	}
	if to != "" && (to == from || strings.HasPrefix(to, from+"/")) {
		if to == from {
			return 0, nil
		}
		return 0, apperr.BadRequest("cannot move a shelf into itself")
	}
	moved := 0
	now := time.Now().UnixMilli()
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		rows, err := q.BooksInShelfTree(ctx, db.BooksInShelfTreeParams{UserID: userID, Path: &from, Prefix: from + "/"})
		if err != nil {
			return err
		}
		for _, r := range rows {
			target := ""
			if to != "" {
				target = to + strings.TrimPrefix(NormalizeShelf(*r.GroupName), from)
			}
			if _, err := setGroup(ctx, q, userID, r.BookHash, target, now); err != nil {
				return err
			}
			moved++
		}
		return nil
	})
	return moved, err
}

// Tags counts books per library tag.
func (s *Service) Tags(ctx context.Context) ([]api.Tag, error) {
	rows, err := s.st.R.QueryContext(ctx, `SELECT j.value, COUNT(*) FROM library_books l, json_each(l.tags) j
		GROUP BY j.value ORDER BY j.value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Tag{}
	for rows.Next() {
		var t api.Tag
		if err := rows.Scan(&t.Name, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Metadata returns the current library metadata of a book (the scraper's query input) and whether it has
// a cover.
func (s *Service) Metadata(ctx context.Context, hash string) (Metadata, bool, error) {
	lb, err := s.row(ctx, hash)
	if err != nil {
		return Metadata{}, false, err
	}
	return Metadata{Title: lb.Title, Authors: decodeList(lb.Authors), ISBN: lb.Isbn, Language: lb.Language,
		WorkType: api.WorkType(lb.WorkType), Description: lb.Description, Publisher: lb.Publisher}, lb.CoverFile != "", nil
}
