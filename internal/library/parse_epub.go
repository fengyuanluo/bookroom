package library

import (
	"archive/zip"
	"html"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
)

type epubContainer struct {
	Rootfiles []struct {
		FullPath  string `xml:"full-path,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"rootfiles>rootfile"`
}

type opfMeta struct {
	Name     string `xml:"name,attr"`
	Content  string `xml:"content,attr"`
	Property string `xml:"property,attr"`
	Refines  string `xml:"refines,attr"`
	ID       string `xml:"id,attr"`
	Value    string `xml:",chardata"`
}

type opfText struct {
	ID     string `xml:"id,attr"`
	Role   string `xml:"role,attr"`
	Scheme string `xml:"scheme,attr"`
	Value  string `xml:",chardata"`
}

type opfPackage struct {
	Metadata struct {
		Titles      []opfText `xml:"title"`
		Creators    []opfText `xml:"creator"`
		Languages   []string  `xml:"language"`
		Publisher   string    `xml:"publisher"`
		Dates       []string  `xml:"date"`
		Description struct {
			Inner string `xml:",innerxml"`
		} `xml:"description"`
		Subjects    []string  `xml:"subject"`
		Identifiers []opfText `xml:"identifier"`
		Metas       []opfMeta `xml:"meta"`
	} `xml:"metadata"`
	Manifest []struct {
		ID         string `xml:"id,attr"`
		Href       string `xml:"href,attr"`
		MediaType  string `xml:"media-type,attr"`
		Properties string `xml:"properties,attr"`
	} `xml:"manifest>item"`
}

func parseEPUB(filePath string) (Metadata, error) {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return Metadata{}, err
	}
	defer zr.Close()
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}
	var c epubContainer
	if err := decodeXML(files["META-INF/container.xml"], &c); err != nil {
		return Metadata{}, err
	}
	opfPath := ""
	for _, rf := range c.Rootfiles {
		if rf.MediaType == "" || rf.MediaType == "application/oebps-package+xml" {
			opfPath = rf.FullPath
			break
		}
	}
	var pkg opfPackage
	if err := decodeXML(files[opfPath], &pkg); err != nil {
		return Metadata{}, err
	}
	m := opfMetadata(&pkg)

	if href := coverHref(&pkg); href != "" {
		if f := files[path.Join(path.Dir(opfPath), href)]; f != nil && f.UncompressedSize64 <= maxCoverBytes {
			if b, err := readZipFile(f); err == nil {
				if ext := sniffImage(b); ext != "" {
					m.Cover, m.CoverExt = b, ext
				}
			}
		}
	}
	return m, nil
}

func opfMetadata(pkg *opfPackage) Metadata {
	md := &pkg.Metadata
	var m Metadata
	// EPUB 3 marks the main title with a refining meta "title-type main"; otherwise the first title wins.
	titleType := map[string]string{}
	roles := map[string]string{}
	var series, seriesIndex string
	type collection struct{ id, name string }
	var collections []collection
	collectionType := map[string]string{}
	positions := map[string]string{}
	for _, mt := range md.Metas {
		ref := strings.TrimPrefix(mt.Refines, "#")
		switch {
		case mt.Name == "calibre:series":
			series = mt.Content
		case mt.Name == "calibre:series_index":
			seriesIndex = mt.Content
		case mt.Property == "title-type":
			titleType[ref] = strings.TrimSpace(mt.Value)
		case mt.Property == "role":
			roles[ref] = strings.TrimSpace(mt.Value)
		case mt.Property == "belongs-to-collection":
			collections = append(collections, collection{mt.ID, strings.TrimSpace(mt.Value)})
		case mt.Property == "collection-type":
			collectionType[ref] = strings.TrimSpace(mt.Value)
		case mt.Property == "group-position":
			positions[ref] = strings.TrimSpace(mt.Value)
		}
	}
	for _, t := range md.Titles {
		if titleType[t.ID] == "main" {
			m.Title = strings.TrimSpace(t.Value)
		}
	}
	if m.Title == "" && len(md.Titles) > 0 {
		m.Title = strings.TrimSpace(md.Titles[0].Value)
	}
	for _, c := range md.Creators {
		role := c.Role
		if role == "" {
			role = roles[c.ID]
		}
		if role == "" || role == "aut" {
			m.Authors = append(m.Authors, strings.TrimSpace(c.Value))
		}
	}
	if len(m.Authors) == 0 && len(md.Creators) > 0 {
		m.Authors = []string{strings.TrimSpace(md.Creators[0].Value)}
	}
	if len(md.Languages) > 0 {
		m.Language = strings.TrimSpace(md.Languages[0])
	}
	m.Publisher = strings.TrimSpace(md.Publisher)
	if len(md.Dates) > 0 {
		m.Published = strings.TrimSpace(md.Dates[0])
		if len(m.Published) > 10 && m.Published[4] == '-' {
			m.Published = m.Published[:10]
		}
	}
	// Descriptions arrive as escaped HTML, raw XHTML children or plain text.
	desc := md.Description.Inner
	if strings.Contains(desc, "&lt;") {
		desc = html.UnescapeString(desc)
	}
	m.Description = stripHTML(desc)
	m.Tags = md.Subjects
	for _, id := range md.Identifiers {
		v := strings.TrimSpace(id.Value)
		low := strings.ToLower(v)
		if strings.EqualFold(id.Scheme, "isbn") || strings.HasPrefix(low, "urn:isbn:") || strings.HasPrefix(low, "isbn:") {
			v = v[strings.LastIndex(v, ":")+1:]
		}
		if isbn, ok := NormalizeISBN(v); ok {
			m.ISBN = isbn
			break
		}
	}
	// EPUB 3 series: a belongs-to-collection typed "series", else the first untyped one.
	if series == "" {
		for _, want := range []string{"series", ""} {
			for _, c := range collections {
				if series == "" && c.name != "" && collectionType[c.id] == want {
					series, seriesIndex = c.name, positions[c.id]
				}
			}
		}
	}
	m.Series = strings.TrimSpace(series)
	if f, err := strconv.ParseFloat(strings.TrimSpace(seriesIndex), 64); err == nil && m.Series != "" {
		m.SeriesIndex = &f
	}
	return m
}

// coverHref finds the cover image: EPUB 3 `properties="cover-image"`, then EPUB 2 `<meta name="cover">`,
// then an image item whose id or href mentions "cover".
func coverHref(pkg *opfPackage) string {
	for _, it := range pkg.Manifest {
		if strings.Contains(" "+it.Properties+" ", " cover-image ") {
			return it.Href
		}
	}
	coverID := ""
	for _, mt := range pkg.Metadata.Metas {
		if mt.Name == "cover" {
			coverID = mt.Content
		}
	}
	for _, it := range pkg.Manifest {
		if coverID != "" && it.ID == coverID {
			return it.Href
		}
	}
	for _, it := range pkg.Manifest {
		if strings.HasPrefix(it.MediaType, "image/") &&
			(strings.Contains(strings.ToLower(it.ID), "cover") || strings.Contains(strings.ToLower(it.Href), "cover")) {
			return it.Href
		}
	}
	return ""
}

func decodeXML(f *zip.File, v any) error {
	if f == nil {
		return errors.New("missing archive entry")
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	d := xml.NewDecoder(io.LimitReader(rc, 8<<20))
	d.Strict = false
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return d.Decode(v)
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxCoverBytes))
}
