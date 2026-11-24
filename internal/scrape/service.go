package scrape

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/library"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// Service runs searches, applies candidates and drives the background gap filler.
type Service struct {
	st        *store.Store
	lib       *library.Service
	providers []Provider
	interval  time.Duration
	http      *http.Client
	wake      chan struct{}
	log       *slog.Logger
}

// New builds the service; providers are queried in the given order.
func New(st *store.Store, lib *library.Service, interval time.Duration, log *slog.Logger, providers ...Provider) *Service {
	return &Service{st: st, lib: lib, providers: providers, interval: interval, log: log,
		http: &http.Client{Timeout: 30 * time.Second}, wake: make(chan struct{}, 1)}
}

func (s *Service) provider(id string) Provider {
	for _, p := range s.providers {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

func (s *Service) pausedUntil(ctx context.Context, id string) time.Time {
	st, err := s.st.Q.GetProviderState(ctx, id)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(st.PausedUntil)
}

func (s *Service) pause(ctx context.Context, q *QuotaError) {
	if err := s.st.WQ.SetProviderPausedUntil(ctx, db.SetProviderPausedUntilParams{
		Provider: q.Provider, PausedUntil: q.RetryAt.UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		s.log.Warn("scrape: persist pause", "provider", q.Provider, "err", err)
	}
}

// usable reports whether a source can be queried now.
func (s *Service) usable(ctx context.Context, p Provider) bool {
	ok, _ := p.Enabled()
	return ok && time.Now().After(s.pausedUntil(ctx, p.ID()))
}

// Providers lists every source with its state.
func (s *Service) Providers(ctx context.Context) []api.MetadataProvider {
	out := make([]api.MetadataProvider, 0, len(s.providers))
	for _, p := range s.providers {
		ok, reason := p.Enabled()
		mp := api.MetadataProvider{Id: p.ID(), Name: p.Name(), WorkType: p.WorkType(), Enabled: ok}
		if reason != "" {
			mp.Reason = &reason
		}
		if until := s.pausedUntil(ctx, p.ID()); time.Now().Before(until) {
			mp.PausedUntil = &until
		}
		out = append(out, mp)
	}
	return out
}

// Search queries every usable source (or one) and returns scored candidates.
func (s *Service) Search(ctx context.Context, hash string, mq api.MetadataQuery) (api.CandidateList, error) {
	cur, _, err := s.lib.Metadata(ctx, hash)
	if err != nil {
		return api.CandidateList{}, err
	}
	q := Query{Title: cur.Title, ISBN: cur.ISBN}
	if len(cur.Authors) > 0 {
		q.Author = cur.Authors[0]
	}
	if mq.Title != nil || mq.Author != nil || mq.Isbn != nil {
		q = Query{Title: deref(mq.Title), Author: deref(mq.Author)}
		if mq.Isbn != nil && *mq.Isbn != "" {
			isbn, ok := library.NormalizeISBN(*mq.Isbn)
			if !ok {
				return api.CandidateList{}, apperr.BadRequest("invalid ISBN")
			}
			q.ISBN = isbn
		}
	}
	res := api.CandidateList{Candidates: []api.MetadataCandidate{}, Errors: []api.ProviderError{}}
	var cands []Candidate
	for _, p := range s.providers {
		if mq.Provider != nil && *mq.Provider != "" && p.ID() != *mq.Provider {
			continue
		}
		if !s.usable(ctx, p) {
			continue
		}
		pq := q
		if p.WorkType() == api.WorkTypeWebnovel {
			pq.ISBN = "" // web-novel sources have no ISBNs
		}
		if pq.ISBN == "" && pq.Title == "" {
			continue
		}
		found, err := p.Search(ctx, pq)
		if err != nil {
			res.Errors = append(res.Errors, providerError(ctx, s, p.ID(), err))
			continue
		}
		for i := range found {
			found[i].Score = score(&found[i], pq, p.ID())
		}
		cands = append(cands, found...)
	}
	slices.SortStableFunc(cands, func(a, b Candidate) int { return cmp.Compare(b.Score, a.Score) })
	for i := range cands {
		res.Candidates = append(res.Candidates, toAPI(&cands[i]))
	}
	return res, nil
}

func providerError(ctx context.Context, s *Service, id string, err error) api.ProviderError {
	pe := api.ProviderError{Provider: id, Message: err.Error()}
	var qe *QuotaError
	if errors.As(err, &qe) {
		s.pause(ctx, qe)
		pe.RetryAt = &qe.RetryAt
	}
	return pe
}

// Apply fetches a candidate, downloads its cover and writes the chosen fields, locked.
func (s *Service) Apply(ctx context.Context, userID, hash string, req api.ApplyMetadataRequest) (api.Book, error) {
	p := s.provider(req.Provider)
	if p == nil {
		return api.Book{}, apperr.BadRequest("unknown provider " + req.Provider)
	}
	if ok, reason := p.Enabled(); !ok {
		return api.Book{}, apperr.BadRequest(p.ID() + " is disabled: " + reason)
	}
	if _, _, err := s.lib.Metadata(ctx, hash); err != nil {
		return api.Book{}, err
	}
	c, err := p.Get(ctx, req.ProviderId)
	if errors.Is(err, ErrNotFound) {
		return api.Book{}, apperr.NotFound("the source has no record " + req.ProviderId)
	}
	if err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			s.pause(ctx, qe)
		}
		return api.Book{}, apperr.Upstream(p.Name()+" failed", err)
	}
	var fields []api.MetadataField
	if req.Fields != nil {
		fields = *req.Fields
	}
	wantCover := len(fields) == 0 || slices.Contains(fields, api.MetadataFieldCover)
	if wantCover && c.CoverURL != "" {
		c.Cover, c.CoverExt = s.fetchCover(ctx, c.CoverURL)
	}
	if c.WorkType == "" {
		c.WorkType = p.WorkType()
	}
	if _, err := s.lib.ApplyMetadata(ctx, hash, &c.Metadata, fields, true, p.ID()+":"+c.ProviderID); err != nil {
		return api.Book{}, err
	}
	return s.lib.Get(ctx, userID, hash)
}

// fetchCover downloads a cover image (≤10 MiB); failures just mean no cover.
func (s *Service) fetchCover(ctx context.Context, u string) ([]byte, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, ""
	}
	req.Header.Set("User-Agent", "BookRoom/0.1")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, ""
	}
	ext := library.SniffImage(b)
	// Open Library answers a 1x1 placeholder GIF for missing covers.
	if ext == "" || len(b) < 1024 {
		return nil, ""
	}
	return b, ext
}

// Enqueue schedules automatic gap filling now.
func (s *Service) Enqueue(ctx context.Context, hash string) (api.ScrapeJob, error) {
	if ok, err := s.lib.HasBook(ctx, hash); err != nil {
		return api.ScrapeJob{}, err
	} else if !ok {
		return api.ScrapeJob{}, apperr.NotFound("no such book")
	}
	now := time.Now()
	if err := s.st.WQ.EnqueueScrapeJob(ctx, db.EnqueueScrapeJobParams{Hash: hash, NextRunAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()}); err != nil {
		return api.ScrapeJob{}, err
	}
	s.Wake()
	return api.ScrapeJob{Hash: hash, Status: api.Pending, NextRunAt: &now}, nil
}

// Wake makes the worker look for due jobs now.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run processes due jobs until ctx ends.
func (s *Service) Run(ctx context.Context) {
	for {
		job, err := s.st.Q.NextDueScrapeJob(ctx, time.Now().UnixMilli())
		if err == nil {
			s.process(ctx, job)
		} else if !store.IsNotFound(err) && ctx.Err() == nil {
			s.log.Error("scrape: next job", "err", err)
		}
		wait := s.interval
		if err != nil {
			wait = time.Minute // idle: poll rarely, Enqueue wakes us
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(wait):
		}
	}
}

const maxAttempts = 5

func (s *Service) process(ctx context.Context, job db.ScrapeJob) {
	status, err := s.autoFill(ctx, job.Hash)
	now := time.Now()
	upd := db.UpdateScrapeJobParams{Hash: job.Hash, Status: status, Attempts: job.Attempts, NextRunAt: job.NextRunAt, UpdatedAt: now.UnixMilli()}
	var qe *QuotaError
	switch {
	case errors.As(err, &qe):
		upd.Status, upd.NextRunAt = string(api.Pending), qe.RetryAt.UnixMilli()
		upd.LastError = err.Error()
	case err != nil:
		upd.Attempts++
		upd.LastError = err.Error()
		if upd.Attempts >= maxAttempts {
			upd.Status = string(api.Failed)
		} else {
			upd.Status = string(api.Pending)
			upd.NextRunAt = now.Add(time.Duration(math.Pow(2, float64(upd.Attempts))) * 5 * time.Minute).UnixMilli()
		}
	}
	if err := s.st.WQ.UpdateScrapeJob(ctx, upd); err != nil && ctx.Err() == nil {
		s.log.Error("scrape: update job", "hash", job.Hash, "err", err)
	}
	if err != nil && qe == nil {
		s.log.Warn("scrape: auto fill", "hash", job.Hash, "err", err)
	}
}

// autoFill fills empty, unlocked fields from the first exact match: the same ISBN from a published-book
// source, or the same title and author from a web-novel source. Sources are never mixed.
func (s *Service) autoFill(ctx context.Context, hash string) (string, error) {
	cur, hasCover, err := s.lib.Metadata(ctx, hash)
	if ae, ok := apperr.As(err); ok && ae.Status == http.StatusNotFound {
		return string(api.Done), nil // deleted meanwhile
	}
	if err != nil {
		return "", err
	}
	var quota error
	for _, p := range s.providers {
		if !s.usable(ctx, p) {
			continue
		}
		var q Query
		webnovel := p.WorkType() == api.WorkTypeWebnovel
		switch {
		case !webnovel && cur.ISBN != "":
			q = Query{ISBN: cur.ISBN}
		case webnovel && cur.ISBN == "" && cur.WorkType != api.WorkTypePublished && cur.Title != "" && len(cur.Authors) > 0:
			q = Query{Title: cur.Title, Author: cur.Authors[0]}
		default:
			continue
		}
		cands, err := p.Search(ctx, q)
		if err != nil {
			var qe *QuotaError
			if errors.As(err, &qe) {
				s.pause(ctx, qe)
				quota = err
				continue
			}
			return "", err
		}
		for i := range cands {
			c := &cands[i]
			if !exactMatch(c, q, webnovel) {
				continue
			}
			full, err := p.Get(ctx, c.ProviderID)
			if err != nil {
				return "", err
			}
			if !exactMatch(&full, q, webnovel) && !webnovel {
				full.ISBN = q.ISBN // the edition lookup may omit the ISBN it was found by
			}
			if !hasCover && full.CoverURL != "" {
				full.Cover, full.CoverExt = s.fetchCover(ctx, full.CoverURL)
			}
			if full.WorkType == "" {
				full.WorkType = p.WorkType()
			}
			if _, err := s.lib.ApplyMetadata(ctx, hash, &full.Metadata, nil, false, p.ID()+":"+full.ProviderID); err != nil {
				return "", err
			}
			return string(api.Done), nil
		}
	}
	if quota != nil {
		return "", quota
	}
	return string(api.NoMatch), nil
}

func exactMatch(c *Candidate, q Query, webnovel bool) bool {
	if !webnovel {
		return q.ISBN != "" && c.ISBN == q.ISBN
	}
	if normalize(c.Title) != normalize(q.Title) {
		return false
	}
	for _, a := range c.Authors {
		if normalize(a) == normalize(q.Author) {
			return true
		}
	}
	return false
}

// normalize folds case, width-insensitive punctuation and spaces for comparisons.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 0xFF01 && r <= 0xFF5E { // full-width ASCII
			r -= 0xFEE0
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// similarity is the Dice coefficient of character bigrams (1 for equal normalized strings).
func similarity(a, b string) float64 {
	a, b = normalize(a), normalize(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	grams := func(s string) map[string]int {
		r := []rune(s)
		m := map[string]int{}
		if len(r) == 1 {
			m[s]++
		}
		for i := 0; i+1 < len(r); i++ {
			m[string(r[i:i+2])]++
		}
		return m
	}
	ga, gb := grams(a), grams(b)
	inter, total := 0, 0
	for g, n := range ga {
		inter += min(n, gb[g])
		total += n
	}
	for _, n := range gb {
		total += n
	}
	return 2 * float64(inter) / float64(total)
}

var providerBonus = map[string]float64{"googlebooks": 10, "openlibrary": 5, "hardcover": 5, "douban": 5, "xiaoshuohub": 15}

// score ranks a candidate 0–100: ISBN agreement dominates, then title and author similarity.
func score(c *Candidate, q Query, provider string) int {
	s := 30.0
	if q.ISBN != "" {
		if c.ISBN == q.ISBN {
			s += 40
		} else {
			s -= 40
		}
	}
	s += 25 * similarity(c.Title, q.Title)
	best := 0.0
	for _, a := range c.Authors {
		best = math.Max(best, similarity(a, q.Author))
	}
	s += 15 * best
	if c.CoverURL != "" {
		s += 10
	}
	s += providerBonus[provider]
	return int(math.Round(math.Max(0, math.Min(100, s))))
}

func toAPI(c *Candidate) api.MetadataCandidate {
	m := api.MetadataCandidate{
		Provider: c.Provider, ProviderId: c.ProviderID, WorkType: c.WorkType, Title: c.Title,
		Authors: nonNil(c.Authors), Tags: nonNil(c.Tags), Score: c.Score, SeriesIndex: c.SeriesIndex, Webnovel: c.Webnovel,
		Series: opt(c.Series), Description: opt(c.Description), Publisher: opt(c.Publisher), Published: opt(c.Published),
		Language: opt(c.Language), Isbn: opt(c.ISBN), CoverUrl: opt(c.CoverURL), SourceUrl: opt(c.SourceURL), PageCount: c.PageCount,
	}
	if m.WorkType == "" {
		m.WorkType = api.WorkTypeUnknown
	}
	return m
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
