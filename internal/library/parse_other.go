package library

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/nwaples/rardecode/v2"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
)

func init() {
	// pdfcpu would otherwise create ~/.config/pdfcpu on first use.
	pdfapi.DisableConfigDir()
}

func parsePDF(ctx context.Context, filePath string) (Metadata, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return Metadata{}, err
	}
	defer f.Close()
	info, err := pdfapi.PDFInfo(ctx, f, path.Base(filePath), nil, false, nil)
	if err != nil {
		return Metadata{}, err
	}
	m := Metadata{Title: strings.TrimSpace(info.Title), Description: strings.TrimSpace(info.Subject)}
	if a := strings.TrimSpace(info.Author); a != "" {
		m.Authors = splitAuthors(a)
	}
	for _, k := range info.Keywords {
		m.Tags = append(m.Tags, strings.Split(k, ",")...)
	}
	if info.PageCount > 0 {
		m.PageCount = new(info.PageCount)
	}
	return m, nil
}

// splitAuthors splits an author string on the separators Calibre and PDF producers use.
func splitAuthors(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '&' || r == '、' })
}

// comicInfo is the ComicRack metadata file found in CBZ/CBR archives.
type comicInfo struct {
	Title       string `xml:"Title"`
	Series      string `xml:"Series"`
	Number      string `xml:"Number"`
	Summary     string `xml:"Summary"`
	Writer      string `xml:"Writer"`
	Publisher   string `xml:"Publisher"`
	Year        int    `xml:"Year"`
	Month       int    `xml:"Month"`
	Day         int    `xml:"Day"`
	LanguageISO string `xml:"LanguageISO"`
	Genre       string `xml:"Genre"`
	Tags        string `xml:"Tags"`
	PageCount   int    `xml:"PageCount"`
	GTIN        string `xml:"GTIN"`
}

func (ci *comicInfo) metadata() Metadata {
	m := Metadata{
		Title: strings.TrimSpace(ci.Title), Series: strings.TrimSpace(ci.Series),
		Description: StripHTML(ci.Summary), Publisher: strings.TrimSpace(ci.Publisher),
		Language: strings.TrimSpace(ci.LanguageISO), ISBN: ci.GTIN,
	}
	if m.Title == "" && m.Series != "" {
		m.Title = strings.TrimSpace(m.Series + " " + ci.Number)
	}
	if ci.Writer != "" {
		m.Authors = strings.Split(ci.Writer, ",")
	}
	if n, err := strconv.ParseFloat(strings.TrimSpace(ci.Number), 64); err == nil && m.Series != "" {
		m.SeriesIndex = &n
	}
	if ci.Year > 0 {
		m.Published = strconv.Itoa(ci.Year)
		if ci.Month > 0 {
			m.Published += "-" + twoDigits(ci.Month)
			if ci.Day > 0 {
				m.Published += "-" + twoDigits(ci.Day)
			}
		}
	}
	m.Tags = append(strings.Split(ci.Genre, ","), strings.Split(ci.Tags, ",")...)
	if ci.PageCount > 0 {
		m.PageCount = new(ci.PageCount)
	}
	return m
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func parseComicInfo(r io.Reader) (Metadata, error) {
	var ci comicInfo
	d := xml.NewDecoder(io.LimitReader(r, 1<<20))
	d.Strict = false
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := d.Decode(&ci); err != nil {
		return Metadata{}, err
	}
	return ci.metadata(), nil
}

func parseCBZ(filePath string) (Metadata, error) {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return Metadata{}, err
	}
	defer zr.Close()
	var m Metadata
	var pages []*zip.File
	for _, f := range zr.File {
		if strings.EqualFold(path.Base(f.Name), "ComicInfo.xml") {
			if rc, err := f.Open(); err == nil {
				m, _ = parseComicInfo(rc)
				rc.Close()
			}
		} else if !f.FileInfo().IsDir() && imageExt(f.Name) != "" && !hiddenEntry(f.Name) {
			pages = append(pages, f)
		}
	}
	slices.SortFunc(pages, func(a, b *zip.File) int { return cmpNatural(a.Name, b.Name) })
	if len(pages) > 0 {
		if m.PageCount == nil {
			m.PageCount = new(len(pages))
		}
		if pages[0].UncompressedSize64 <= maxCoverBytes {
			if b, err := readZipFile(pages[0]); err == nil {
				m.Cover, m.CoverExt = b, SniffImage(b)
			}
		}
	}
	return m, nil
}

// parseCBR walks the archive twice: RAR is a sequential format, and the cover is the first page by name, not
// by position.
func parseCBR(filePath string) (Metadata, error) {
	var m Metadata
	var pages []string
	err := walkRAR(filePath, func(h *rardecode.FileHeader, r io.Reader) error {
		switch {
		case strings.EqualFold(path.Base(h.Name), "ComicInfo.xml"):
			m, _ = parseComicInfo(r)
		case !h.IsDir && imageExt(h.Name) != "" && !hiddenEntry(h.Name):
			pages = append(pages, h.Name)
		}
		return nil
	})
	if err != nil {
		return m, err
	}
	if len(pages) == 0 {
		return m, nil
	}
	slices.SortFunc(pages, cmpNatural)
	if m.PageCount == nil {
		m.PageCount = new(len(pages))
	}
	errDone := errors.New("done")
	err = walkRAR(filePath, func(h *rardecode.FileHeader, r io.Reader) error {
		if h.Name != pages[0] || h.UnPackedSize > maxCoverBytes {
			return nil
		}
		if b, err := io.ReadAll(io.LimitReader(r, maxCoverBytes)); err == nil {
			m.Cover, m.CoverExt = b, SniffImage(b)
		}
		return errDone
	})
	if err != nil && !errors.Is(err, errDone) {
		return m, err
	}
	return m, nil
}

func walkRAR(filePath string, fn func(*rardecode.FileHeader, io.Reader) error) error {
	rc, err := rardecode.OpenReader(filePath)
	if err != nil {
		return err
	}
	defer rc.Close()
	for {
		h, err := rc.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(h, rc); err != nil {
			return err
		}
	}
}

func hiddenEntry(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(base, ".") || strings.HasPrefix(name, "__MACOSX/")
}

func cmpNatural(a, b string) int {
	switch {
	case naturalLess(a, b):
		return -1
	case naturalLess(b, a):
		return 1
	}
	return 0
}

var (
	cjkTitleRe     = regexp.MustCompile(`《([^》]+)》(.*)`)
	labeledAuthor  = regexp.MustCompile(`作者\s*[：:\s]\s*(.+)$`)
	bracketedAuthr = regexp.MustCompile(`[\[(（【［]\s*([^\])）】］]+?)\s*[\])）】］]`)
)

// FilenameMetadata extracts a title and author from a file name, porting Readest's
// extractTxtFilenameMetadata (utils/txt.ts): `《书名》作者.txt`, `《书名》作者：X.txt`, `《书名》[X].txt`, and a
// labelled `作者：X` anywhere when there are no 《》. Without a match the base name is the title.
func FilenameMetadata(filename string) (title, author string) {
	base := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if ext := path.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.TrimSpace(base)
	m := cjkTitleRe.FindStringSubmatch(base)
	if m == nil {
		return base, parseLabeledAuthor(base)
	}
	return strings.TrimSpace(m[1]), parseAuthorFragment(strings.TrimSpace(m[2]))
}

func parseLabeledAuthor(text string) string {
	if m := labeledAuthor.FindStringSubmatch(text); m != nil {
		return stripWrappingPunctuation(m[1])
	}
	return ""
}

func parseAuthorFragment(text string) string {
	if text == "" {
		return ""
	}
	if a := parseLabeledAuthor(text); a != "" {
		return a
	}
	if m := bracketedAuthr.FindStringSubmatch(text); m != nil {
		return stripWrappingPunctuation(m[1])
	}
	return stripWrappingPunctuation(text)
}

// stripWrappingPunctuation trims leading and trailing Unicode punctuation, symbols and spaces
// (JavaScript /^[\p{P}\p{S}\s]+|[\p{P}\p{S}\s]+$/gu).
func stripWrappingPunctuation(s string) string {
	return strings.TrimFunc(strings.TrimSpace(s), func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r)
	})
}
