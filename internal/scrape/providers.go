package scrape

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"bookroom/internal/api"
	"bookroom/internal/library"
)

// ── Google Books ──

// Google queries the Google Books API. A key is mandatory: the keyless daily quota is 0.
type Google struct {
	Key  string
	Base string // https://www.googleapis.com/books/v1
	c    *client
}

func NewGoogle(key string) *Google {
	return &Google{Key: key, Base: "https://www.googleapis.com/books/v1", c: newClient("googlebooks", 1, "")}
}

func (g *Google) ID() string             { return "googlebooks" }
func (g *Google) Name() string           { return "Google Books" }
func (g *Google) WorkType() api.WorkType { return api.WorkTypePublished }
func (g *Google) Enabled() (bool, string) {
	if g.Key == "" {
		return false, "set BOOKROOM_GOOGLE_BOOKS_KEY (the keyless quota is 0)"
	}
	return true, ""
}

type googleVolume struct {
	ID         string `json:"id"`
	VolumeInfo struct {
		Title               string   `json:"title"`
		Subtitle            string   `json:"subtitle"`
		Authors             []string `json:"authors"`
		Publisher           string   `json:"publisher"`
		PublishedDate       string   `json:"publishedDate"`
		Description         string   `json:"description"`
		PageCount           int      `json:"pageCount"`
		Language            string   `json:"language"`
		Categories          []string `json:"categories"`
		IndustryIdentifiers []struct {
			Type       string `json:"type"`
			Identifier string `json:"identifier"`
		} `json:"industryIdentifiers"`
		ImageLinks map[string]string `json:"imageLinks"`
		InfoLink   string            `json:"canonicalVolumeLink"`
	} `json:"volumeInfo"`
}

func (v *googleVolume) candidate() Candidate {
	vi := &v.VolumeInfo
	c := Candidate{Provider: "googlebooks", ProviderID: v.ID, SourceURL: vi.InfoLink}
	c.Title, c.Authors, c.Publisher, c.Published = vi.Title, vi.Authors, vi.Publisher, vi.PublishedDate
	c.Description, c.Language, c.Tags, c.WorkType = stripTags(vi.Description), vi.Language, vi.Categories, api.WorkTypePublished
	if vi.PageCount > 0 {
		c.PageCount = new(vi.PageCount)
	}
	for _, id := range vi.IndustryIdentifiers {
		if isbn, ok := library.NormalizeISBN(id.Identifier); ok && (c.ISBN == "" || id.Type == "ISBN_13") {
			c.ISBN = isbn
		}
	}
	for _, size := range []string{"extraLarge", "large", "medium", "small", "thumbnail", "smallThumbnail"} {
		if u := vi.ImageLinks[size]; u != "" {
			c.CoverURL = strings.Replace(u, "http://", "https://", 1)
			break
		}
	}
	return c
}

func (g *Google) Search(ctx context.Context, q Query) ([]Candidate, error) {
	query := ""
	if q.ISBN != "" {
		query = "isbn:" + q.ISBN
	} else {
		query = fmt.Sprintf("intitle:%q", q.Title)
		if q.Author != "" {
			query += fmt.Sprintf("+inauthor:%q", q.Author)
		}
	}
	var res struct {
		Items []googleVolume `json:"items"`
	}
	u := fmt.Sprintf("%s/volumes?q=%s&maxResults=5&key=%s", g.Base, url.QueryEscape(query), url.QueryEscape(g.Key))
	if err := g.c.getJSON(ctx, u, nil, &res); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(res.Items))
	for i := range res.Items {
		out = append(out, res.Items[i].candidate())
	}
	return out, nil
}

func (g *Google) Get(ctx context.Context, id string) (Candidate, error) {
	var v googleVolume
	if err := g.c.getJSON(ctx, fmt.Sprintf("%s/volumes/%s?key=%s", g.Base, url.PathEscape(id), url.QueryEscape(g.Key)), nil, &v); err != nil {
		return Candidate{}, err
	}
	return v.candidate(), nil
}

// ── Open Library ──

// OpenLibrary queries openlibrary.org (1 req/s, 3 req/s with a contact address in the User-Agent).
type OpenLibrary struct {
	On     bool
	Base   string // https://openlibrary.org
	Covers string // https://covers.openlibrary.org
	c      *client
}

func NewOpenLibrary(enabled bool, contact string) *OpenLibrary {
	ua, perSecond := "", 1.0
	if contact != "" {
		ua, perSecond = "BookRoom ("+contact+")", 3
	}
	return &OpenLibrary{On: enabled, Base: "https://openlibrary.org", Covers: "https://covers.openlibrary.org", c: newClient("openlibrary", perSecond, ua)}
}

func (o *OpenLibrary) ID() string             { return "openlibrary" }
func (o *OpenLibrary) Name() string           { return "Open Library" }
func (o *OpenLibrary) WorkType() api.WorkType { return api.WorkTypePublished }
func (o *OpenLibrary) Enabled() (bool, string) {
	if !o.On {
		return false, "disabled by BOOKROOM_OPENLIBRARY_ENABLED"
	}
	return true, ""
}

type olDoc struct {
	Key         string   `json:"key"`
	Title       string   `json:"title"`
	AuthorName  []string `json:"author_name"`
	ISBN        []string `json:"isbn"`
	CoverI      int      `json:"cover_i"`
	FirstPub    int      `json:"first_publish_year"`
	Publisher   []string `json:"publisher"`
	Language    []string `json:"language"`
	Subject     []string `json:"subject"`
	PagesMedian int      `json:"number_of_pages_median"`
}

const olFields = "key,title,author_name,isbn,cover_i,first_publish_year,publisher,language,subject,number_of_pages_median"

func (o *OpenLibrary) Search(ctx context.Context, q Query) ([]Candidate, error) {
	params := url.Values{"fields": {olFields}, "limit": {"5"}}
	if q.ISBN != "" {
		params.Set("isbn", q.ISBN)
	} else {
		params.Set("title", q.Title)
		if q.Author != "" {
			params.Set("author", q.Author)
		}
	}
	var res struct {
		Docs []olDoc `json:"docs"`
	}
	if err := o.c.getJSON(ctx, o.Base+"/search.json?"+params.Encode(), nil, &res); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(res.Docs))
	for _, d := range res.Docs {
		c := Candidate{Provider: "openlibrary", ProviderID: strings.TrimPrefix(d.Key, "/works/"), SourceURL: o.Base + d.Key}
		c.Title, c.Authors, c.WorkType = d.Title, d.AuthorName, api.WorkTypePublished
		if q.ISBN != "" {
			// Search returns works carrying dozens of ISBNs; pin the edition that was asked for.
			for _, i := range d.ISBN {
				if n, ok := library.NormalizeISBN(i); ok && n == q.ISBN {
					c.ISBN, c.ProviderID = n, "isbn:"+n
				}
			}
		}
		if len(d.Publisher) > 0 {
			c.Publisher = d.Publisher[0]
		}
		if d.FirstPub > 0 {
			c.Published = strconv.Itoa(d.FirstPub)
		}
		if len(d.Language) > 0 {
			c.Language = d.Language[0]
		}
		c.Tags = first(d.Subject, 10)
		if d.PagesMedian > 0 {
			c.PageCount = new(d.PagesMedian)
		}
		if d.CoverI > 0 {
			c.CoverURL = fmt.Sprintf("%s/b/id/%d-L.jpg", o.Covers, d.CoverI)
		}
		out = append(out, c)
	}
	return out, nil
}

type olText struct{ Value string }

func (t *olText) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		t.Value = s
		return nil
	}
	var o struct {
		Value string `json:"value"`
	}
	err := json.Unmarshal(b, &o)
	t.Value = o.Value
	return err
}

type olWork struct {
	Key         string   `json:"key"`
	Title       string   `json:"title"`
	Description olText   `json:"description"`
	Subjects    []string `json:"subjects"`
	Covers      []int    `json:"covers"`
	Authors     []struct {
		Author struct {
			Key string `json:"key"`
		} `json:"author"`
	} `json:"authors"`
}

// Get accepts a work id (OL123W) or "isbn:<isbn13>" for a specific edition.
func (o *OpenLibrary) Get(ctx context.Context, id string) (Candidate, error) {
	c := Candidate{Provider: "openlibrary", ProviderID: id}
	c.WorkType = api.WorkTypePublished
	workID := id
	if isbn, ok := strings.CutPrefix(id, "isbn:"); ok {
		var ed struct {
			Title       string   `json:"title"`
			Publishers  []string `json:"publishers"`
			PublishDate string   `json:"publish_date"`
			Pages       int      `json:"number_of_pages"`
			Covers      []int    `json:"covers"`
			Works       []struct {
				Key string `json:"key"`
			} `json:"works"`
		}
		if err := o.c.getJSON(ctx, o.Base+"/isbn/"+url.PathEscape(isbn)+".json", nil, &ed); err != nil {
			return c, err
		}
		c.Title, c.ISBN, c.Published = ed.Title, isbn, ed.PublishDate
		if len(ed.Publishers) > 0 {
			c.Publisher = ed.Publishers[0]
		}
		if ed.Pages > 0 {
			c.PageCount = new(ed.Pages)
		}
		if len(ed.Covers) > 0 && ed.Covers[0] > 0 {
			c.CoverURL = fmt.Sprintf("%s/b/id/%d-L.jpg", o.Covers, ed.Covers[0])
		}
		if len(ed.Works) == 0 {
			return c, nil
		}
		workID = strings.TrimPrefix(ed.Works[0].Key, "/works/")
	}
	var w olWork
	if err := o.c.getJSON(ctx, o.Base+"/works/"+url.PathEscape(workID)+".json", nil, &w); err != nil {
		return c, err
	}
	c.SourceURL = o.Base + "/works/" + workID
	if c.Title == "" {
		c.Title = w.Title
	}
	c.Description, c.Tags = strings.TrimSpace(w.Description.Value), first(w.Subjects, 10)
	if c.CoverURL == "" && len(w.Covers) > 0 && w.Covers[0] > 0 {
		c.CoverURL = fmt.Sprintf("%s/b/id/%d-L.jpg", o.Covers, w.Covers[0])
	}
	for i, a := range w.Authors {
		if i == 3 {
			break
		}
		var author struct {
			Name string `json:"name"`
		}
		if err := o.c.getJSON(ctx, o.Base+a.Author.Key+".json", nil, &author); err == nil && author.Name != "" {
			c.Authors = append(c.Authors, author.Name)
		}
	}
	return c, nil
}

func first(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ── Hardcover ──

// Hardcover queries the Hardcover GraphQL API with a personal access token.
type Hardcover struct {
	Token    string
	Endpoint string // https://api.hardcover.app/v1/graphql
	c        *client
}

func NewHardcover(token string) *Hardcover {
	return &Hardcover{Token: token, Endpoint: "https://api.hardcover.app/v1/graphql", c: newClient("hardcover", 1, "")}
}

func (h *Hardcover) ID() string             { return "hardcover" }
func (h *Hardcover) Name() string           { return "Hardcover" }
func (h *Hardcover) WorkType() api.WorkType { return api.WorkTypePublished }
func (h *Hardcover) Enabled() (bool, string) {
	if h.Token == "" {
		return false, "set BOOKROOM_HARDCOVER_TOKEN"
	}
	return true, ""
}

func (h *Hardcover) query(ctx context.Context, q string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
	req, err := http.NewRequest(http.MethodPost, h.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	auth := h.Token
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		auth = "Bearer " + auth
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := h.c.decode(ctx, req, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("hardcover: %s", res.Errors[0].Message)
	}
	return json.Unmarshal(res.Data, out)
}

type hcBook struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	Description   string  `json:"description"`
	ReleaseDate   string  `json:"release_date"`
	Pages         int     `json:"pages"`
	Slug          string  `json:"slug"`
	Image         *hcURL  `json:"image"`
	Contributions []hcCon `json:"contributions"`
}

type hcURL struct {
	URL string `json:"url"`
}

type hcCon struct {
	Author struct {
		Name string `json:"name"`
	} `json:"author"`
}

func (b *hcBook) candidate() Candidate {
	c := Candidate{Provider: "hardcover", ProviderID: strconv.Itoa(b.ID)}
	c.Title, c.Description, c.Published, c.WorkType = b.Title, b.Description, b.ReleaseDate, api.WorkTypePublished
	if b.Slug != "" {
		c.SourceURL = "https://hardcover.app/books/" + b.Slug
	}
	if b.Pages > 0 {
		c.PageCount = new(b.Pages)
	}
	if b.Image != nil {
		c.CoverURL = b.Image.URL
	}
	for _, a := range b.Contributions {
		c.Authors = append(c.Authors, a.Author.Name)
	}
	return c
}

const hcBookFields = `id title description release_date pages slug image { url } contributions { author { name } }`

func (h *Hardcover) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if q.ISBN != "" {
		var res struct {
			Editions []struct {
				ISBN13    string `json:"isbn_13"`
				Publisher *struct {
					Name string `json:"name"`
				} `json:"publisher"`
				Book hcBook `json:"book"`
			} `json:"editions"`
		}
		err := h.query(ctx, `query ($isbn: String!) { editions(where: {isbn_13: {_eq: $isbn}}, limit: 5) { isbn_13 publisher { name } book { `+hcBookFields+` } } }`,
			map[string]any{"isbn": q.ISBN}, &res)
		if err != nil {
			return nil, err
		}
		out := make([]Candidate, 0, len(res.Editions))
		for _, e := range res.Editions {
			c := e.Book.candidate()
			c.ISBN = e.ISBN13
			if e.Publisher != nil {
				c.Publisher = e.Publisher.Name
			}
			out = append(out, c)
		}
		return out, nil
	}
	var res struct {
		Search struct {
			Results struct {
				Hits []struct {
					Document struct {
						ID          json.Number `json:"id"`
						Title       string      `json:"title"`
						AuthorNames []string    `json:"author_names"`
						ISBNs       []string    `json:"isbns"`
						ReleaseYear int         `json:"release_year"`
						Pages       int         `json:"pages"`
						Slug        string      `json:"slug"`
						Image       *hcURL      `json:"image"`
					} `json:"document"`
				} `json:"hits"`
			} `json:"results"`
		} `json:"search"`
	}
	term := strings.TrimSpace(q.Title + " " + q.Author)
	err := h.query(ctx, `query ($q: String!) { search(query: $q, query_type: "Book", per_page: 5, page: 1) { results } }`,
		map[string]any{"q": term}, &res)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, hit := range res.Search.Results.Hits {
		d := hit.Document
		c := Candidate{Provider: "hardcover", ProviderID: d.ID.String()}
		c.Title, c.Authors, c.WorkType = d.Title, d.AuthorNames, api.WorkTypePublished
		if d.ReleaseYear > 0 {
			c.Published = strconv.Itoa(d.ReleaseYear)
		}
		if d.Pages > 0 {
			c.PageCount = new(d.Pages)
		}
		if d.Image != nil {
			c.CoverURL = d.Image.URL
		}
		if d.Slug != "" {
			c.SourceURL = "https://hardcover.app/books/" + d.Slug
		}
		for _, i := range d.ISBNs {
			if n, ok := library.NormalizeISBN(i); ok {
				c.ISBN = n
				break
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (h *Hardcover) Get(ctx context.Context, id string) (Candidate, error) {
	n, err := strconv.Atoi(id)
	if err != nil {
		return Candidate{}, ErrNotFound
	}
	var res struct {
		Book *hcBook `json:"books_by_pk"`
	}
	if err := h.query(ctx, `query ($id: Int!) { books_by_pk(id: $id) { `+hcBookFields+` } }`, map[string]any{"id": n}, &res); err != nil {
		return Candidate{}, err
	}
	if res.Book == nil {
		return Candidate{}, ErrNotFound
	}
	return res.Book.candidate(), nil
}
