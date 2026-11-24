// Package opds renders the library as an OPDS 1.2 Atom catalog (https://specs.opds.io/opds-1.2).
package opds

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/library"
)

// Media types of the two OPDS feed kinds.
const (
	Navigation  = "application/atom+xml;profile=opds-catalog;kind=navigation"
	Acquisition = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	OpenSearch  = "application/opensearchdescription+xml"
	pageSize    = 25
)

type link struct {
	Rel   string `xml:"rel,attr,omitempty"`
	Href  string `xml:"href,attr"`
	Type  string `xml:"type,attr,omitempty"`
	Title string `xml:"title,attr,omitempty"`
}

type person struct {
	Name string `xml:"name"`
}

type category struct {
	Term  string `xml:"term,attr"`
	Label string `xml:"label,attr"`
}

type text struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type entry struct {
	Title      string     `xml:"title"`
	ID         string     `xml:"id"`
	Updated    string     `xml:"updated"`
	Authors    []person   `xml:"author"`
	Language   string     `xml:"dc:language,omitempty"`
	Publisher  string     `xml:"dc:publisher,omitempty"`
	Issued     string     `xml:"dc:issued,omitempty"`
	Identifier string     `xml:"dc:identifier,omitempty"`
	Categories []category `xml:"category"`
	Summary    *text      `xml:"summary,omitempty"`
	Content    *text      `xml:"content,omitempty"`
	Links      []link     `xml:"link"`
}

type feed struct {
	XMLName      xml.Name `xml:"feed"`
	Xmlns        string   `xml:"xmlns,attr"`
	XmlnsDC      string   `xml:"xmlns:dc,attr"`
	XmlnsOS      string   `xml:"xmlns:opensearch,attr"`
	XmlnsOPDS    string   `xml:"xmlns:opds,attr"`
	ID           string   `xml:"id"`
	Title        string   `xml:"title"`
	Updated      string   `xml:"updated"`
	Author       person   `xml:"author"`
	TotalResults int      `xml:"opensearch:totalResults,omitempty"`
	ItemsPerPage int      `xml:"opensearch:itemsPerPage,omitempty"`
	StartIndex   int      `xml:"opensearch:startIndex,omitempty"`
	Links        []link   `xml:"link"`
	Entries      []entry  `xml:"entry"`
}

// Feed is a rendered document and its media type.
type Feed struct {
	ContentType string
	Body        []byte
}

// Catalog builds feeds for one user against a base URL such as https://host.
type Catalog struct {
	Lib *library.Service
}

func newFeed(base, id, title string) *feed {
	return &feed{
		Xmlns: "http://www.w3.org/2005/Atom", XmlnsDC: "http://purl.org/dc/terms/",
		XmlnsOS: "http://a9.com/-/spec/opensearch/1.1/", XmlnsOPDS: "http://opds-spec.org/2010/catalog",
		ID: "urn:bookroom:" + id, Title: title, Updated: stamp(time.Now()), Author: person{Name: "BookRoom"},
		Links: []link{
			{Rel: "start", Href: base + "/opds", Type: Navigation, Title: "BookRoom"},
			{Rel: "search", Href: base + "/opds/opensearch.xml", Type: OpenSearch, Title: "Search"},
		},
	}
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func render(f *feed, contentType string) (Feed, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", " ")
	if err := enc.Encode(f); err != nil {
		return Feed{}, err
	}
	return Feed{ContentType: contentType, Body: buf.Bytes()}, nil
}

func navEntry(id, title, href, kind, content string) entry {
	e := entry{Title: title, ID: "urn:bookroom:" + id, Updated: stamp(time.Now()),
		Links: []link{{Rel: "subsection", Href: href, Type: kind}}}
	if content != "" {
		e.Content = &text{Type: "text", Value: content}
	}
	return e
}

// Root is the start feed.
func (c *Catalog) Root(base string) (Feed, error) {
	f := newFeed(base, "root", "BookRoom")
	f.Links = append(f.Links, link{Rel: "self", Href: base + "/opds", Type: Navigation})
	f.Entries = []entry{
		navEntry("new", "Recently added", base+"/opds/books?sort=added", Acquisition, "Newest books first"),
		navEntry("title", "All books by title", base+"/opds/books?sort=title", Acquisition, ""),
		navEntry("reading", "Currently reading", base+"/opds/books?status=reading", Acquisition, ""),
		navEntry("shelves", "Shelves", base+"/opds/shelves", Navigation, ""),
		navEntry("tags", "Tags", base+"/opds/tags", Navigation, ""),
	}
	return render(f, Navigation)
}

// BookQuery selects the books of an acquisition feed.
type BookQuery struct {
	Sort, Status, Tag, Q string
	Shelf                *string
	Page                 int
}

// Books renders one page of books; self is the feed path with its query (without page).
func (c *Catalog) Books(ctx context.Context, base, userID, title, id, self string, q BookQuery) (Feed, error) {
	f, err := c.booksFeed(ctx, base, userID, title, id, self, q)
	if err != nil {
		return Feed{}, err
	}
	return render(f, Acquisition)
}

func (c *Catalog) booksFeed(ctx context.Context, base, userID, title, id, self string, q BookQuery) (*feed, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	lq := library.ListQuery{Q: q.Q, Status: q.Status, Tag: q.Tag, Shelf: q.Shelf, Sort: "added", Desc: true, Limit: pageSize}
	if q.Sort == "title" {
		lq.Sort, lq.Desc = "title", false
	}
	if q.Page > 1 {
		lq.Cursor = cursorFor((q.Page - 1) * pageSize)
	}
	page, err := c.Lib.List(ctx, userID, lq)
	if err != nil {
		return nil, err
	}
	f := newFeed(base, id, title)
	f.TotalResults, f.ItemsPerPage, f.StartIndex = page.Total, pageSize, (q.Page-1)*pageSize+1
	pageURL := func(n int) string { return withPage(base+self, n) }
	last := max(1, (page.Total+pageSize-1)/pageSize)
	f.Links = append(f.Links,
		link{Rel: "self", Href: pageURL(q.Page), Type: Acquisition},
		link{Rel: "first", Href: pageURL(1), Type: Acquisition},
		link{Rel: "last", Href: pageURL(last), Type: Acquisition},
		link{Rel: "up", Href: base + "/opds", Type: Navigation},
	)
	if q.Page > 1 {
		f.Links = append(f.Links, link{Rel: "previous", Href: pageURL(q.Page - 1), Type: Acquisition})
	}
	if q.Page < last {
		f.Links = append(f.Links, link{Rel: "next", Href: pageURL(q.Page + 1), Type: Acquisition})
	}
	for i := range page.Items {
		f.Entries = append(f.Entries, bookEntry(base, &page.Items[i]))
	}
	return f, nil
}

// cursorFor reuses the library's opaque cursor format for an offset.
func cursorFor(offset int) string {
	return library.EncodeCursor(offset)
}

func withPage(u string, page int) string {
	if page <= 1 {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "page=" + strconv.Itoa(page)
}

func bookEntry(base string, b *api.Book) entry {
	e := entry{
		Title: b.Title, ID: "urn:bookroom:book:" + b.Hash, Updated: stamp(b.UpdatedAt),
		Language: deref(b.Language), Publisher: deref(b.Publisher), Issued: deref(b.Published),
	}
	for _, a := range b.Authors {
		e.Authors = append(e.Authors, person{Name: a})
	}
	if b.Isbn != nil {
		e.Identifier = "urn:isbn:" + *b.Isbn
	}
	for _, t := range b.Tags {
		e.Categories = append(e.Categories, category{Term: t, Label: t})
	}
	if b.Series != nil {
		label := *b.Series
		if b.SeriesIndex != nil {
			label += fmt.Sprintf(" #%g", *b.SeriesIndex)
		}
		e.Content = &text{Type: "text", Value: label}
	}
	if b.Description != nil {
		e.Summary = &text{Type: "text", Value: truncate(*b.Description, 2000)}
	}
	file := fmt.Sprintf("%s/opds/books/%s/file", base, b.Hash)
	e.Links = append(e.Links, link{Rel: "http://opds-spec.org/acquisition", Href: file, Type: mediaType(b.Format), Title: string(b.Format)})
	if b.HasCover {
		cover := fmt.Sprintf("%s/opds/books/%s/cover", base, b.Hash)
		e.Links = append(e.Links,
			link{Rel: "http://opds-spec.org/image", Href: cover},
			link{Rel: "http://opds-spec.org/image/thumbnail", Href: cover})
	}
	return e
}

func mediaType(f api.BookFormat) string {
	mt := library.Format(f).MIME()
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return mt
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Shelf renders a shelf level: sub-shelves as navigation entries, then the books directly on it.
func (c *Catalog) Shelf(ctx context.Context, base, userID, path string, page int) (Feed, error) {
	path = library.NormalizeShelf(path)
	shelves, err := c.Lib.Shelves(ctx, userID)
	if err != nil {
		return Feed{}, err
	}
	depth, prefix := 0, ""
	if path != "" {
		depth, prefix = strings.Count(path, "/")+1, path+"/"
	}
	var subs []entry
	for _, s := range shelves {
		if s.Depth == depth && strings.HasPrefix(s.Path, prefix) {
			subs = append(subs, navEntry("shelf:"+s.Path, s.Name, base+"/opds/shelves?path="+url.QueryEscape(s.Path),
				Navigation, fmt.Sprintf("%d books", s.TotalCount)))
		}
	}
	if path == "" {
		f := newFeed(base, "shelves", "Shelves")
		f.Links = append(f.Links, link{Rel: "self", Href: base + "/opds/shelves", Type: Navigation},
			link{Rel: "up", Href: base + "/opds", Type: Navigation})
		f.Entries = subs
		return render(f, Navigation)
	}
	f, err := c.booksFeed(ctx, base, userID, path, "shelf:"+path, "/opds/shelves?path="+url.QueryEscape(path),
		BookQuery{Shelf: &path, Page: page})
	if err != nil {
		return Feed{}, err
	}
	if page <= 1 {
		f.Entries = append(subs, f.Entries...)
	}
	return render(f, Acquisition)
}

// Tags renders the tag list.
func (c *Catalog) Tags(ctx context.Context, base string) (Feed, error) {
	tags, err := c.Lib.Tags(ctx)
	if err != nil {
		return Feed{}, err
	}
	f := newFeed(base, "tags", "Tags")
	f.Links = append(f.Links, link{Rel: "self", Href: base + "/opds/tags", Type: Navigation},
		link{Rel: "up", Href: base + "/opds", Type: Navigation})
	for _, t := range tags {
		f.Entries = append(f.Entries, navEntry("tag:"+t.Name, t.Name, base+"/opds/books?tag="+url.QueryEscape(t.Name),
			Acquisition, fmt.Sprintf("%d books", t.Count)))
	}
	return render(f, Navigation)
}

// OpenSearchDescription is the search template document.
func OpenSearchDescription(base string) Feed {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
 <ShortName>BookRoom</ShortName>
 <Description>Search the BookRoom library</Description>
 <InputEncoding>UTF-8</InputEncoding>
 <OutputEncoding>UTF-8</OutputEncoding>
 <Url type="%s" template="%s/opds/search?q={searchTerms}"/>
</OpenSearchDescription>
`, Acquisition, xmlEscape(base))
	return Feed{ContentType: OpenSearch, Body: []byte(body)}
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
