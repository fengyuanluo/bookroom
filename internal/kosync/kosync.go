// Package kosync implements the KOReader sync protocol. Positions are stored per (user, document); EPUB
// XPointers are also written into the Readest book config of the matching library book so that Readest and
// KOReader (with or without readest.koplugin) continue each other's reading. The mirroring follows Readest's
// own CrossPoint KOSync server (app/api/crosspoint/syncs/progress).
package kosync

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// Error codes of the reference server (config/errors.lua).
const (
	CodeUnauthorized     = 2001
	CodeInvalidRequest   = 2003
	CodeInvalidDocument  = 2004
	CodeRegistrationOff  = 2005
	percentPages         = 100 // page count for books Readest has not paginated yet: whole percents
	maxProgressLength    = 4096
	readestDevice        = "Readest"
	readestDeviceID      = "readest"
	readestPositionScope = "/body"
)

// Fail builds a KOSync-style error.
func Fail(status, code int, msg string) *apperr.Error {
	return apperr.WithBody(status, api.KosyncError{Code: code, Message: msg})
}

// Service implements the protocol.
type Service struct {
	st *store.Store
}

// New builds the service.
func New(st *store.Store) *Service { return &Service{st: st} }

func validDocument(d string) bool {
	return d != "" && len(d) <= 128 && !strings.ContainsAny(d, ":/ \t\r\n")
}

// Put stores a position and mirrors an EPUB XPointer into Readest.
func (s *Service) Put(ctx context.Context, userID string, in api.KosyncProgressIn) (api.KosyncPutResult, error) {
	if !validDocument(in.Document) {
		return api.KosyncPutResult{}, Fail(http.StatusForbidden, CodeInvalidDocument, "Field 'document' not provided.")
	}
	if in.Progress == "" || in.Device == "" || len(in.Progress) > maxProgressLength || math.IsNaN(in.Percentage) {
		return api.KosyncPutResult{}, Fail(http.StatusForbidden, CodeInvalidRequest, "Invalid request")
	}
	now := time.Now()
	nowMs := now.UnixMilli()
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpsertKosyncProgress(ctx, db.UpsertKosyncProgressParams{
			UserID: userID, Document: in.Document, Progress: in.Progress, Percentage: in.Percentage,
			Device: in.Device, DeviceID: deref(in.DeviceId), Timestamp: nowMs,
		}); err != nil {
			return err
		}
		if !strings.HasPrefix(in.Progress, readestPositionScope) {
			return nil // page numbers of fixed-layout books stay KOSync-only
		}
		bookHash, newLink, err := resolve(ctx, q, userID, in.Document, in.Metadata, true)
		if err != nil || bookHash == "" {
			return err
		}
		return mirror(ctx, q, userID, bookHash, newLink, in.Progress, in.Percentage, nowMs)
	})
	if err != nil {
		return api.KosyncPutResult{}, err
	}
	return api.KosyncPutResult{Document: in.Document, Timestamp: now.Unix()}, nil
}

// mirror writes the XPointer into book_configs unless that would move Readest backwards: when Readest has a
// page position but no XPointer yet (or the document was just linked), a smaller percentage from a new
// device does not overwrite it.
func mirror(ctx context.Context, q *db.Queries, userID, bookHash string, newLink bool, xp string, pct float64, now int64) error {
	cfg, err := q.GetConfig(ctx, db.GetConfigParams{UserID: userID, BookHash: bookHash})
	exists := err == nil
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	stored := parsePages(cfg.Progress)
	if stored != nil && (newLink || cfg.Xpointer == nil || *cfg.Xpointer == "") && pct < stored[0]/stored[1] {
		return nil
	}
	total := float64(percentPages)
	if stored != nil {
		total = stored[1]
	}
	pages := []float64{math.Max(1, math.Round(pct*total)), total}
	pagesJSON, _ := json.Marshal(pages)
	if exists && cfg.UpdatedAt != nil && *cfg.UpdatedAt >= now {
		return nil
	}
	if !exists {
		cfg = db.BookConfig{UserID: userID, BookHash: bookHash, CreatedAt: &now}
	}
	cfg.Xpointer, cfg.Progress, cfg.UpdatedAt = &xp, new(string(pagesJSON)), &now
	if err := q.UpsertConfig(ctx, db.UpsertConfigParams{
		UserID: cfg.UserID, BookHash: cfg.BookHash, MetaHash: cfg.MetaHash, Location: cfg.Location, Xpointer: cfg.Xpointer,
		Progress: cfg.Progress, RsvpPosition: cfg.RsvpPosition, SearchConfig: cfg.SearchConfig, ViewSettings: cfg.ViewSettings,
		TtsPacks: cfg.TtsPacks, CreatedAt: cfg.CreatedAt, UpdatedAt: cfg.UpdatedAt, DeletedAt: cfg.DeletedAt,
	}); err != nil {
		return err
	}
	return q.SetBookProgressIfOlder(ctx, db.SetBookProgressIfOlderParams{
		Progress: new(string(pagesJSON)), UpdatedAt: &now, SyncedAt: now, UserID: userID, BookHash: bookHash,
	})
}

func parsePages(p *string) []float64 {
	if p == nil {
		return nil
	}
	var v []float64
	if json.Unmarshal([]byte(*p), &v) != nil || len(v) != 2 || v[1] <= 0 {
		return nil
	}
	return v
}

// resolve finds the library book a document stands for: a saved link, the user's book with that hash
// (binary checksum mode), the book whose file name hashes to it (file-name mode), or an EPUB with the same
// title and first author. New links are saved when save is set.
func resolve(ctx context.Context, q *db.Queries, userID, document string, metadata *map[string]any, save bool) (string, bool, error) {
	if h, err := q.GetKosyncLink(ctx, db.GetKosyncLinkParams{UserID: userID, Document: document}); err == nil {
		return h, false, nil
	} else if !store.IsNotFound(err) {
		return "", false, err
	}
	if _, err := q.GetBook(ctx, db.GetBookParams{UserID: userID, BookHash: document}); err == nil {
		return document, false, nil
	} else if !store.IsNotFound(err) {
		return "", false, err
	}
	link := ""
	if hashes, err := q.UserBookHashesByFilenameMD5(ctx, db.UserBookHashesByFilenameMD5Params{UserID: userID, FilenameMd5: document}); err != nil {
		return "", false, err
	} else if len(hashes) > 0 {
		link = hashes[0]
	}
	if link == "" && metadata != nil {
		title, _ := (*metadata)["title"].(string)
		authors, _ := (*metadata)["authors"].(string)
		if title = strings.TrimSpace(title); title != "" {
			rows, err := q.UserBooksByTitle(ctx, db.UserBooksByTitleParams{UserID: userID, Title: title})
			if err != nil {
				return "", false, err
			}
			first := firstAuthor(authors)
			for _, r := range rows {
				if len(rows) == 1 || first != "" && firstAuthor(r.Author) == first {
					link = r.BookHash
					break
				}
			}
		}
	}
	if link == "" {
		return "", false, nil
	}
	if save {
		if err := q.SaveKosyncLink(ctx, db.SaveKosyncLinkParams{UserID: userID, Document: document, BookHash: link}); err != nil {
			return "", false, err
		}
	}
	return link, true, nil
}

func firstAuthor(s string) string {
	s = strings.NewReplacer(" and ", ",", " & ", ",", "、", ",").Replace(strings.ToLower(s))
	first, _, _ := strings.Cut(s, ",")
	return strings.TrimSpace(first)
}

// Get returns the newest position: KOSync's own or Readest's XPointer, whichever is newer.
func (s *Service) Get(ctx context.Context, userID, document string) (api.KosyncProgress, error) {
	var best api.KosyncProgress
	var bestAt int64 = -1
	q := s.st.Q
	if kp, err := q.GetKosyncProgress(ctx, db.GetKosyncProgressParams{UserID: userID, Document: document}); err == nil {
		best = api.KosyncProgress{Document: &document, Progress: &kp.Progress, Percentage: &kp.Percentage,
			Device: &kp.Device, Timestamp: new(kp.Timestamp / 1000)}
		if kp.DeviceID != "" {
			best.DeviceId = &kp.DeviceID
		}
		bestAt = kp.Timestamp
	} else if !store.IsNotFound(err) {
		return best, err
	}
	bookHash, _, err := resolve(ctx, q, userID, document, nil, false)
	if err != nil {
		return best, err
	}
	if bookHash != "" {
		cfg, err := q.GetConfig(ctx, db.GetConfigParams{UserID: userID, BookHash: bookHash})
		if err == nil && cfg.Xpointer != nil && *cfg.Xpointer != "" && cfg.DeletedAt == nil && cfg.UpdatedAt != nil && *cfg.UpdatedAt > bestAt {
			pct := 0.0
			if p := parsePages(cfg.Progress); p != nil {
				pct = p[0] / p[1]
			}
			best = api.KosyncProgress{Document: &document, Progress: cfg.Xpointer, Percentage: &pct,
				Device: new(readestDevice), DeviceId: new(readestDeviceID), Timestamp: new(*cfg.UpdatedAt / 1000)}
		} else if err != nil && !store.IsNotFound(err) {
			return best, err
		}
	}
	return best, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
