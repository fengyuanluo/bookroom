// Package scrape fills book metadata from external sources (docs/adr/0006-embedded-metadata-first.md):
// manual search-and-apply through the API, and a background worker that fills empty fields only on an exact
// match (ISBN for published books, title and author on xiaoshuohub for web novels).
package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"bookroom/internal/api"
	"bookroom/internal/library"
)

// Query is what a source is asked for.
type Query struct {
	Title  string
	Author string
	ISBN   string
}

// Candidate is one source record.
type Candidate struct {
	library.Metadata
	Provider   string
	ProviderID string
	CoverURL   string
	SourceURL  string
	Score      int
}

// Provider is one metadata source.
type Provider interface {
	ID() string
	Name() string
	WorkType() api.WorkType
	// Enabled reports whether the source is configured; reason explains why not.
	Enabled() (ok bool, reason string)
	Search(ctx context.Context, q Query) ([]Candidate, error)
	Get(ctx context.Context, id string) (Candidate, error)
}

// PauseError means the source cannot be used until RetryAt: its quota is spent, or it is unreachable.
type PauseError struct {
	Provider string
	RetryAt  time.Time
	Reason   string
}

func (e *PauseError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "quota exhausted"
	}
	return fmt.Sprintf("%s: %s until %s", e.Provider, reason, e.RetryAt.Format(time.RFC3339))
}

// unreachablePause is how long a source that does not answer is left alone, so a dead host does not stall
// the queue on connection timeouts.
const unreachablePause = 10 * time.Minute

// ErrNotFound means the source has no such record.
var ErrNotFound = errors.New("scrape: record not found")

// client is a rate-limited JSON/HTML fetcher shared by the providers.
type client struct {
	id      string
	http    *http.Client
	limiter *rate.Limiter
	ua      string
	// quota inspects every successful response and returns when the source should next be called; the
	// response itself is still used, later calls fail fast with a PauseError until then.
	quota func(*http.Response) time.Time

	mu    sync.Mutex
	until time.Time
}

func newClient(id string, perSecond float64, ua string) *client {
	if ua == "" {
		ua = "BookRoom/0.1 (+https://github.com/bookroom)"
	}
	return &client{id: id, http: &http.Client{Timeout: 20 * time.Second}, limiter: rate.NewLimiter(rate.Limit(perSecond), 1), ua: ua}
}

func (c *client) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	until := c.until
	c.mu.Unlock()
	if time.Now().Before(until) {
		return nil, &PauseError{Provider: c.id, RetryAt: until}
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.ua)
	}
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		until := time.Now().Add(unreachablePause)
		c.mu.Lock()
		c.until = until
		c.mu.Unlock()
		return nil, &PauseError{Provider: c.id, RetryAt: until, Reason: "unreachable (" + err.Error() + ")"}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return nil, &PauseError{Provider: c.id, RetryAt: time.Now().Add(retryAfter(resp.Header.Get("Retry-After"), time.Hour))}
	}
	if c.quota != nil && resp.StatusCode < 300 {
		if t := c.quota(resp); !t.IsZero() {
			c.mu.Lock()
			c.until = t
			c.mu.Unlock()
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d: %s", c.id, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (c *client) getJSON(ctx context.Context, url string, header http.Header, v any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "application/json")
	return c.decode(ctx, req, v)
}

func (c *client) decode(ctx context.Context, req *http.Request, v any) error {
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v); err != nil {
		return fmt.Errorf("%s: decode: %w", c.id, err)
	}
	return nil
}

func retryAfter(h string, fallback time.Duration) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return fallback
}
