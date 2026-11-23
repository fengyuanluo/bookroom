package readest

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/store/db"
)

// isoMillis renders epoch milliseconds the way JavaScript's toISOString does.
func isoMillis(v *int64) *string {
	if v == nil {
		return nil
	}
	return new(time.UnixMilli(*v).UTC().Format("2006-01-02T15:04:05.000Z"))
}

// parseISO reads an ISO-8601 timestamp (or a numeric epoch-ms string) into epoch milliseconds.
func parseISO(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z07", "2006-01-02 15:04:05.999999999Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// truthyMs maps a JavaScript `x ? new Date(x) : null` timestamp: 0 and null are null.
func truthyMs(v *float64) *int64 {
	if v == nil || *v == 0 {
		return nil
	}
	return new(int64(*v))
}

// orNow maps `new Date(x ?? Date.now())`.
func orNow(v *float64, now int64) *int64 {
	if v == nil {
		return new(now)
	}
	return new(int64(*v))
}

// sanitize removes NUL, which Readest strips before Postgres rejects it.
func sanitize(s *string) *string {
	if s == nil || !strings.ContainsRune(*s, 0) {
		return s
	}
	return new(strings.ReplaceAll(*s, "\x00", ""))
}

func sanitizeStr(s *string) string { return str(sanitize(s)) }

// rawJSON stores an opaque JSON member: absent and null both become NULL.
func rawJSON(r json.RawMessage) *string {
	if isAbsentOrNull(r) {
		return nil
	}
	var buf bytes.Buffer
	if json.Compact(&buf, r) != nil {
		return nil
	}
	return new(strings.ReplaceAll(buf.String(), `\u0000`, ""))
}

func isAbsentOrNull(r json.RawMessage) bool {
	t := bytes.TrimSpace(r)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

func jsonText(v any) *string {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return new(string(b))
}

// bookFromWire is transformBookToDB.
func bookFromWire(in *api.ReadestBookIn, userID string, now int64) db.Book {
	b := db.Book{
		UserID: userID, BookHash: in.Hash, MetaHash: in.MetaHash, Format: str(in.Format),
		Title: sanitizeStr(in.Title), Author: sanitizeStr(in.Author), GroupID: in.GroupId, GroupName: sanitize(in.GroupName),
		GroupUpdatedAt: truthyMs(in.GroupUpdatedAt), ReadingStatus: in.ReadingStatus,
		ReadingStatusUpdatedAt: truthyMs(in.ReadingStatusUpdatedAt), CoverHash: in.CoverHash,
		CoverUpdatedAt: truthyMs(in.CoverUpdatedAt), SourceTitle: sanitize(in.SourceTitle),
		OriginalFilename: sanitize(in.OriginalFilename), Metadata: rawJSON(in.Metadata),
		MetadataUpdatedAt: truthyMs(in.MetadataUpdatedAt), CreatedAt: orNow(in.CreatedAt, now),
		UpdatedAt: orNow(in.UpdatedAt, now), DeletedAt: truthyMs(in.DeletedAt), UploadedAt: truthyMs(in.UploadedAt),
	}
	if in.Tags != nil {
		b.Tags = jsonText(*in.Tags)
	}
	if in.Progress != nil {
		b.Progress = jsonText(*in.Progress)
	}
	if in.PageCount != nil {
		b.PageCount = new(int64(*in.PageCount))
	}
	return b
}

// configFromWire is transformBookConfigToDB, plus which opaque members were omitted.
func configFromWire(in *api.ReadestConfigIn, userID string, now int64) (db.BookConfig, configOmitted) {
	c := db.BookConfig{
		UserID: userID, BookHash: in.BookHash, MetaHash: in.MetaHash, Location: in.Location, Xpointer: in.Xpointer,
		Progress: rawJSON(in.Progress), RsvpPosition: rawJSON(in.RsvpPosition), SearchConfig: rawJSON(in.SearchConfig),
		ViewSettings: rawJSON(in.ViewSettings), TtsPacks: rawJSON(in.TtsPacks), UpdatedAt: orNow(in.UpdatedAt, now),
	}
	return c, configOmitted{rsvp: len(in.RsvpPosition) == 0, search: len(in.SearchConfig) == 0, view: len(in.ViewSettings) == 0}
}

// noteFromWire is transformBookNoteToDB, plus which members were omitted.
func noteFromWire(in *api.ReadestNoteIn, userID string, now int64) (db.BookNote, noteOmitted) {
	n := db.BookNote{
		UserID: userID, BookHash: in.BookHash, ID: in.Id, MetaHash: in.MetaHash, Type: in.Type, Cfi: in.Cfi,
		Xpointer0: in.Xpointer0, Xpointer1: in.Xpointer1, Page: in.Page, Text: sanitize(in.Text), Style: in.Style,
		Color: in.Color, Note: in.Note, Ink: rawJSON(in.Ink), Crosspoint: rawJSON(in.Crosspoint),
		CreatedAt: orNow(in.CreatedAt, now), UpdatedAt: orNow(in.UpdatedAt, now), DeletedAt: truthyMs(in.DeletedAt),
	}
	if !isAbsentOrNull(in.Global) {
		var g bool
		if json.Unmarshal(in.Global, &g) == nil {
			n.Global = new(boolInt(g))
		}
	}
	return n, noteOmitted{global: len(in.Global) == 0, ink: len(in.Ink) == 0, crosspoint: len(in.Crosspoint) == 0}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func bookRow(b *db.Book) api.ReadestBookRow {
	r := api.ReadestBookRow{
		UserId: b.UserID, BookHash: b.BookHash, MetaHash: b.MetaHash, Format: b.Format, Title: b.Title,
		SourceTitle: b.SourceTitle, OriginalFilename: b.OriginalFilename, Author: b.Author, GroupId: b.GroupID,
		GroupName: b.GroupName, GroupUpdatedAt: isoMillis(b.GroupUpdatedAt), ReadingStatus: b.ReadingStatus,
		ReadingStatusUpdatedAt: isoMillis(b.ReadingStatusUpdatedAt), CoverHash: b.CoverHash,
		CoverUpdatedAt: isoMillis(b.CoverUpdatedAt), Metadata: b.Metadata, MetadataUpdatedAt: isoMillis(b.MetadataUpdatedAt),
		CreatedAt: isoMillis(b.CreatedAt), UpdatedAt: isoMillis(b.UpdatedAt), DeletedAt: isoMillis(b.DeletedAt),
		UploadedAt: isoMillis(b.UploadedAt), SyncedAt: isoMillis(&b.SyncedAt),
	}
	if b.Tags != nil {
		var tags []string
		if json.Unmarshal([]byte(*b.Tags), &tags) == nil {
			r.Tags = &tags
		}
	}
	if b.Progress != nil {
		var p []float64
		if json.Unmarshal([]byte(*b.Progress), &p) == nil {
			r.Progress = &p
		}
	}
	if b.PageCount != nil {
		r.PageCount = new(int(*b.PageCount))
	}
	return r
}

func configRow(c *db.BookConfig) api.ReadestConfigRow {
	return api.ReadestConfigRow{
		UserId: c.UserID, BookHash: c.BookHash, MetaHash: c.MetaHash, Location: c.Location, Xpointer: c.Xpointer,
		Progress: c.Progress, RsvpPosition: c.RsvpPosition, SearchConfig: c.SearchConfig, ViewSettings: c.ViewSettings,
		TtsPacks: c.TtsPacks, CreatedAt: isoMillis(c.CreatedAt), UpdatedAt: isoMillis(c.UpdatedAt),
		DeletedAt: isoMillis(c.DeletedAt),
	}
}

func noteRow(n *db.BookNote) api.ReadestNoteRow {
	r := api.ReadestNoteRow{
		UserId: n.UserID, BookHash: n.BookHash, MetaHash: n.MetaHash, Id: n.ID, Type: n.Type, Cfi: n.Cfi,
		Xpointer0: n.Xpointer0, Xpointer1: n.Xpointer1, Page: n.Page, Text: n.Text, Style: n.Style, Color: n.Color,
		Note: n.Note, CreatedAt: isoMillis(n.CreatedAt), UpdatedAt: isoMillis(n.UpdatedAt), DeletedAt: isoMillis(n.DeletedAt),
		Ink: json.RawMessage("null"), Crosspoint: json.RawMessage("null"),
	}
	if n.Global != nil {
		r.Global = new(*n.Global != 0)
	}
	if n.Ink != nil {
		r.Ink = json.RawMessage(*n.Ink)
	}
	if n.Crosspoint != nil {
		r.Crosspoint = json.RawMessage(*n.Crosspoint)
	}
	return r
}

func statBookRow(b *db.StatBook) api.ReadestStatBook {
	r := api.ReadestStatBook{
		UserId: new(b.UserID), BookHash: b.BookHash, Title: new(b.Title), Authors: new(b.Authors),
		UpdatedAt: isoMillis(&b.UpdatedAt), UpdatedAtMs: new(b.UpdatedAt),
	}
	if b.DeletedAt != nil {
		r.DeletedAt, r.DeletedAtMs = isoMillis(b.DeletedAt), b.DeletedAt
	}
	return r
}

func statPageRow(p *db.StatPage) api.ReadestStatPage {
	r := api.ReadestStatPage{
		UserId: new(p.UserID), BookHash: p.BookHash, Page: float64(p.Page), StartTime: float64(p.StartTime),
		Duration: float64(p.Duration), TotalPages: new(float64(p.TotalPages)), UpdatedAt: isoMillis(&p.UpdatedAt),
		UpdatedAtMs: new(p.UpdatedAt),
	}
	if p.Ext != nil {
		r.Ext = json.RawMessage(*p.Ext)
	}
	if p.DeletedAt != nil {
		r.DeletedAt, r.DeletedAtMs = isoMillis(p.DeletedAt), p.DeletedAt
	}
	return r
}
