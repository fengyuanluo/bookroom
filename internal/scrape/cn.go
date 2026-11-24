package scrape

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"bookroom/internal/api"
	"bookroom/internal/library"
)

var stripTags = library.StripHTML

// ── xiaoshuohub ──

// Xiaoshuohub queries the xiaoshuohub developer API (web novels only: no ISBN). Without app credentials it
// runs anonymously (200 calls/day per IP); with them 1000 calls per rolling 24 h.
type Xiaoshuohub struct {
	Base   string
	Key    string
	Secret string
	c      *client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// lowWater pauses the source before the server starts answering 429.
const xshLowWater = 20

func NewXiaoshuohub(base, key, secret string) *Xiaoshuohub {
	x := &Xiaoshuohub{Base: strings.TrimRight(base, "/"), Key: key, Secret: secret, c: newClient("xiaoshuohub", 2, "")}
	x.c.quota = func(r *http.Response) time.Time {
		if rem, err := strconv.Atoi(r.Header.Get("Ratelimit-Remaining")); err == nil && rem < xshLowWater {
			// The window rolls over 24 h; an hour later enough calls have aged out to continue slowly.
			return time.Now().Add(time.Hour)
		}
		return time.Time{}
	}
	return x
}

func (x *Xiaoshuohub) ID() string             { return "xiaoshuohub" }
func (x *Xiaoshuohub) Name() string           { return "小说汇 xiaoshuohub" }
func (x *Xiaoshuohub) WorkType() api.WorkType { return api.WorkTypeWebnovel }
func (x *Xiaoshuohub) Enabled() (bool, string) {
	if x.Base == "" {
		return false, "set BOOKROOM_XIAOSHUOHUB_URL"
	}
	return true, ""
}

func (x *Xiaoshuohub) header(ctx context.Context) (http.Header, error) {
	h := http.Header{}
	if x.Key == "" {
		return h, nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.token == "" || time.Now().After(x.expires) {
		req, err := http.NewRequest(http.MethodPost, x.Base+"/authorization/token", strings.NewReader("grant_type=client_credentials"))
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(x.Key, x.Secret)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var tok struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := x.c.decode(ctx, req, &tok); err != nil {
			return nil, err
		}
		x.token = tok.AccessToken
		// Refresh an hour early (tokens live 48 h).
		x.expires = time.Now().Add(time.Duration(max(tok.ExpiresIn-3600, 60)) * time.Second)
	}
	h.Set("Authorization", "Bearer "+x.token)
	return h, nil
}

type xshWork struct {
	ID            int64    `json:"id"`
	Title         string   `json:"title"`
	TitleHTML     string   `json:"title_html"`
	Author        string   `json:"author"`
	Intro         string   `json:"intro"`
	IntroHTML     string   `json:"intro_html"`
	Cover         string   `json:"cover"`
	Status        string   `json:"status"`
	WordCount     int64    `json:"word_count"`
	ChapterCount  int      `json:"chapter_count"`
	LatestChapter string   `json:"latest_chapter"`
	LatestUpdate  string   `json:"latest_update"`
	Tags          []string `json:"tags"`
	Category      any      `json:"category"`
	Genre         *struct {
		Name string `json:"name"`
	} `json:"genre"`
}

func (w *xshWork) candidate(base string) Candidate {
	c := Candidate{Provider: "xiaoshuohub", ProviderID: strconv.FormatInt(w.ID, 10),
		SourceURL: fmt.Sprintf("https://www.xiaoshuohub.com/book/%d/", w.ID), CoverURL: w.Cover}
	c.Title = w.Title
	if c.Title == "" {
		c.Title = stripTags(w.TitleHTML)
	}
	c.Description = w.Intro
	if c.Description == "" {
		c.Description = stripTags(w.IntroHTML)
	}
	if w.Author != "" {
		c.Authors = []string{w.Author}
	}
	c.WorkType, c.Language = api.WorkTypeWebnovel, "zh"
	c.Tags = append([]string{}, w.Tags...)
	if w.Genre != nil && w.Genre.Name != "" {
		c.Tags = append(c.Tags, w.Genre.Name)
	}
	switch cat := w.Category.(type) {
	case string:
		c.Tags = append(c.Tags, cat)
	case map[string]any:
		if names, ok := cat["names"].([]any); ok {
			for _, n := range names {
				if s, ok := n.(string); ok {
					c.Tags = append(c.Tags, s)
				}
			}
		}
	}
	info := &api.WebnovelInfo{SourceUrl: new(c.SourceURL)}
	if w.WordCount > 0 {
		info.WordCount = new(w.WordCount)
	}
	if w.ChapterCount > 0 {
		info.ChapterCount = new(w.ChapterCount)
	}
	if w.Status == "ongoing" || w.Status == "completed" {
		info.Status = new(api.WebnovelInfoStatus(w.Status))
	}
	if w.LatestChapter != "" {
		info.LatestChapter = new(w.LatestChapter)
	}
	if w.LatestUpdate != "" {
		info.LatestUpdate = new(w.LatestUpdate)
	}
	c.Webnovel = info
	return c
}

func (x *Xiaoshuohub) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if q.Title == "" {
		return nil, nil
	}
	h, err := x.header(ctx)
	if err != nil {
		return nil, err
	}
	params := url.Values{"q": {q.Title}}
	if q.Author != "" {
		params.Set("author", q.Author)
	}
	var res struct {
		Items []xshWork `json:"items"`
	}
	if err := x.c.getJSON(ctx, x.Base+"/v1/search?"+params.Encode(), h, &res); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(res.Items))
	for i := range res.Items {
		out = append(out, res.Items[i].candidate(x.Base))
	}
	return out, nil
}

func (x *Xiaoshuohub) Get(ctx context.Context, id string) (Candidate, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return Candidate{}, ErrNotFound
	}
	h, err := x.header(ctx)
	if err != nil {
		return Candidate{}, err
	}
	var w xshWork
	if err := x.c.getJSON(ctx, x.Base+"/v1/works/"+id, h, &w); err != nil {
		return Candidate{}, err
	}
	return w.candidate(x.Base), nil
}

// ── Douban ──

// Douban scrapes book.douban.com. It has no API anymore and its terms forbid scraping, so it is off unless
// the operator enables it (docs/adr/0006-embedded-metadata-first.md).
type Douban struct {
	On   bool
	Base string // https://book.douban.com
	c    *client
}

func NewDouban(enabled bool) *Douban {
	return &Douban{On: enabled, Base: "https://book.douban.com",
		c: newClient("douban", 1.0/3, "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")}
}

func (d *Douban) ID() string             { return "douban" }
func (d *Douban) Name() string           { return "豆瓣读书 (scraping, opt-in)" }
func (d *Douban) WorkType() api.WorkType { return api.WorkTypePublished }
func (d *Douban) Enabled() (bool, string) {
	if !d.On {
		return false, "scraping; enable with BOOKROOM_DOUBAN_ENABLED at your own risk"
	}
	return true, ""
}

func (d *Douban) Search(ctx context.Context, q Query) ([]Candidate, error) {
	term := q.ISBN
	if term == "" {
		term = strings.TrimSpace(q.Title + " " + q.Author)
	}
	var res []struct {
		Title      string `json:"title"`
		URL        string `json:"url"`
		Pic        string `json:"pic"`
		AuthorName string `json:"author_name"`
		Year       string `json:"year"`
		Type       string `json:"type"`
		ID         string `json:"id"`
	}
	if err := d.c.getJSON(ctx, d.Base+"/j/subject_suggest?q="+url.QueryEscape(term), http.Header{"Referer": {d.Base + "/"}}, &res); err != nil {
		return nil, err
	}
	var out []Candidate
	for _, r := range res {
		if r.Type != "" && r.Type != "b" {
			continue
		}
		c := Candidate{Provider: "douban", ProviderID: r.ID, SourceURL: r.URL, CoverURL: r.Pic}
		c.Title, c.Published, c.WorkType = r.Title, r.Year, api.WorkTypePublished
		if r.AuthorName != "" {
			c.Authors = []string{r.AuthorName}
		}
		out = append(out, c)
	}
	return out, nil
}

var doubanInfoLine = regexp.MustCompile(`^\s*([^:：]+)[:：]\s*(.*)$`)

func (d *Douban) Get(ctx context.Context, id string) (Candidate, error) {
	if _, err := strconv.Atoi(id); err != nil {
		return Candidate{}, ErrNotFound
	}
	req, err := http.NewRequest(http.MethodGet, d.Base+"/subject/"+id+"/", nil)
	if err != nil {
		return Candidate{}, err
	}
	req.Header.Set("Referer", d.Base+"/")
	resp, err := d.c.do(ctx, req)
	if err != nil {
		return Candidate{}, err
	}
	defer resp.Body.Close()
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return Candidate{}, err
	}
	return parseDouban(doc, id, d.Base), nil
}

func parseDouban(doc *goquery.Document, id, base string) Candidate {
	c := Candidate{Provider: "douban", ProviderID: id, SourceURL: base + "/subject/" + id + "/"}
	c.WorkType = api.WorkTypePublished
	c.Title = strings.TrimSpace(doc.Find(`span[property="v:itemreviewed"]`).First().Text())
	c.CoverURL, _ = doc.Find("#mainpic img").First().Attr("src")
	// #info is a flat list of "label: value" lines separated by <br>.
	info, _ := doc.Find("#info").Html()
	for _, line := range strings.Split(info, "<br/>") {
		m := doubanInfoLine.FindStringSubmatch(strings.TrimSpace(stripTags(line)))
		if m == nil {
			continue
		}
		key, val := strings.TrimSpace(m[1]), strings.Join(strings.Fields(m[2]), " ")
		switch key {
		case "作者":
			for _, a := range strings.Split(val, "/") {
				c.Authors = append(c.Authors, strings.TrimSpace(a))
			}
		case "出版社":
			c.Publisher = val
		case "出版年":
			c.Published = val
		case "页数":
			if n, err := strconv.Atoi(strings.TrimFunc(val, func(r rune) bool { return r < '0' || r > '9' })); err == nil {
				c.PageCount = new(n)
			}
		case "ISBN":
			if isbn, ok := library.NormalizeISBN(val); ok {
				c.ISBN = isbn
			}
		case "丛书":
			c.Series = val
		}
	}
	intro := doc.Find("#link-report .intro").Last()
	var paras []string
	intro.Find("p").Each(func(_ int, p *goquery.Selection) { paras = append(paras, strings.TrimSpace(p.Text())) })
	c.Description = strings.Join(paras, "\n")
	doc.Find("#db-tags-section a.tag").Each(func(_ int, a *goquery.Selection) { c.Tags = append(c.Tags, strings.TrimSpace(a.Text())) })
	return c
}
