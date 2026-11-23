package library

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/store/db"
)

type applyMode int

const (
	// applyFill writes only empty, unlocked fields (automatic scraping, docs/adr/0006).
	applyFill applyMode = iota
	// applyOverwrite writes the fields regardless of locks (the caller decides whether to lock them).
	applyOverwrite
	// applyLock overwrites and locks: a user edit.
	applyLock
)

// UpdateMetadata applies a user edit; every field present is written and locked.
func (s *Service) UpdateMetadata(ctx context.Context, userID, hash string, p api.BookMetadataPatch) (api.Book, error) {
	m := Metadata{}
	var fields []api.MetadataField
	add := func(f api.MetadataField) { fields = append(fields, f) }
	if p.Title != nil {
		if strings.TrimSpace(*p.Title) == "" {
			return api.Book{}, apperr.BadRequest("title cannot be empty")
		}
		m.Title = strings.TrimSpace(*p.Title)
		add(api.MetadataFieldTitle)
	}
	if p.Authors != nil {
		m.Authors = *p.Authors
		add(api.MetadataFieldAuthors)
	}
	if p.Series != nil {
		m.Series = strings.TrimSpace(*p.Series)
		add(api.MetadataFieldSeries)
	}
	if p.SeriesIndex != nil {
		m.SeriesIndex = p.SeriesIndex
		add(api.MetadataFieldSeriesIndex)
	}
	if p.Description != nil {
		m.Description = *p.Description
		add(api.MetadataFieldDescription)
	}
	if p.Publisher != nil {
		m.Publisher = *p.Publisher
		add(api.MetadataFieldPublisher)
	}
	if p.Published != nil {
		m.Published = *p.Published
		add(api.MetadataFieldPublished)
	}
	if p.Language != nil {
		m.Language = *p.Language
		add(api.MetadataFieldLanguage)
	}
	if p.Isbn != nil {
		if *p.Isbn != "" {
			isbn, ok := NormalizeISBN(*p.Isbn)
			if !ok {
				return api.Book{}, apperr.BadRequest("invalid ISBN")
			}
			m.ISBN = isbn
		}
		add(api.MetadataFieldIsbn)
	}
	if p.Tags != nil {
		m.Tags = *p.Tags
		add(api.MetadataFieldTags)
	}
	if p.WorkType != nil {
		m.WorkType = *p.WorkType
		add(api.MetadataFieldWorkType)
	}
	if len(fields) == 0 {
		return api.Book{}, apperr.BadRequest("no fields to update")
	}
	unlock := s.lock(hash)
	defer unlock()
	lb, err := s.row(ctx, hash)
	if err != nil {
		return api.Book{}, err
	}
	if err := s.saveMetadata(ctx, &lb, &m, fields, applyLock, ""); err != nil {
		return api.Book{}, err
	}
	return s.Get(ctx, userID, hash)
}

// SetLocks replaces the locked field set.
func (s *Service) SetLocks(ctx context.Context, userID, hash string, fields []api.MetadataField) (api.Book, error) {
	for _, f := range fields {
		if !ValidField(f) {
			return api.Book{}, apperr.BadRequest("unknown field " + string(f))
		}
	}
	unlock := s.lock(hash)
	defer unlock()
	lb, err := s.row(ctx, hash)
	if err != nil {
		return api.Book{}, err
	}
	lb.LockedFields = encodeFields(slices.Clone(fields))
	lb.UpdatedAt = time.Now().UnixMilli()
	if err := s.st.WQ.UpdateLibraryMetadata(ctx, updateParams(&lb)); err != nil {
		return api.Book{}, err
	}
	return s.Get(ctx, userID, hash)
}

// ApplyMetadata writes scraped metadata. With lock (a user picked the candidate) the chosen fields overwrite
// and are locked; without lock only empty, unlocked fields are filled. It reports the fields written.
func (s *Service) ApplyMetadata(ctx context.Context, hash string, m *Metadata, fields []api.MetadataField, lock bool, source string) ([]api.MetadataField, error) {
	if len(fields) == 0 {
		fields = AllFields
	}
	var chosen []api.MetadataField
	for _, f := range fields {
		if m.has(f) {
			chosen = append(chosen, f)
		}
	}
	unlock := s.lock(hash)
	defer unlock()
	lb, err := s.row(ctx, hash)
	if err != nil {
		return nil, err
	}
	mode := applyFill
	if lock {
		mode = applyLock
	}
	written := writable(&lb, chosen, mode)
	if len(written) == 0 {
		return nil, nil
	}
	return written, s.saveMetadata(ctx, &lb, m, written, mode, source)
}

// writable filters fields by the apply mode.
func writable(lb *db.LibraryBook, fields []api.MetadataField, mode applyMode) []api.MetadataField {
	if mode != applyFill {
		return fields
	}
	locked := decodeFields(lb.LockedFields)
	var out []api.MetadataField
	for _, f := range fields {
		if !slices.Contains(locked, f) && empty(lb, f) {
			out = append(out, f)
		}
	}
	return out
}

// saveMetadata writes fields of m into the row, persists it and propagates it to every user's Readest row.
// The caller holds the hash lock.
func (s *Service) saveMetadata(ctx context.Context, lb *db.LibraryBook, m *Metadata, fields []api.MetadataField, mode applyMode, source string) error {
	fields = writable(lb, fields, mode)
	for _, f := range fields {
		if f == api.MetadataFieldCover {
			if err := s.writeCover(lb, m.Cover, m.CoverExt); err != nil {
				return err
			}
			continue
		}
		set(lb, m, f)
	}
	if lb.Isbn != "" && (lb.WorkType == "" || lb.WorkType == string(api.WorkTypeUnknown)) {
		lb.WorkType = string(api.WorkTypePublished)
	}
	if mode == applyLock {
		lb.LockedFields = encodeFields(append(decodeFields(lb.LockedFields), fields...))
	}
	if source != "" {
		lb.MetadataSource = source
	}
	now := time.Now().UnixMilli()
	lb.UpdatedAt = now
	return s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpdateLibraryMetadata(ctx, updateParams(lb)); err != nil {
			return err
		}
		return propagate(ctx, q, lb, "", now)
	})
}

// propagate rewrites the metadata group of every user's Readest row for the book with a fresh clock.
func propagate(ctx context.Context, q *db.Queries, lb *db.LibraryBook, exceptUser string, now int64) error {
	p := db.PropagateLibraryMetadataParams{
		Title: lb.Title, Author: strings.Join(decodeList(lb.Authors), ", "), Tags: new(lb.Tags),
		Metadata: new(readestMetadata(lb)), Now: new(now), PageCount: lb.PageCount, BookHash: lb.Hash, ExceptUser: exceptUser,
	}
	if lb.CoverHash != "" {
		p.CoverHash, p.CoverUpdatedAt = new(lb.CoverHash), new(lb.UpdatedAt)
	}
	return q.PropagateLibraryMetadata(ctx, p)
}

func updateParams(lb *db.LibraryBook) db.UpdateLibraryMetadataParams {
	return db.UpdateLibraryMetadataParams{
		Title: lb.Title, Authors: lb.Authors, Series: lb.Series, SeriesIndex: lb.SeriesIndex, Description: lb.Description,
		Publisher: lb.Publisher, Published: lb.Published, Language: lb.Language, Isbn: lb.Isbn, Tags: lb.Tags,
		PageCount: lb.PageCount, WorkType: lb.WorkType, Webnovel: lb.Webnovel, CoverFile: lb.CoverFile,
		CoverHash: lb.CoverHash, LockedFields: lb.LockedFields, MetadataSource: lb.MetadataSource,
		UpdatedAt: lb.UpdatedAt, Hash: lb.Hash,
	}
}

// ReadestEdit is the metadata group of a Readest books row that won a sync merge.
type ReadestEdit struct {
	Title    string
	Author   string
	Tags     []string
	Metadata json.RawMessage // Readest BookMetadata, may be empty
}

// ApplyReadestEdit writes an admin's metadata edit made in Readest back to the library, locks the changed
// fields and propagates them to the other users. It runs inside the sync transaction q.
func ApplyReadestEdit(ctx context.Context, q *db.Queries, hash, userID string, e ReadestEdit, now int64) error {
	lb, err := q.GetLibraryBook(ctx, hash)
	if err != nil {
		return nil // not a library book: nothing to write back
	}
	var md struct {
		Language    any      `json:"language"`
		Publisher   string   `json:"publisher"`
		Published   string   `json:"published"`
		Description string   `json:"description"`
		ISBN        string   `json:"isbn"`
		Series      string   `json:"series"`
		SeriesIndex *float64 `json:"seriesIndex"`
	}
	_ = json.Unmarshal(e.Metadata, &md)
	m := Metadata{
		Title: strings.TrimSpace(e.Title), Authors: splitReadestAuthors(e.Author), Tags: e.Tags,
		Publisher: md.Publisher, Published: md.Published, Description: md.Description, Series: md.Series,
		SeriesIndex: md.SeriesIndex,
	}
	if lang, ok := md.Language.(string); ok {
		m.Language = lang
	}
	if isbn, ok := NormalizeISBN(md.ISBN); ok {
		m.ISBN = isbn
	}
	before := lb
	var changed []api.MetadataField
	check := func(f api.MetadataField, differs bool, present bool) {
		if present && differs {
			set(&lb, &m, f)
			changed = append(changed, f)
		}
	}
	check(api.MetadataFieldTitle, m.Title != before.Title, m.Title != "")
	check(api.MetadataFieldAuthors, encodeList(m.Authors) != before.Authors, len(m.Authors) > 0)
	check(api.MetadataFieldTags, encodeList(m.Tags) != before.Tags, e.Tags != nil)
	if len(e.Metadata) > 0 {
		check(api.MetadataFieldDescription, m.Description != before.Description, true)
		check(api.MetadataFieldPublisher, m.Publisher != before.Publisher, true)
		check(api.MetadataFieldPublished, m.Published != before.Published, true)
		check(api.MetadataFieldLanguage, m.Language != before.Language, m.Language != "")
		check(api.MetadataFieldSeries, m.Series != before.Series, true)
		check(api.MetadataFieldIsbn, m.ISBN != before.Isbn, m.ISBN != "")
	}
	if len(changed) == 0 {
		return nil
	}
	lb.LockedFields = encodeFields(append(decodeFields(lb.LockedFields), changed...))
	lb.UpdatedAt = now
	if err := q.UpdateLibraryMetadata(ctx, updateParams(&lb)); err != nil {
		return err
	}
	return propagate(ctx, q, &lb, userID, now)
}

// splitReadestAuthors splits Readest's joined author string (", ", " and ", " & ", "、").
func splitReadestAuthors(s string) []string {
	s = strings.NewReplacer(" and ", ",", " & ", ",", "、", ",").Replace(s)
	return cleanList(strings.Split(s, ","))
}
