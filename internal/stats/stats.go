// Package stats aggregates KOReader page events (stat_pages) into reading time per day and per book. Nothing
// is precomputed: events are the source of truth (docs/adr/0008-reading-stats-raw-events.md).
package stats

import (
	"cmp"
	"context"
	"math"
	"slices"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// Service computes statistics.
type Service struct {
	st *store.Store
}

// New builds the service.
func New(st *store.Store) *Service { return &Service{st: st} }

// Range is an inclusive date range in a time zone.
type Range struct {
	From, To time.Time // midnight of the first day and of the day after the last, in Loc
	Loc      *time.Location
}

// NewRange resolves optional dates (default: the last 30 days) and an optional IANA zone.
func NewRange(from, to *openapi_types.Date, tz *string) (Range, error) {
	loc := time.Local
	if tz != nil && *tz != "" {
		l, err := time.LoadLocation(*tz)
		if err != nil {
			return Range{}, apperr.BadRequest("unknown time zone " + *tz)
		}
		loc = l
	}
	now := time.Now().In(loc)
	end := midnight(now, loc).AddDate(0, 0, 1)
	if to != nil {
		end = midnight(to.Time, loc).AddDate(0, 0, 1)
	}
	start := end.AddDate(0, 0, -30)
	if from != nil {
		start = midnight(from.Time, loc)
	}
	if !start.Before(end) {
		return Range{}, apperr.BadRequest("from must not be after to")
	}
	return Range{From: start, To: end, Loc: loc}, nil
}

func midnight(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

func (s *Service) events(ctx context.Context, userID string, from, to int64) ([]db.StatEventsRow, error) {
	return s.st.Q.StatEvents(ctx, db.StatEventsParams{UserID: userID, FromS: from, ToS: to})
}

func day(sec int64, loc *time.Location) string {
	return time.Unix(sec, 0).In(loc).Format(time.DateOnly)
}

type pageKey struct {
	hash string
	page int64
}

// Summary totals a range and computes streaks over the whole history.
func (s *Service) Summary(ctx context.Context, userID string, r Range) (api.StatsSummary, error) {
	all, err := s.events(ctx, userID, 0, math.MaxInt64)
	if err != nil {
		return api.StatsSummary{}, err
	}
	out := api.StatsSummary{From: openapi_types.Date{Time: r.From}, To: openapi_types.Date{Time: r.To.AddDate(0, 0, -1)}}
	pages, books, days := map[pageKey]bool{}, map[string]bool{}, map[string]bool{}
	allDays := map[string]bool{}
	from, to := r.From.Unix(), r.To.Unix()
	for _, e := range all {
		d := day(e.StartTime, r.Loc)
		allDays[d] = true
		if e.StartTime < from || e.StartTime >= to {
			continue
		}
		out.TotalSeconds += e.Duration
		pages[pageKey{e.BookHash, e.Page}] = true
		books[e.BookHash] = true
		days[d] = true
	}
	out.TotalPages, out.BooksRead, out.ReadingDays = int64(len(pages)), len(books), len(days)
	out.CurrentStreak, out.LongestStreak = streaks(allDays, time.Now().In(r.Loc))
	return out, nil
}

// streaks returns the run of consecutive days ending today (or yesterday, so an unfinished day does not
// break it) and the longest run ever.
func streaks(days map[string]bool, now time.Time) (current, longest int) {
	if len(days) == 0 {
		return 0, 0
	}
	sorted := make([]string, 0, len(days))
	for d := range days {
		sorted = append(sorted, d)
	}
	slices.Sort(sorted)
	run := 0
	var prev time.Time
	for i, d := range sorted {
		t, _ := time.Parse(time.DateOnly, d)
		if i > 0 && t.Sub(prev) == 24*time.Hour {
			run++
		} else {
			run = 1
		}
		longest = max(longest, run)
		prev = t
	}
	today := now.Format(time.DateOnly)
	cursor, _ := time.Parse(time.DateOnly, today)
	if !days[today] {
		cursor = cursor.AddDate(0, 0, -1)
	}
	for days[cursor.Format(time.DateOnly)] {
		current++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return current, longest
}

// Daily returns reading time per day with activity.
func (s *Service) Daily(ctx context.Context, userID string, r Range) (api.DailyStats, error) {
	evs, err := s.events(ctx, userID, r.From.Unix(), r.To.Unix())
	if err != nil {
		return api.DailyStats{}, err
	}
	type acc struct {
		seconds int64
		pages   map[pageKey]bool
	}
	byDay := map[string]*acc{}
	for _, e := range evs {
		d := day(e.StartTime, r.Loc)
		a := byDay[d]
		if a == nil {
			a = &acc{pages: map[pageKey]bool{}}
			byDay[d] = a
		}
		a.seconds += e.Duration
		a.pages[pageKey{e.BookHash, e.Page}] = true
	}
	out := api.DailyStats{Timezone: r.Loc.String(), Items: make([]api.DailyStat, 0, len(byDay))}
	for d, a := range byDay {
		t, _ := time.Parse(time.DateOnly, d)
		out.Items = append(out.Items, api.DailyStat{Date: openapi_types.Date{Time: t}, Seconds: a.seconds, Pages: int64(len(a.pages))})
	}
	slices.SortFunc(out.Items, func(a, b api.DailyStat) int { return a.Date.Compare(b.Date.Time) })
	return out, nil
}

// Books returns reading time per book, most read first.
func (s *Service) Books(ctx context.Context, userID string, r Range, limit int) (api.BookStats, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	evs, err := s.events(ctx, userID, r.From.Unix(), r.To.Unix())
	if err != nil {
		return api.BookStats{}, err
	}
	type acc struct {
		seconds, last int64
		pages         map[int64]bool
	}
	byBook := map[string]*acc{}
	for _, e := range evs {
		a := byBook[e.BookHash]
		if a == nil {
			a = &acc{pages: map[int64]bool{}}
			byBook[e.BookHash] = a
		}
		a.seconds += e.Duration
		a.pages[e.Page] = true
		a.last = max(a.last, e.StartTime+e.Duration)
	}
	hashes := make([]string, 0, len(byBook))
	for h := range byBook {
		hashes = append(hashes, h)
	}
	slices.SortFunc(hashes, func(a, b string) int {
		return cmp.Or(cmp.Compare(byBook[b].seconds, byBook[a].seconds), cmp.Compare(byBook[b].last, byBook[a].last), strings.Compare(a, b))
	})
	if len(hashes) > limit {
		hashes = hashes[:limit]
	}
	titles := map[string]db.StatBookTitlesRow{}
	if len(hashes) > 0 {
		rows, err := s.st.Q.StatBookTitles(ctx, db.StatBookTitlesParams{UserID: userID, Hashes: hashes})
		if err != nil {
			return api.BookStats{}, err
		}
		for _, row := range rows {
			titles[row.BookHash] = row
		}
	}
	out := api.BookStats{Items: make([]api.BookStat, 0, len(hashes))}
	for _, h := range hashes {
		a := byBook[h]
		t := titles[h]
		out.Items = append(out.Items, api.BookStat{BookHash: h, Title: t.Title, Authors: t.Authors, Seconds: a.seconds,
			Pages: int64(len(a.pages)), LastReadAt: time.Unix(a.last, 0).UTC()})
	}
	return out, nil
}
